package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
)

// A Commit error releases the step at once: retrying with the normal
// backoff, the attempt counts, the lease is not waited for.
func TestCommitErrorRetriesAtOnce(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1")
	e.handlers = map[string]Handler{"validate": func(context.Context, Run) Result {
		return Result{Outcome: OK, Commit: func(context.Context, pgx.Tx) error {
			return errors.New("decision changed")
		}}
	}}
	e.names = []string{"validate"}
	reg := metrics.NewWorkerRegistry()
	before := metricstest.Value(t, reg, "recap_step_runs_total", "step", "validate", "outcome", "transient")

	run := mustClaim(t, e, id, "validate")
	e.runStep(ctx, ctx, run)

	s := getStep(t, pool, id, "validate")
	if s.State != "retrying" || s.Attempt != 1 || s.Owner != "" || s.ErrorClass != "internal" ||
		s.ErrorCode != "step_commit" {
		t.Fatalf("step = %+v, want retrying, attempt 1, no owner, internal/step_commit", s)
	}
	// Backoff(1) without jitter is 30 s; nothing waits for the 60 s lease.
	if s.DueIn < 25*time.Second || s.DueIn > 31*time.Second {
		t.Errorf("run_after in %v, want the 30 s backoff", s.DueIn)
	}
	if j := getJob(t, pool, id); j.State != "running" {
		t.Errorf("job = %+v, want running", j)
	}
	if got := metricstest.Value(t, reg, "recap_step_runs_total", "step", "validate", "outcome", "transient") - before; got != 1 {
		t.Errorf("transient runs counted %v times, want 1", got)
	}
}

// On the last attempt a Commit error fails the job like any transient
// result (retries_exhausted).
func TestCommitErrorOnLastAttempt(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1")
	e.handlers = map[string]Handler{"validate": func(context.Context, Run) Result {
		return Result{Outcome: OK, Commit: func(context.Context, pgx.Tx) error { return errors.New("boom") }}
	}}
	e.names = []string{"validate"}
	exec(t, pool, `UPDATE steps SET attempt = $2 - 1 WHERE job_id = $1 AND name = 'validate'`, id,
		MaxAttempts("validate"))
	e.runStep(ctx, ctx, mustClaim(t, e, id, "validate"))
	if s := getStep(t, pool, id, "validate"); s.State != "failed" {
		t.Errorf("step = %+v, want failed", s)
	}
	if j := getJob(t, pool, id); j.State != "failed" {
		t.Errorf("job = %+v, want failed", j)
	}
}
