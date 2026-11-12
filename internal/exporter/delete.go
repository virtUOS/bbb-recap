package exporter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// deletion says whether §7 allows deleting raw/<id>/ now, and with which
// reason. It checks the state, the grace or the failure expiry, and BBB's
// completion markers (condition 2). The events.xml check is done by
// deleteRaw. policy is the configured policy, or "" for a manual delete-raw,
// which ignores the policy but applies every other check.
func (d *Daemon) deletion(rec *Record, policy string, now time.Time) (reason, why string) {
	if rec.PackageID == "" {
		return "", "no manifest to check events.xml against"
	}
	switch rec.State {
	case StateAcked:
		if policy == PolicyNever {
			return "", "policy never"
		}
		if rec.JobID == "" {
			return "", "no verified ack"
		}
		at := rec.enteredAt(StateAcked)
		if at.IsZero() || now.Sub(at) < deleteGrace {
			return "", "within the 24 h grace after the ack"
		}
		reason = ReasonAfterAck
	case StateReady, StateDelivered, StateFailed:
		if rec.State == StateFailed && (rec.ReasonString() == ReasonSourceChanged || rec.ReasonString() == ReasonSourceMissing) {
			// The raw dir no longer matches the manifest and never will
			// again: an admin acts, the loop does not retry every 10 s.
			return "", "raw dir no longer matches the manifest"
		}
		if d.cfg.FailureDays <= 0 {
			return "", "failure expiry is off"
		}
		if rec.FirstAttemptAt.IsZero() || now.Sub(rec.FirstAttemptAt) < time.Duration(d.cfg.FailureDays)*24*time.Hour {
			return "", "failure expiry not reached"
		}
		reason = ReasonFailureExpiry
	default:
		return "", "state " + rec.State
	}
	if !d.markersDone(rec.RecordID) {
		return "", "BBB has not published every listed format"
	}
	return reason, ""
}

// markersDone reports whether every format in RECAP_EXPORTER_RECORDING_FORMATS
// has <STATUS_DIR>/published/<record_id>-<format>.done (§7 condition 2,
// mirroring cron.daily/bigbluebutton:94-123 @ 7d27e7e).
func (d *Daemon) markersDone(id string) bool {
	if d.cfg.StatusDir == "" || len(d.cfg.Formats) == 0 {
		return false
	}
	for _, f := range d.cfg.Formats {
		if _, err := os.Stat(filepath.Join(d.cfg.StatusDir, "published", id+"-"+f+".done")); err != nil {
			return false
		}
	}
	return true
}

// deleteRaw removes raw/<id>/ and moves the record to deleted (§7). It
// first checks that events.xml still matches the manifest; if not, the
// record fails with source_changed (or source_missing) and nothing is
// deleted. It never follows a symlink and never touches anything outside
// raw/<id>/.
func (d *Daemon) deleteRaw(rec *Record, reason string) error {
	if !ValidRecordID(rec.RecordID) {
		return errors.New("invalid record_id")
	}
	mb, err := d.store.Manifest(rec.RecordID)
	if err != nil {
		return err
	}
	if mb == nil {
		return errors.New("no manifest, so events.xml cannot be checked")
	}
	m, err := parseManifest(mb)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.cfg.RawDir, rec.RecordID)

	fail := func(r string) error {
		rec.setState(StateFailed, r, d.now())
		if err := d.store.Save(rec); err != nil {
			return err
		}
		d.log.Error("raw dir not deleted: it no longer matches the manifest", "record_id", rec.RecordID,
			"package_id", rec.PackageID, "state", StateFailed, "reason", r)
		return nil
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(ReasonSourceMissing)
	case err != nil:
		return err
	case !fi.IsDir(): // a symlink or anything else: never follow it
		return fail(ReasonSourceChanged)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if r := checkEvents(root, m); r != "" {
		return fail(r)
	}

	// Remove events.xml last, so an interrupted deletion is resumed by the
	// next loop (the events.xml check still passes).
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "events.xml" {
			continue
		}
		if err := root.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	if err := root.Remove("events.xml"); err != nil {
		return err
	}
	_ = root.Close()
	if err := os.Remove(dir); err != nil {
		return err
	}

	rec.setState(StateDeleted, reason, d.now())
	if err := d.store.Save(rec); err != nil {
		return err
	}
	d.metrics.deleted.WithLabelValues(reason).Inc()
	d.log.Info("raw dir deleted", "record_id", rec.RecordID, "package_id", rec.PackageID,
		"state", StateDeleted, "reason", reason, "size", m.Totals.Size)
	return nil
}

// checkEvents compares events.xml below root with the manifest entry.
func checkEvents(root *os.Root, m *manifest.Manifest) string {
	var want *manifest.File
	for i := range m.Files {
		if m.Files[i].Role == manifest.RoleEvents {
			want = &m.Files[i]
		}
	}
	if want == nil {
		return ReasonSourceChanged
	}
	fi, err := root.Lstat("events.xml")
	if errors.Is(err, fs.ErrNotExist) {
		return ReasonSourceMissing
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != want.Size {
		return ReasonSourceChanged
	}
	f, err := root.Open("events.xml")
	if err != nil {
		return ReasonSourceMissing
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return ReasonSourceChanged
	}
	return ""
}

// prune removes acked and deleted records 30 days after they entered that
// state (§5.4).
// It reports whether the record is gone.
func (d *Daemon) prune(rec *Record, now time.Time) bool {
	if rec.State != StateAcked && rec.State != StateDeleted {
		return false
	}
	at := rec.enteredAt(rec.State)
	if at.IsZero() || now.Sub(at) < stateRetention {
		return false
	}
	if err := d.store.removeRecord(rec.RecordID); err != nil {
		d.log.Error("state record cannot be pruned", "record_id", rec.RecordID)
		return false
	}
	d.log.Info("state record pruned", "record_id", rec.RecordID, "state", rec.State)
	return true
}
