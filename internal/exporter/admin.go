package exporter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Admin commands of §8.3. They never contact the service. requeue and
// delete-raw take the state dir lock, so they refuse to run while the daemon
// runs; status only reads files that are replaced atomically and needs no
// lock.

// ErrUsage marks a wrong argument (exit 2).
var ErrUsage = errors.New("usage")

// statusRow is one record in `status`: record_id, state and reason only,
// never meeting_id or file names (§8.3).
type statusRow struct {
	RecordID string  `json:"record_id"`
	State    string  `json:"state"`
	Reason   *string `json:"reason"`
}

// Status writes the state of every record (or of one, with all its
// state.json fields) as JSON to w.
func (d *Daemon) Status(w io.Writer, recordID string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if recordID != "" {
		if !ValidRecordID(recordID) {
			return fmt.Errorf("%w: invalid record_id", ErrUsage)
		}
		rec, err := d.store.Load(recordID)
		if err != nil {
			return err
		}
		if rec == nil {
			if !fileExists(d.store.recordDir(recordID)) {
				return fmt.Errorf("record %s not found", recordID)
			}
			return enc.Encode(statusRow{RecordID: recordID, State: StateSpooled})
		}
		return enc.Encode(rec)
	}

	rows := []statusRow{}
	ids, err := d.store.RecordIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		rec, err := d.store.Load(id)
		switch {
		case err != nil:
			return err
		case rec == nil:
			rows = append(rows, statusRow{RecordID: id, State: StateSpooled})
		default:
			rows = append(rows, statusRow{RecordID: id, State: rec.State, Reason: rec.Reason})
		}
	}
	// Spool entries not claimed yet.
	entries, _ := os.ReadDir(d.cfg.SpoolDir)
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && ValidRecordID(id) {
			rows = append(rows, statusRow{RecordID: id, State: StateSpooled})
		}
	}
	return enc.Encode(map[string]any{"records": rows})
}

// Requeue moves a failed record back to spooled: the manifest and the state
// are removed and the next loop rebuilds it (§5.1). spool.json is not
// needed for that, so a record whose claim was interrupted can be requeued.
func (d *Daemon) Requeue(recordID string) error {
	if !ValidRecordID(recordID) {
		return fmt.Errorf("%w: invalid record_id", ErrUsage)
	}
	lock, err := Lock(d.cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	rec, err := d.store.Load(recordID)
	if err != nil {
		return err
	}
	if rec == nil || rec.State != StateFailed {
		return fmt.Errorf("record %s is not failed; only failed records can be requeued", recordID)
	}
	dir := d.store.recordDir(recordID)
	// Manifest first: a state.json without manifest stays failed and can be
	// requeued again; a manifest without state.json would be reused.
	if err := os.Remove(filepath.Join(dir, manifestFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(dir, stateFile)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	d.log.Info("record requeued", "record_id", recordID, "from", StateFailed, "reason", rec.ReasonString())
	return nil
}

// DeleteRaw is the manual `delete-raw`: every check of §7 (verified ack and
// 24 h grace, or failure expiry; BBB completion markers; events.xml
// unchanged), whatever the configured policy. Reason manual.
func (d *Daemon) DeleteRaw(recordID string) error {
	if !ValidRecordID(recordID) {
		return fmt.Errorf("%w: invalid record_id", ErrUsage)
	}
	lock, err := Lock(d.cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	rec, err := d.store.Load(recordID)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("record %s has no state", recordID)
	}
	if _, why := d.deletion(rec, "", d.now()); why != "" {
		return fmt.Errorf("record %s: raw dir not deleted: %s", recordID, why)
	}
	if err := d.deleteRaw(rec, ReasonManual); err != nil {
		return err
	}
	if rec.State != StateDeleted {
		return fmt.Errorf("record %s: raw dir not deleted: %s/%s", recordID, rec.State, rec.ReasonString())
	}
	return nil
}
