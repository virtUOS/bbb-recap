// Package deliver implements the deliver step (docs/reference/architecture.md
// §5.2, §6.4; sinks.md §1.4, §1.8): it builds the delivery request from the
// render blobs and the job's registration, calls each sink of the job that
// is not done yet in delivery order, commits a sink's deliveries row in
// sending before its one request, and records the results with the step's
// completion.
//
// The request (title, content, owner sub and email, links) lives in memory
// only. The step output, the deliveries rows and the step items hold IDs,
// states and codes, and the log holds IDs and codes (privacy.md §6).
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
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

// Sink types of the two delivery groups (sinks.md §1.1).
const (
	TypeFrontend = "frontend"
	TypeDocs     = "docs"
	TypeWebhook  = "webhook"
	TypeMatrix   = "matrix"
)

const (
	errorClassInternal = "internal"
	errorClassWait     = "wait"
	defaultMaxInflight = 2
)

// Sink is one sink profile as the step uses it.
type Sink struct {
	// Type is the profile's type; it picks the delivery group (sinks.md
	// §1.1): frontend and docs first, then webhook and matrix.
	Type   string
	Sender sink.Sender
	// Idempotent marks a sender whose Send may repeat without harm, the
	// frontend sink (sinks.md §4a): no sending row before, no uncertain case.
	Idempotent       bool
	MaxDocumentBytes int64 // 0: sink.DefaultMaxDocumentBytes
	MaxInflight      int   // requests at once in this process; 0: 2
	// DocumentURLTemplate is the link for humans with {id} (docs,
	// sinks.md §3.1); empty: the sink has no link.
	DocumentURLTemplate string
}

// group is the delivery group of a sink type (sinks.md §1.1).
func group(typ string) int {
	if typ == TypeWebhook || typ == TypeMatrix {
		return 2
	}
	return 1
}

// Deps are what the deliver handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	// Results holds the frontend sink's summaries; nil when the backend
	// keeps none. Group 2 waits while a summary waits for pickup there.
	Results blob.Results
	Log     *slog.Logger
	Sinks   map[string]Sink // by profile name
}

// Output is the step output: the sinks with their final statuses, IDs and
// states only.
type Output struct {
	Deliveries []OutputSink `json:"deliveries"`
}

// OutputSink is one sink in Output.
type OutputSink struct {
	SinkProfile string `json:"sink_profile"`
	State       string `json:"state"`
}

// job is what the handler reads before sending.
type job struct {
	sinks          []queue.SinkStatus // in delivery order
	registrationID int64
	owner          sink.Owner
	resultURL      string
	matrixRoom     *string
	createdAt      time.Time
	render         render.Output
}

// result is what one attempt did to one sink it called.
type result struct {
	rec *sink.Record // the record to commit (sent) or kept (uncertain)
	err *sink.Error  // the sink's error class, nil on success
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
		for _, st := range j.sinks {
			if _, ok := d.Sinks[st.Profile]; !ok {
				d.Log.Error("deliver: sink profile not configured", "job_id", run.JobID, "profile", st.Profile)
				return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
					ErrorClass: string(sink.Config), ErrorCode: "unknown_sink_profile"}
			}
		}
		req, err := d.request(ctx, run.JobID, j)
		if err != nil {
			code := errorCode(err)
			d.Log.Warn("deliver: cannot read the render output", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
		}

		a := &attempt{d: d, run: run, j: j, req: req, slots: slots, results: map[string]*result{}}
		a.deliver(ctx)
		res := a.outcome()
		if res.ErrorClass == errorClassInternal && len(a.results) == 0 {
			d.Log.Warn("deliver: not sent", "job_id", run.JobID, "err", res.ErrorCode)
			return res
		}
		d.Log.Info("deliver: done", "job_id", run.JobID, "outcome", string(res.Outcome), "reason", res.Reason,
			"error_class", res.ErrorClass, "error_code", res.ErrorCode)
		res.Commit = a.commit()
		return res
	}
}

// attempt is one run of the step (sinks.md §1.8).
type attempt struct {
	d       Deps
	run     queue.Run
	j       *job
	req     sink.Request
	slots   map[string]chan struct{}
	results map[string]*result
	// internal is the code of an internal failure that stopped the
	// attempt; wait is why group 2 was not called ("" if it was or had
	// nothing to do).
	internal, wait string
}

// status returns the sink's status after what this attempt did.
func (a *attempt) status(st queue.SinkStatus) string {
	r, ok := a.results[st.Profile]
	if !ok {
		return st.Status
	}
	switch {
	case r.err == nil:
		return r.rec.State
	case r.err.Class == sink.Uncertain:
		return queue.SinkSending
	case r.err.Class == sink.Config, r.err.Class == sink.Rejected:
		return queue.SinkFailed
	}
	return queue.SinkPending
}

// deliver calls every pending sink of group 1 in delivery order, then, when
// group 1 is done and nothing waits for the pickup, those of group 2.
func (a *attempt) deliver(ctx context.Context) {
	for g := 1; g <= 2; g++ {
		if g == 2 {
			for _, st := range a.j.sinks {
				if group(a.d.Sinks[st.Profile].Type) == 1 && !done(a.status(st)) {
					a.wait = "group_pending"
					return
				}
			}
			waiting, err := a.pickupPending(ctx)
			if err != nil {
				a.internal = errorCode(err)
				return
			}
			if waiting {
				a.wait = "pickup_pending"
				return
			}
			a.req.Links = a.links()
		}
		for _, st := range a.j.sinks {
			if group(a.d.Sinks[st.Profile].Type) != g || st.Status != queue.SinkPending {
				continue
			}
			if !a.call(ctx, st) {
				return
			}
		}
	}
}

func done(status string) bool {
	return status == queue.SinkSent || status == queue.SinkConfirmedByAdmin || status == queue.SinkSkippedByAdmin
}

// call delivers to one sink. It returns false when an internal failure
// stops the attempt (the step is no longer claimed, the job is stopping, or
// the database failed before the request).
func (a *attempt) call(ctx context.Context, st queue.SinkStatus) bool {
	s := a.d.Sinks[st.Profile]
	req := a.req
	req.SinkProfile = st.Profile
	select {
	case a.slots[st.Profile] <- struct{}{}:
		defer func() { <-a.slots[st.Profile] }()
	case <-ctx.Done():
		a.internal = "cancelled"
		return false
	}
	var rec *sink.Record
	var err error
	if s.Idempotent {
		// The guard of markSending, without the row: a reaped or replaced
		// run stores nothing. A summary stored by a run whose completion
		// then fails is replaced by the next attempt, or deleted by cleanup
		// if the job does not end delivered.
		if err = a.d.checkActive(ctx, a.run, a.j); err == nil {
			rec, err = sink.Keep(ctx, s.Sender, req, nil, s.MaxDocumentBytes)
		}
	} else {
		begin := func(ctx context.Context) error { return a.d.markSending(ctx, a.run, a.j, st.Profile) }
		rec, err = sink.Deliver(ctx, s.Sender, req, nil, s.MaxDocumentBytes, begin)
	}
	var serr *sink.Error
	if err != nil && !errors.As(err, &serr) {
		a.internal = internalCode(err)
		return false
	}
	a.results[st.Profile] = &result{rec: rec, err: serr}
	attrs := []any{"job_id", a.run.JobID, "profile", st.Profile, "status", a.status(st)}
	if serr != nil {
		attrs = append(attrs, "error_class", string(serr.Class), "error_code", serr.Code)
	}
	a.d.Log.Info("deliver: sink called", attrs...)
	return true
}

// links builds the request's links (sinks.md §1.2): the registration's
// result_url under the name of the job's frontend sink when that sink is
// sent, then the document link of every other done sink that has one, in
// delivery order.
func (a *attempt) links() []sink.Link {
	links := []sink.Link{}
	for _, st := range a.j.sinks {
		s := a.d.Sinks[st.Profile]
		if s.Type == TypeFrontend && a.j.resultURL != "" && a.status(st) == queue.SinkSent {
			links = append(links, sink.Link{SinkProfile: st.Profile, URL: a.j.resultURL})
		}
	}
	for _, st := range a.j.sinks {
		s := a.d.Sinks[st.Profile]
		if s.Type == TypeFrontend || s.DocumentURLTemplate == "" || !done(a.status(st)) {
			continue
		}
		id := st.DocumentID
		if r := a.results[st.Profile]; r != nil && r.err == nil {
			id = r.rec.DocumentID
		}
		if id != nil && *id != "" {
			links = append(links, sink.Link{SinkProfile: st.Profile,
				URL: strings.ReplaceAll(s.DocumentURLTemplate, "{id}", url.PathEscape(*id))})
		}
	}
	return links
}

// pickupPending reports whether group 2 waits for the front end's pickup
// (sinks.md §1.8): the registration has a result_url and the job's frontend
// sink is sent and its summary still waits in the blob store.
func (a *attempt) pickupPending(ctx context.Context) (bool, error) {
	if a.j.resultURL == "" || a.d.Results == nil {
		return false, nil
	}
	for _, st := range a.j.sinks {
		if a.d.Sinks[st.Profile].Type != TypeFrontend || a.status(st) != queue.SinkSent {
			continue
		}
		rc, err := a.d.Results.GetSummary(ctx, a.run.JobID)
		if errors.Is(err, blob.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_ = rc.Close()
		return true, nil
	}
	return false, nil
}

// outcome is the step outcome from the sink statuses, by the first rule of
// sinks.md §1.8 that applies.
func (a *attempt) outcome() queue.Result {
	var sending, unresolved, pending bool
	var notSent *sink.Error
	for _, st := range a.j.sinks {
		status, r := a.status(st), a.results[st.Profile]
		switch status {
		case queue.SinkFailed:
			reason, class, code := "", "", ""
			switch {
			case r != nil:
				reason, class, code = failureReason(r.err.Class), string(r.err.Class), r.err.Code
			case st.FailureReason != nil:
				reason = *st.FailureReason
			}
			return queue.Result{Outcome: queue.Permanent, Reason: reason, ErrorClass: class, ErrorCode: code}
		case queue.SinkSending:
			sending = true
		case queue.SinkPending:
			pending = true
			if r != nil && r.err.Class == sink.RecipientUnresolved {
				unresolved = true
			}
			if r != nil && r.err.Class == sink.NotSent && (notSent == nil || r.err.RetryAfter > notSent.RetryAfter) {
				notSent = r.err
			}
		}
	}
	switch {
	case sending:
		return a.classed(queue.Result{Outcome: queue.Hold, Reason: HeldDeliveryUncertain}, sink.Uncertain)
	case unresolved:
		return a.classed(queue.Result{Outcome: queue.Hold, Reason: HeldOwnerUnresolved}, sink.RecipientUnresolved)
	case a.internal != "":
		return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: a.internal}
	case notSent != nil:
		return queue.Result{Outcome: queue.Transient, ErrorClass: string(notSent.Class), ErrorCode: notSent.Code,
			RetryAfter: notSent.RetryAfter}
	case pending:
		return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassWait, ErrorCode: a.wait}
	}
	out := Output{}
	for _, st := range a.j.sinks {
		out.Deliveries = append(out.Deliveries, OutputSink{SinkProfile: st.Profile, State: a.status(st)})
	}
	b, _ := json.Marshal(out)
	return queue.Result{Outcome: queue.OK, Output: b}
}

// classed adds the error class and code of the first sink of this attempt
// with that class.
func (a *attempt) classed(r queue.Result, class sink.Class) queue.Result {
	for _, st := range a.j.sinks {
		if x := a.results[st.Profile]; x != nil && x.err != nil && x.err.Class == class {
			r.ErrorClass, r.ErrorCode = string(class), x.err.Code
			return r
		}
	}
	return r
}

// failureReason maps a permanent sink error class to the failure reason.
func failureReason(c sink.Class) string {
	if c == sink.Config {
		return ReasonConfigError
	}
	return ReasonRejectedBySink
}

// Result maps the result of one sink.Deliver call to the step outcome of
// docs/reference/README.md "Adapter error classes → step outcome" (the table
// of contracts/sink/README.md), as for a job with this one sink. An error
// that is not a *sink.Error is an internal failure before sending:
// transient.
func Result(rec *sink.Record, err error) queue.Result {
	if err == nil {
		b, _ := json.Marshal(Output{Deliveries: []OutputSink{{SinkProfile: rec.SinkProfile, State: rec.State}}})
		return queue.Result{Outcome: queue.OK, Output: b}
	}
	var serr *sink.Error
	if !errors.As(err, &serr) {
		return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: internalCode(err)}
	}
	r := queue.Result{ErrorClass: string(serr.Class), ErrorCode: serr.Code}
	switch serr.Class {
	case sink.NotSent:
		r.Outcome, r.RetryAfter = queue.Transient, serr.RetryAfter
	case sink.Uncertain:
		r.Outcome, r.Reason = queue.Hold, HeldDeliveryUncertain
	case sink.RecipientUnresolved:
		r.Outcome, r.Reason = queue.Hold, HeldOwnerUnresolved
	default:
		r.Outcome, r.Reason = queue.Permanent, failureReason(serr.Class)
	}
	return r
}

// internalCode names an internal failure before sending.
func internalCode(err error) string {
	switch {
	case errors.Is(err, errNotClaimed):
		return "not_claimed"
	case errors.Is(err, errStopping):
		return "job_stopping"
	}
	return "send_not_started"
}

// commit returns what the completion transaction writes (sinks.md §1.4,
// §1.8), per sink called in this attempt: a success sets the row to sent;
// uncertain keeps it in sending; every other class deletes it, so a later
// attempt may send, and config and rejected also mark the sink failed. When
// the step runs out of attempts, the engine marks the sinks still pending
// failed (internal/queue).
func (a *attempt) commit() func(context.Context, pgx.Tx) error {
	jobID := a.run.JobID
	return func(ctx context.Context, tx pgx.Tx) error {
		for _, st := range a.j.sinks {
			r := a.results[st.Profile]
			var err error
			switch {
			case r == nil:
			case r.err == nil:
				_, err = tx.Exec(ctx, `
					INSERT INTO deliveries (job_id, sink_profile, state, document_id, target, sent_at)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (job_id, sink_profile) DO UPDATE
					SET state = $3, document_id = $4, target = coalesce($5, deliveries.target), sent_at = $6`,
					jobID, st.Profile, r.rec.State, nullIfEmpty(r.rec.DocumentID), r.rec.Target, r.rec.SentAt)
			case r.err.Class != sink.Uncertain:
				_, err = tx.Exec(ctx, `DELETE FROM deliveries WHERE job_id = $1 AND sink_profile = $2 AND state = 'sending'`,
					jobID, st.Profile)
				if err == nil && (r.err.Class == sink.Config || r.err.Class == sink.Rejected) {
					err = markFailed(ctx, tx, jobID, st.Profile, failureReason(r.err.Class), r.err.Code)
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
}

// markFailed records a failed sink as a deliver step item with its failure
// reason and code (sinks.md §1.8).
func markFailed(ctx context.Context, tx pgx.Tx, jobID, profile, reason, code string) error {
	var detail *string
	if code != "" {
		detail = &code
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO step_items (job_id, step, item, state, failure_reason, detail)
		VALUES ($1, 'deliver', $2, 'failed', $3, $4)
		ON CONFLICT (job_id, step, item) DO UPDATE SET state = 'failed', failure_reason = $3, detail = $4`,
		jobID, profile, reason, detail)
	return err
}

func nullIfEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
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

// load reads the job's sinks with their statuses, the owner and result_url
// of its registration, the Matrix room and the render output. One
// registration has one active job: the one whose registration_id references
// it (architecture.md §5.4).
func (d Deps) load(ctx context.Context, jobID string) (*job, error) {
	j := &job{}
	var regID *int64
	var sub, email, resultURL *string
	var out []byte
	err := d.Pool.QueryRow(ctx, `
		SELECT j.matrix_room, j.created_at, j.registration_id, r.owner_sub, r.owner_email, r.result_url, s.output
		FROM jobs j
		JOIN steps s ON s.job_id = j.job_id AND s.name = 'render'
		LEFT JOIN registrations r ON r.registration_id = j.registration_id AND r.status = 'registered'
		WHERE j.job_id = $1`, jobID).Scan(&j.matrixRoom, &j.createdAt, &regID, &sub, &email, &resultURL, &out)
	if err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	if j.sinks, err = queue.SinkStatuses(ctx, d.Pool, jobID); err != nil {
		return nil, fmt.Errorf("sinks: %w", err)
	}
	if len(j.sinks) == 0 || regID == nil || sub == nil || email == nil || *sub == "" || *email == "" {
		return nil, errNoOwner
	}
	if out == nil || json.Unmarshal(out, &j.render) != nil || j.render.SummarySHA256 == "" || j.render.TitleSHA256 == "" {
		return nil, errStored
	}
	j.registrationID = *regID
	j.owner = sink.Owner{Sub: *sub, Email: *email}
	if resultURL != nil {
		j.resultURL = *resultURL
	}
	return j, nil
}

// request builds the delivery request from the render blobs (sinks.md
// §1.2), without sink_profile and links, which each call sets.
func (d Deps) request(ctx context.Context, jobID string, j *job) (sink.Request, error) {
	md, err := d.read(ctx, jobID, j.render.SummarySHA256)
	if err != nil {
		return sink.Request{}, err
	}
	title, err := d.read(ctx, jobID, j.render.TitleSHA256)
	if err != nil {
		return sink.Request{}, err
	}
	renderedAt := j.render.RenderedAt
	if renderedAt == "" {
		// Rendered before render recorded the time: the job's creation,
		// a fixed time of the job, stands in.
		renderedAt = j.createdAt.UTC().Format(time.RFC3339)
	}
	return sink.Request{JobID: jobID, Title: string(title), SummaryMD: string(md),
		SummarySHA256: j.render.SummarySHA256, Owner: j.owner, Links: []sink.Link{}, RenderedAt: renderedAt,
		MatrixRoom: j.matrixRoom}, nil
}

func (d Deps) read(ctx context.Context, jobID, key string) ([]byte, error) {
	rc, err := d.Blobs.Get(ctx, jobID, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// markSending commits the sink's deliveries row in sending before the
// request (sinks.md §1.4 step 1), in its own transaction, guarded by guard.
func (d Deps) markSending(ctx context.Context, run queue.Run, j *job, profile string) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := guard(ctx, tx, run, j); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, $2, 'sending')
			ON CONFLICT (job_id, sink_profile) DO NOTHING`, run.JobID, profile)
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
