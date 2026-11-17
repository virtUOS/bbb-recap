package queue

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Admin actions with a sink and the sink statuses (sinks.md §1.8, ADR-016).

// sinkSetup describes the sinks of a job at deliver: profile → status, one
// of pending, sending, sent, failed (rejected_by_sink) or exhausted (pending
// when the step runs out of attempts, which marks it failed).
type sinkSetup struct {
	profile, status string
}

// toDeliverWith runs a job to deliver, writes its sinks and records the
// step outcome that the statuses give: failed, held delivery_uncertain or
// failed with retries_exhausted.
func toDeliverWith(t *testing.T, pool *pgxpool.Pool, e *Engine, id string, sinks ...sinkSetup) {
	t.Helper()
	run := toDeliver(t, e, id)
	var names []string
	failed, sending, exhausted := false, false, false
	for _, s := range sinks {
		names = append(names, s.profile)
		switch s.status {
		case "sending", "sent":
			exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state, document_id) VALUES ($1, $2, $3, $4)`,
				id, s.profile, s.status, map[bool]any{true: "doc-" + s.profile, false: nil}[s.status == "sent"])
			sending = sending || s.status == "sending"
		case "failed":
			exec(t, pool, `INSERT INTO step_items (job_id, step, item, state, failure_reason, detail)
				VALUES ($1, 'deliver', $2, 'failed', 'rejected_by_sink', 'http_400')`, id, s.profile)
			failed = true
		case "exhausted":
			exhausted = true
		}
	}
	exec(t, pool, `UPDATE jobs SET sink_profile = $2 WHERE job_id = $1`, id, joinNames(names))
	switch {
	case failed:
		mustFinish(t, e, run, Result{Outcome: Permanent, Reason: "rejected_by_sink"})
	case sending:
		mustFinish(t, e, run, Result{Outcome: Hold, Reason: "delivery_uncertain"})
	case exhausted:
		exec(t, pool, `UPDATE steps SET attempt = $2 WHERE job_id = $1 AND name = 'deliver'`, id, MaxAttempts("deliver"))
		run.Attempt = MaxAttempts("deliver")
		mustFinish(t, e, run, Result{Outcome: Transient, ErrorClass: "not_sent", ErrorCode: "throttled"})
	}
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

func statuses(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	sinks, err := SinkStatuses(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range sinks {
		out = append(out, s.Profile+"="+s.Status)
	}
	return out
}

func expectSinks(t *testing.T, pool *pgxpool.Pool, id string, want ...string) {
	t.Helper()
	if got := statuses(t, pool, id); !slices.Equal(got, want) {
		t.Fatalf("sinks %v, want %v", got, want)
	}
}

func act(t *testing.T, pool *pgxpool.Pool, f func(tx pgx.Tx) error) error {
	t.Helper()
	return inTx(t, pool, f)
}

// release with sink of a failed sink skips it; the job is delivered when
// every other sink is done (CT-SINK-14).
func TestReleaseSinkSkipsFailed(t *testing.T) {
	pool := actionDB(t)
	e := newEngine(pool, "w1", Steps...)
	id := seedJob(t, pool)
	toDeliverWith(t, pool, e, id, sinkSetup{"docs-a", "failed"}, sinkSetup{"front-a", "sent"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "", "", 0) }); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("release {} of a failed job: %v, want ErrInvalidState", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "docs-a", "doc-1", 0) }); !errors.Is(err, ErrDocumentIDNotAllowed) {
		t.Fatalf("skip with a document ID: %v, want ErrDocumentIDNotAllowed", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "docs-a", "", time.Hour) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "docs-a=skipped_by_admin", "front-a=sent")
	if j := getJob(t, pool, id); j.State != "delivered" || j.Step != "cleanup" || j.Failure != "" {
		t.Fatalf("job %+v, want delivered at cleanup", j)
	}
	if s := getStep(t, pool, id, "cleanup"); s.DueIn < 59*time.Minute {
		t.Errorf("cleanup due in %v, want the retention", s.DueIn)
	}
	if got := actionRows(t, pool, id, "release"); len(got) != 1 || got[0] != "delivered" {
		t.Errorf("release audit rows %v", got)
	}
}

// The only sink, or the last one not skipped, cannot be skipped: purge
// instead (CT-SINK-17); a name not in the job is ErrSinkNotInJob.
func TestReleaseSinkRefused(t *testing.T) {
	pool := actionDB(t)
	e := newEngine(pool, "w1", Steps...)
	id := seedJob(t, pool)
	toDeliverWith(t, pool, e, id, sinkSetup{"docs-a", "failed"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "no-such", "", 0) }); !errors.Is(err, ErrSinkNotInJob) {
		t.Fatalf("unknown sink: %v, want ErrSinkNotInJob", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "docs-a", "", 0) }); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("skip of the only sink: %v, want ErrInvalidState", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id, "no-such", 0) }); !errors.Is(err, ErrSinkNotInJob) {
		t.Fatalf("retry of an unknown sink: %v, want ErrSinkNotInJob", err)
	}
	if j := getJob(t, pool, id); j.State != "failed" {
		t.Errorf("job %+v, want failed", j)
	}

	// A failed job outside deliver takes no sink.
	other := seedJob(t, pool)
	mustFinish(t, e, mustClaim(t, e, other, "validate"), Result{Outcome: OK})
	exec(t, pool, `UPDATE jobs SET sink_profile = 'docs-a' WHERE job_id = $1`, other)
	mustFinish(t, e, mustClaim(t, e, other, "normalize"), Result{Outcome: Permanent, Reason: "events_mismatch"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, other, "docs-a", 0) }); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry with sink outside deliver: %v, want ErrInvalidState", err)
	}
}

// retry with sink resets that sink only; the sent sink stays (CT-SINK-15).
func TestRetrySinkUncertain(t *testing.T) {
	pool := actionDB(t)
	e := newEngine(pool, "w1", Steps...)
	id := seedJob(t, pool)
	toDeliverWith(t, pool, e, id, sinkSetup{"docs-a", "sending"}, sinkSetup{"front-a", "sent"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id, "front-a", 0) }); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry of a sent sink: %v, want ErrInvalidState", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id, "docs-a", 0) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "docs-a=pending", "front-a=sent")
	if j := getJob(t, pool, id); j.State != "running" || j.Held != "" {
		t.Fatalf("job %+v, want running", j)
	}
	if s := getStep(t, pool, id, "deliver"); s.State != "pending" || s.Attempt != 0 {
		t.Fatalf("deliver %+v, want pending with attempts reset", s)
	}
	mustClaim(t, e, id, "deliver")
	if got := actionRows(t, pool, id, "retry"); len(got) != 1 || got[0] != "running" {
		t.Errorf("retry audit rows %v", got)
	}
}

// release without sink confirms every sending sink; a document ID needs
// exactly one; with sink, the job follows the other statuses.
func TestReleaseSending(t *testing.T) {
	pool := actionDB(t)
	e := newEngine(pool, "w1", Steps...)
	id := seedJob(t, pool)
	toDeliverWith(t, pool, e, id, sinkSetup{"docs-a", "sending"}, sinkSetup{"hook-a", "sending"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "", "doc-1", 0) }); !errors.Is(err, ErrDocumentIDNotAllowed) {
		t.Fatalf("document ID with two sending sinks: %v, want ErrDocumentIDNotAllowed", err)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "docs-a", "doc-1", time.Hour) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "docs-a=confirmed_by_admin", "hook-a=sending")
	if j := getJob(t, pool, id); j.State != "held" || j.Held != "delivery_uncertain" {
		t.Fatalf("job %+v, want still held delivery_uncertain", j)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "", "", time.Hour) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "docs-a=confirmed_by_admin", "hook-a=confirmed_by_admin")
	if j := getJob(t, pool, id); j.State != "delivered" {
		t.Fatalf("job %+v, want delivered", j)
	}
	var doc string
	if err := pool.QueryRow(ctx, `SELECT document_id FROM deliveries WHERE job_id = $1 AND sink_profile = 'docs-a'`, id).
		Scan(&doc); err != nil || doc != "doc-1" {
		t.Errorf("document_id %q (%v), want doc-1", doc, err)
	}

	// A sink left pending: the step runs again.
	other := seedJob(t, pool)
	toDeliverWith(t, pool, e, other, sinkSetup{"docs-a", "sending"}, sinkSetup{"hook-a", "pending"})
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, other, "docs-a", "", 0) }); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, other); j.State != "running" {
		t.Fatalf("job %+v, want running", j)
	}
	mustClaim(t, e, other, "deliver")
}

// When deliver runs out of attempts, the engine marks every pending sink
// failed with retries_exhausted (CT-SINK-19); retrying one keeps the other
// failed; skipping that one lets the step run again.
func TestExhaustedSinks(t *testing.T) {
	pool := actionDB(t)
	e := newEngine(pool, "w1", Steps...)
	id := seedJob(t, pool)
	toDeliverWith(t, pool, e, id, sinkSetup{"front-a", "sent"}, sinkSetup{"docs-a", "exhausted"},
		sinkSetup{"hook-a", "exhausted"})
	if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "retries_exhausted" {
		t.Fatalf("job %+v, want failed retries_exhausted", j)
	}
	expectSinks(t, pool, id, "front-a=sent", "docs-a=failed", "hook-a=failed")
	if err := act(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id, "docs-a", 0) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "front-a=sent", "docs-a=pending", "hook-a=failed")
	if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "retries_exhausted" {
		t.Fatalf("job %+v, want still failed (hook-a)", j)
	}
	if err := act(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "hook-a", "", 0) }); err != nil {
		t.Fatal(err)
	}
	expectSinks(t, pool, id, "front-a=sent", "docs-a=pending", "hook-a=skipped_by_admin")
	if j := getJob(t, pool, id); j.State != "running" {
		t.Fatalf("job %+v, want running (docs-a pending)", j)
	}
	mustClaim(t, e, id, "deliver")
}
