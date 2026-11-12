// Package deliver implements the deliver step (docs/reference/architecture.md
// §5.2, §6.4; sinks.md §1.4): it builds the delivery request from the
// render blobs and the owner of the job's registration, commits the
// deliveries row in sending before the one request, and records the
// result with the step's completion.
//
// The request (title, content, owner sub and email) lives in memory only.
// The step output and the deliveries row hold IDs and states, and the log
// holds IDs and codes (privacy.md §6).
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/sink"
	"github.com/virtUOS/bbb-recap/internal/steps/render"
)

// Step is the step name.
const Step = "deliver"

// Held and failure reasons of the step (docs/reference/README.md).
const (
	HeldOwnerUnresolved   = "owner_unresolved"
	HeldDeliveryUncertain = "delivery_uncertain"
	ReasonConfigError     = "config_error"
	ReasonRejectedBySink  = "rejected_by_sink"
	ReasonInternalError   = "internal_error"
)

const (
	errorClassInternal = "internal"
	defaultMaxInflight = 2
)

// Sink is one sink profile as the step uses it.
type Sink struct {
	Sender sink.Sender
	// Idempotent marks a sender whose Send may repeat without harm, the
	// frontend sink (sinks.md §4a): no sending row before, no uncertain case.
	Idempotent       bool
	MaxDocumentBytes int64 // 0: sink.DefaultMaxDocumentBytes
	MaxInflight      int   // requests at once in this process; 0: 2
}

// Deps are what the deliver handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	Log   *slog.Logger
	Sinks map[string]Sink // by profile name
}

// Output is the step output: the delivery state and document ID only.
type Output struct {
	State      string `json:"delivery_state"`
	DocumentID string `json:"document_id,omitempty"`
}

// job is what the handler reads before sending.
type job struct {
	profile        string
	registrationID int64
	owner          sink.Owner
	render         render.Output
	prior          *sink.Record
}

// Handler returns the deliver step handler. max_inflight is a semaphore
// per profile in this process (architecture.md §7), taken before the
// sending row is committed.
func Handler(d Deps) queue.Handler {
	slots := map[string]chan struct{}{}
	for name, s := range d.Sinks {
		n := s.MaxInflight
		if n <= 0 {
			n = defaultMaxInflight
		}
		slots[name] = make(chan struct{}, n)
	}
	return func(ctx context.Context, run queue.Run) queue.Result {
		j, err := d.load(ctx, run.JobID)
		switch {
		case errors.Is(err, errNoOwner):
			d.Log.Error("deliver: job has no registered owner", "job_id", run.JobID)
			return internalError("registration_missing")
		case errors.Is(err, errStored):
			d.Log.Error("deliver: render output unreadable", "job_id", run.JobID)
			return internalError("stored_output_unreadable")
		case err != nil:
			code := errorCode(err)
			d.Log.Warn("deliver: cannot load the job", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
		}
		s, ok := d.Sinks[j.profile]
		if !ok {
			d.Log.Error("deliver: sink profile not configured", "job_id", run.JobID, "profile", j.profile)
			return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError, ErrorClass: string(sink.Config),
				ErrorCode: "unknown_sink_profile"}
		}
		req, err := d.request(ctx, run.JobID, j)
		if err != nil {
			code := errorCode(err)
			d.Log.Warn("deliver: cannot read the render output", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
		}

		select {
		case slots[j.profile] <- struct{}{}:
			defer func() { <-slots[j.profile] }()
		case <-ctx.Done():
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: "cancelled"}
		}
		var rec *sink.Record
		if s.Idempotent {
			// The guard of markSending, without the row: a reaped or
			// replaced run stores nothing. A summary stored by a run whose
			// completion then fails is replaced by the next attempt, or
			// deleted by cleanup if the job does not end delivered.
			if err = d.checkActive(ctx, run, j); err == nil {
				rec, err = sink.Keep(ctx, s.Sender, req, j.prior, s.MaxDocumentBytes)
			}
		} else {
			begin := func(ctx context.Context) error { return d.markSending(ctx, run, j) }
			rec, err = sink.Deliver(ctx, s.Sender, req, j.prior, s.MaxDocumentBytes, begin)
		}
		res := Result(rec, err)
		if res.Outcome == queue.Transient && res.ErrorClass == errorClassInternal {
			d.Log.Warn("deliver: not sent", "job_id", run.JobID, "err", res.ErrorCode)
			return res
		}
		d.Log.Info("deliver: done", "job_id", run.JobID, "profile", j.profile, "outcome", string(res.Outcome),
			"error_class", res.ErrorClass, "error_code", res.ErrorCode)
		res.Commit = commit(run.JobID, rec, err)
		return res
	}
}

// Result maps the result of sink.Deliver to the step outcome of
// docs/reference/README.md "Adapter error classes → step outcome" (the table of
// contracts/sink/README.md). An error that is not a *sink.Error is an
// internal failure before sending: transient.
func Result(rec *sink.Record, err error) queue.Result {
	if err == nil {
		out := Output{State: rec.State}
		if rec.DocumentID != nil {
			out.DocumentID = *rec.DocumentID
		}
		b, _ := json.Marshal(out)
		return queue.Result{Outcome: queue.OK, Output: b}
	}
	var serr *sink.Error
	if !errors.As(err, &serr) {
		code := "send_not_started"
		if errors.Is(err, errNotClaimed) {
			code = "not_claimed"
		} else if errors.Is(err, errStopping) {
			code = "job_stopping"
		}
		return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
	}
	r := queue.Result{ErrorClass: string(serr.Class), ErrorCode: serr.Code}
	switch serr.Class {
	case sink.NotSent:
		r.Outcome, r.RetryAfter = queue.Transient, serr.RetryAfter
	case sink.Uncertain:
		r.Outcome, r.Reason = queue.Hold, HeldDeliveryUncertain
	case sink.Config:
		r.Outcome, r.Reason = queue.Permanent, ReasonConfigError
	case sink.RecipientUnresolved:
		r.Outcome, r.Reason = queue.Hold, HeldOwnerUnresolved
	default:
		r.Outcome, r.Reason = queue.Permanent, ReasonRejectedBySink
	}
	return r
}

// commit returns what the completion transaction writes (sinks.md §1.4):
// a 201 sets the row to sent; uncertain keeps it in sending; every other
// class deletes it, so a later attempt may send. Nothing is written for an
// internal failure before begin.
func commit(jobID string, rec *sink.Record, err error) func(context.Context, pgx.Tx) error {
	var serr *sink.Error
	switch {
	case err == nil && rec.State == sink.StateSent:
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
				VALUES ($1, $2, 'sent', $3, $4)
				ON CONFLICT (job_id) DO UPDATE SET state = 'sent', document_id = $3, sent_at = $4`,
				jobID, rec.SinkProfile, rec.DocumentID, rec.SentAt)
			return err
		}
	case err == nil, !errors.As(err, &serr), serr.Class == sink.Uncertain:
		return nil
	}
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE job_id = $1 AND state = 'sending'`, jobID)
		return err
	}
}

var (
	// errNoOwner: the job has no registration with an owner.
	errNoOwner = errors.New("deliver: no registered owner")
	// errStored: the render output does not decode.
	errStored = errors.New("deliver: stored output unreadable")
	// errNotClaimed: the step is no longer this run's, so it must not send.
	errNotClaimed = errors.New("deliver: step no longer claimed")
	// errStopping: the job is being cancelled or purged, or is no longer
	// its registration's active job (rerun).
	errStopping = errors.New("deliver: job stopping")
)

// load reads the job's sink profile, the owner of its registration, the
// render output and the delivery record. One registration has one active
// job: the one whose registration_id references it (architecture.md §5.4).
func (d Deps) load(ctx context.Context, jobID string) (*job, error) {
	j := &job{}
	var profile *string
	var regID *int64
	var sub, email *string
	var out []byte
	var state, docID *string
	var sentAt *time.Time
	err := d.Pool.QueryRow(ctx, `
		SELECT j.sink_profile, j.registration_id, r.owner_sub, r.owner_email, s.output,
			dl.state, dl.document_id, dl.sent_at
		FROM jobs j
		JOIN steps s ON s.job_id = j.job_id AND s.name = 'render'
		LEFT JOIN registrations r ON r.registration_id = j.registration_id AND r.status = 'registered'
		LEFT JOIN deliveries dl ON dl.job_id = j.job_id
		WHERE j.job_id = $1`, jobID).Scan(&profile, &regID, &sub, &email, &out, &state, &docID, &sentAt)
	if err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	if profile == nil || regID == nil || sub == nil || email == nil || *sub == "" || *email == "" {
		return nil, errNoOwner
	}
	if out == nil || json.Unmarshal(out, &j.render) != nil || j.render.SummarySHA256 == "" || j.render.TitleSHA256 == "" {
		return nil, errStored
	}
	j.profile, j.registrationID = *profile, *regID
	j.owner = sink.Owner{Sub: *sub, Email: *email}
	if state != nil {
		j.prior = &sink.Record{JobID: jobID, SinkProfile: j.profile, State: *state, DocumentID: docID}
		if sentAt != nil {
			at := sentAt.UTC().Format(time.RFC3339)
			j.prior.SentAt = &at
		}
	}
	return j, nil
}

// request builds the delivery request from the render blobs (sinks.md §1.2).
func (d Deps) request(ctx context.Context, jobID string, j *job) (sink.Request, error) {
	md, err := d.read(ctx, jobID, j.render.SummarySHA256)
	if err != nil {
		return sink.Request{}, err
	}
	title, err := d.read(ctx, jobID, j.render.TitleSHA256)
	if err != nil {
		return sink.Request{}, err
	}
	return sink.Request{JobID: jobID, SinkProfile: j.profile, Title: string(title), SummaryMD: string(md),
		SummarySHA256: j.render.SummarySHA256, Owner: j.owner}, nil
}

func (d Deps) read(ctx context.Context, jobID, key string) ([]byte, error) {
	rc, err := d.Blobs.Get(ctx, jobID, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// markSending commits the deliveries row in sending before the request
// (sinks.md §1.4 step 1), in its own transaction, guarded by guard.
func (d Deps) markSending(ctx context.Context, run queue.Run, j *job) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := guard(ctx, tx, run, j); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, $2, 'sending')
			ON CONFLICT (job_id) DO NOTHING`, run.JobID, j.profile)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return sink.ErrRecordExists
		}
		return nil
	})
}

// checkActive runs guard in a transaction of its own, before an idempotent
// sender stores anything.
func (d Deps) checkActive(ctx context.Context, run queue.Run, j *job) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error { return guard(ctx, tx, run, j) })
}

// guard checks, like a completion, that the step is still claimed by this
// run (state, attempt and an unexpired lease; the run carries no owner) and
// that the job is neither stopping nor replaced as its registration's
// active job. A reaped or released step therefore never sends. The step row
// is locked before the job row, as in the completion (internal/queue).
func guard(ctx context.Context, tx pgx.Tx, run queue.Run, j *job) error {
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM steps
		WHERE job_id = $1 AND name = $2 AND state = 'claimed' AND attempt = $3 AND lease_expires_at > now()
		FOR UPDATE`, run.JobID, run.Step, run.Attempt).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotClaimed
	}
	if err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT cancel_requested_at IS NULL AND purge_requested_at IS NULL
			AND registration_id IS NOT DISTINCT FROM $2
		FROM jobs WHERE job_id = $1 FOR UPDATE`, run.JobID, j.registrationID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return errStopping
	}
	return nil
}

func internalError(code string) queue.Result {
	return queue.Result{Outcome: queue.Permanent, Reason: ReasonInternalError, ErrorClass: errorClassInternal,
		ErrorCode: code}
}

// errorCode reduces an internal error to a code (no paths or content).
func errorCode(err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "job_not_found"
	case errors.Is(err, blob.ErrNotFound):
		return "blob_not_found"
	case errors.Is(err, blob.ErrHashMismatch):
		return "blob_hash_mismatch"
	case errors.Is(err, blob.ErrTransient):
		return "blob_transient"
	}
	return "deliver"
}
