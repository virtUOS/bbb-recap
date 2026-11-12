package validate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Deps are what the validate handler needs.
type Deps struct {
	Pool            *pgxpool.Pool
	Blobs           blob.Store
	Log             *slog.Logger
	MetaPrefix      string // RECAP_META_PREFIX
	Configured      Configured
	FrontendServers []config.FrontendServer // RECAP_FRONTEND_SERVERS; empty: all servers
}

// Decision is the outcome of rows 1–9a of metadata-interface.md §3.
// It holds only comparable codes, so the commit can check that it still
// holds.
type Decision struct {
	Cancel         bool   // row 1: a tombstone
	Reason, Detail string // a hold (rows 2–9a); empty if none
	RegistrationID int64  // the matched registration on ok
	MeetingType    string // on ok
	SinkProfile    string // on ok
}

// Registration is the registrations row of a record_id.
type Registration struct {
	ID         int64
	FrontendID string
	MeetingID  string
	Status     string // registered or cancelled (a tombstone)
}

// Decide applies rows 1–9a of metadata-interface.md §3 in order. reg is the
// registration of the package's record_id, or nil. Row 10 is checked at
// deliver.
func Decide(reg *Registration, meta map[string]string, prefix string, c Configured,
	servers []config.FrontendServer, j *Job) Decision {
	if reg != nil && reg.Status == "cancelled" { // row 1
		return Decision{Cancel: true}
	}
	if h := CheckMetadata(meta, prefix, c); h != nil { // rows 2–6
		return Decision{Reason: h.Reason, Detail: h.Detail}
	}
	switch {
	case reg == nil || reg.Status != "registered": // row 7
		return Decision{Reason: HeldNoRegistration, Detail: DetailNoRegistration}
	case !serverAllowed(servers, reg.FrontendID, j.BBBServerID): // row 8
		return Decision{Reason: HeldNoRegistration, Detail: DetailServerNotAllowed}
	case reg.MeetingID != j.MeetingID: // row 9
		return Decision{Reason: HeldNoRegistration, Detail: DetailMeetingIDMismatch}
	}
	typ, sink := meta[prefix+"-type"], meta[prefix+"-sink"]
	switch { // row 9a
	case !allowed(c.FrontendTypes, reg.FrontendID, typ):
		return Decision{Reason: HeldUnknownType, Detail: DetailTypeNotAllowed}
	case !allowed(c.FrontendSinks, reg.FrontendID, sink):
		return Decision{Reason: HeldUnknownSink, Detail: DetailSinkNotAllowed}
	}
	return Decision{RegistrationID: reg.ID, MeetingType: typ, SinkProfile: sink}
}

// serverAllowed is row 8: the front end may claim packages of the BBB
// server. With no RECAP_FRONTEND_SERVERS every front end may claim
// packages of every server.
func serverAllowed(servers []config.FrontendServer, frontendID, bbbServerID string) bool {
	return len(servers) == 0 ||
		slices.Contains(servers, config.FrontendServer{FrontendID: frontendID, BBBServerID: bbbServerID})
}

// LookupRegistration reads the registration of recordID (exact, case
// sensitive), or nil. It takes no lock (see the commit in run).
func LookupRegistration(ctx context.Context, q Querier, recordID string) (*Registration, error) {
	var r Registration
	err := q.QueryRow(ctx, `
		SELECT registration_id, frontend_id, meeting_id, status FROM registrations WHERE record_id = $1`,
		recordID).Scan(&r.ID, &r.FrontendID, &r.MeetingID, &r.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registration: %w", err)
	}
	return &r, nil
}

// ErrDecisionChanged rolls back a completion whose decision no longer holds
// inside the transaction (a registration changed meanwhile). The step is
// then retried and decides again.
var ErrDecisionChanged = errors.New("validate: registration changed during the step")

// Handler returns the validate step handler.
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		r, err := d.run(ctx, run.JobID)
		if err != nil {
			d.Log.Warn("validate: step error", "job_id", run.JobID, "err", err)
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "validate"}
		}
		d.Log.Debug("validate: done", "job_id", run.JobID, "outcome", string(r.Outcome),
			"reason", r.Reason, "detail", r.Detail, "cancel", r.Cancel)
		return r
	}
}

func (d Deps) run(ctx context.Context, jobID string) (queue.Result, error) {
	j, err := LoadJob(ctx, d.Pool, jobID)
	if err != nil {
		return queue.Result{}, err
	}
	m, err := ReadManifest(ctx, d.Blobs, j)
	if err != nil {
		return queue.Result{}, err
	}
	gaps, v, err := Rules(j.FS(ctx, d.Blobs), Package{
		RecordID:    j.RecordID,
		MeetingID:   j.MeetingID,
		Metadata:    m.Metadata,
		MetaPrefix:  m.MetaPrefix,
		Microphones: j.Microphones(),
	})
	if err != nil {
		return queue.Result{}, err
	}
	if v != nil {
		return queue.Result{Outcome: queue.Permanent, Reason: v.Reason, Detail: v.Rule}, nil
	}
	d.Log.Debug("validate: package rules passed", "job_id", jobID, "gap_count", len(gaps))

	decide := func(ctx context.Context, q Querier) (Decision, error) {
		reg, err := LookupRegistration(ctx, q, j.RecordID)
		if err != nil {
			return Decision{}, err
		}
		return Decide(reg, m.Metadata, d.MetaPrefix, d.Configured, d.FrontendServers, j), nil
	}
	dec, err := decide(ctx, d.Pool)
	if err != nil {
		return queue.Result{}, err
	}
	// The commit decides again inside the completion transaction, which
	// already holds the job row lock (queue finish). A registration PUT or
	// DELETE that committed after the first read is seen there; then the
	// result is dropped and the step runs again. The registration is read
	// without a lock: the lock order is registrations before jobs
	// (architecture.md §6.2), and a PUT or DELETE that is still open locks
	// the registration first and then waits for this job row, so it sees
	// the job as this step leaves it (held → released by the PUT, joined →
	// cancelled by the DELETE).
	commit := func(ctx context.Context, tx pgx.Tx) error {
		again, err := decide(ctx, tx)
		if err != nil {
			return err
		}
		if again != dec {
			return ErrDecisionChanged
		}
		if dec.Cancel || dec.Reason != "" {
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE jobs SET meeting_type = $2, sink_profile = $3, registration_id = $4, updated_at = now()
			WHERE job_id = $1`, jobID, dec.MeetingType, dec.SinkProfile, dec.RegistrationID)
		return err
	}
	switch {
	case dec.Cancel:
		return queue.Result{Outcome: queue.OK, Cancel: true, Commit: commit}, nil
	case dec.Reason != "":
		return queue.Result{Outcome: queue.Hold, Reason: dec.Reason, Detail: dec.Detail, Commit: commit}, nil
	}
	return queue.Result{Outcome: queue.OK, Commit: commit}, nil
}
