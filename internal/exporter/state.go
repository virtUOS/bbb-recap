package exporter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Exporter states and reasons (docs/reference/README.md "States and enums").
const (
	StateReady     = "ready"
	StateDelivered = "delivered"
	StateAcked     = "acked"
	StateDeleted   = "deleted"
	StateFailed    = "failed"
	// StateSpooled is shown by `status` for a claimed record that has no
	// state.json yet; it is never persisted (§5).
	StateSpooled = "spooled"
)

// States lists the persisted states, for metrics.
var States = []string{StateReady, StateDelivered, StateAcked, StateDeleted, StateFailed}

// Failure and deletion reasons.
const (
	ReasonNoEvents      = "no_events"
	ReasonNoMicrophone  = "no_microphone_tracks"
	ReasonAudioBridge   = "unsupported_audio_bridge"
	ReasonLimitExceeded = "limit_exceeded"
	ReasonSourceChanged = "source_changed"
	ReasonSourceMissing = "source_missing"
	ReasonRejected      = "rejected"
	ReasonConflict      = "conflict"
	ReasonUnauthorized  = "unauthorized"
	ReasonAfterAck      = "after_ack"
	ReasonFailureExpiry = "failure_expiry"
	ReasonManual        = "manual"
	stateFile           = "state.json"
	manifestFile        = "manifest.json"
	spoolFile           = "spool.json"
	lockFile            = ".lock"
	stateRetention      = 30 * 24 * time.Hour // §5.4
	deleteGrace         = 24 * time.Hour      // §7 condition 3
)

// Record is <STATE_DIR>/<record_id>/state.json (§5.3). Times are UTC.
type Record struct {
	RecordID       string     `json:"record_id"`
	State          string     `json:"state"`
	Reason         *string    `json:"reason"`
	PackageID      string     `json:"package_id,omitempty"`
	JobID          string     `json:"job_id,omitempty"`
	Attempts       int        `json:"attempts"`
	FirstAttemptAt time.Time  `json:"first_attempt_at,omitzero"`
	LastAttemptAt  time.Time  `json:"last_attempt_at,omitzero"`
	NextAttemptAt  time.Time  `json:"next_attempt_at,omitzero"`
	LastError      *LastError `json:"last_error"`
	History        []Entry    `json:"history"`
}

// LastError is the last ingest error: only the problem type and HTTP status.
type LastError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
}

// Entry is one state change.
type Entry struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

// ReasonString returns the reason or "".
func (r *Record) ReasonString() string {
	if r.Reason == nil {
		return ""
	}
	return *r.Reason
}

// setState records a transition in State, Reason and History. reason is ""
// for states without one.
func (r *Record) setState(state, reason string, now time.Time) {
	r.State = state
	r.Reason = nil
	if reason != "" {
		r.Reason = &reason
	}
	r.History = append(r.History, Entry{State: state, At: now.UTC()})
}

// enteredAt returns when the record last entered state, or the zero time.
func (r *Record) enteredAt(state string) time.Time {
	for i := len(r.History) - 1; i >= 0; i-- {
		if r.History[i].State == state {
			return r.History[i].At
		}
	}
	return time.Time{}
}

// Store is the state dir. Each record is a directory <record_id>/ with
// spool.json, manifest.json and state.json.
type Store struct {
	Dir string
}

func (s *Store) recordDir(id string) string { return filepath.Join(s.Dir, id) }

// RecordIDs returns the record dirs in the state dir, sorted. Entries whose
// names are not record IDs (the lock file, temp files) are skipped.
func (s *Store) RecordIDs() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidRecordID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Load returns the state record of id, or nil without error if the record
// has no state.json yet (claimed, not built).
func (s *Store) Load(id string) (*Record, error) {
	b, err := os.ReadFile(filepath.Join(s.recordDir(id), stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("state.json of %s: %w", id, err)
	}
	if r.RecordID != id {
		return nil, fmt.Errorf("state.json of %s names another record", id)
	}
	return &r, nil
}

// Save writes state.json atomically and durably (§5.1: before the next side
// effect).
func (s *Store) Save(r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.recordDir(r.RecordID), stateFile), append(b, '\n'))
}

// Manifest returns the manifest bytes of id, or nil if there are none.
func (s *Store) Manifest(id string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.recordDir(id), manifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeFileAtomic writes a temp file in the same dir, fsyncs it, renames it
// over path and fsyncs the dir.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after the rename
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o640); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// ErrLocked means another process holds the state dir lock.
var ErrLocked = errors.New("another recap-exporter holds the state dir lock (stop the daemon first)")

// Lock takes the exclusive flock on <STATE_DIR>/.lock without waiting (§4.1:
// one daemon per server). Closing the file releases it.
func Lock(stateDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(stateDir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

// removeRecord removes the record dir <STATE_DIR>/<id>/ (only that dir).
func (s *Store) removeRecord(id string) error {
	if !ValidRecordID(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid record_id")
	}
	if err := os.RemoveAll(s.recordDir(id)); err != nil {
		return err
	}
	return syncDir(s.Dir)
}
