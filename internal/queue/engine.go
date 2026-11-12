package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/metrics"
)

// Causes of a cancelled step context. Either way the result is dropped.
var (
	errLeaseLost = errors.New("lease lost")
	errShutdown  = errors.New("worker shutting down")
)

// errStepCommit wraps an error of Result.Commit.
var errStepCommit = errors.New("step commit")

// Config configures an Engine.
type Config struct {
	Owner                  string // steps.owner of this process, from NewOwnerID
	Slots                  int    // RECAP_WORKER_SLOTS
	RetentionAfterDelivery time.Duration
	// Handlers by step name. The engine claims only steps that have a
	// handler, so a worker without later steps never takes them.
	Handlers map[string]Handler
	Log      *slog.Logger
	// Drain is how long running steps may finish after the context of
	// Run ends; Run returns within Drain + StopTime. 0 means DefaultDrain.
	Drain time.Duration
}

// Engine claims steps, runs their handlers and records the results.
type Engine struct {
	pool      *pgxpool.Pool
	owner     string
	slots     int
	retention time.Duration
	handlers  map[string]Handler
	names     []string
	log       *slog.Logger
	jitter    func() float64 // random number in [0, 1) for Backoff

	// The constants, as fields so tests can shorten them.
	heartbeatEvery time.Duration
	drain          time.Duration
}

// New returns an engine. Nil handlers are ignored.
func New(pool *pgxpool.Pool, c Config) *Engine {
	e := &Engine{
		pool:      pool,
		owner:     c.Owner,
		slots:     max(c.Slots, 1),
		retention: c.RetentionAfterDelivery,
		handlers:  map[string]Handler{},
		log:       c.Log,
		jitter:    rand.Float64,

		heartbeatEvery: HeartbeatInterval,
		drain:          c.Drain,
	}
	if e.drain <= 0 {
		e.drain = DefaultDrain
	}
	for name, h := range c.Handlers {
		if h != nil {
			e.handlers[name] = h
			e.names = append(e.names, name)
			metrics.InitStep(name, string(OK), string(Transient), string(Permanent), string(Hold))
		}
	}
	slices.Sort(e.names)
	return e
}

// Run claims and runs steps until ctx is cancelled. Then it stops claiming,
// lets running steps finish, cancels the rest and releases them in one
// statement without costing an attempt (architecture.md §9). It returns
// within Config.Drain + StopTime after ctx ends. Database errors are
// logged and retried; Run does not return early.
func (e *Engine) Run(ctx context.Context) {
	// stepsCtx is what handlers run under; finishCtx bounds the recording of
	// results, so a slow database cannot stretch the shutdown.
	stepsCtx, cancelSteps := context.WithCancelCause(context.Background())
	defer cancelSteps(nil)
	finishCtx, abortFinish := context.WithCancel(context.Background())
	defer abortFinish()

	var wg sync.WaitGroup
	ended := make(chan struct{}, e.slots) // one send per finished step
	running := 0
	var lastReap time.Time
	for ctx.Err() == nil {
		if time.Since(lastReap) >= ReapInterval {
			e.reap(ctx)
			lastReap = time.Now()
		}
		for running < e.slots && ctx.Err() == nil {
			run, err := e.claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					e.log.Warn("claim failed", "err", err)
				}
				break
			}
			if run == nil {
				break
			}
			running++
			wg.Go(func() {
				e.runStep(stepsCtx, finishCtx, *run)
				ended <- struct{}{}
			})
		}
		select {
		case <-ctx.Done():
		case <-ended:
			running--
		case <-time.After(PollInterval):
		}
	}

	e.log.Info("worker stopping", "running_steps", running)
	stopped := time.Now()
	idle := make(chan struct{})
	go func() { wg.Wait(); close(idle) }()
	select {
	case <-idle:
	case <-time.After(e.drain):
		e.log.Warn("cancelling running steps")
		cancelSteps(errShutdown)
		select {
		case <-idle:
		case <-time.After(cancelGrace):
		}
	}
	cancelSteps(errShutdown) // results that arrive from now on are dropped
	abortFinish()            // and a result still being recorded is rolled back
	rctx, cancel := context.WithDeadline(context.Background(), stopped.Add(e.drain+cancelGrace+releaseTime))
	defer cancel()
	if n, err := e.releaseOwned(rctx); err != nil {
		e.log.Error("releasing claimed steps failed; their leases will expire", "err", err)
	} else {
		e.log.Info("worker stopped", "released_steps", n)
	}
}

// runStep runs one claimed step with a heartbeat and records its result,
// unless the lease was lost or the worker is shutting down.
// finishCtx bounds the recording of the result.
func (e *Engine) runStep(parent, finishCtx context.Context, run Run) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	e.log.Debug("step started", "job_id", run.JobID, "step", run.Step, "attempt", run.Attempt)
	start := time.Now()

	hbCtx, stopHB := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		e.heartbeat(hbCtx, run, cancel)
	}()
	res := e.handlers[run.Step](ctx, run)
	stopHB()
	<-hbDone
	metrics.StepDuration(run.Step, time.Since(start))

	if cause := context.Cause(ctx); cause != nil {
		e.log.Info("step result dropped", "job_id", run.JobID, "step", run.Step, "cause", cause.Error())
		return
	}
	fctx, fcancel := context.WithTimeout(finishCtx, 10*time.Second)
	defer fcancel()
	ok, err := e.finish(fctx, run, e.owner, res)
	if errors.Is(err, errStepCommit) {
		// The step's own rows could not be written: release the step
		// now instead of waiting for its lease to expire.
		e.log.Warn("step commit failed; retrying the step", "job_id", run.JobID, "step", run.Step, "err", err)
		res = Result{Outcome: Transient, ErrorClass: "internal", ErrorCode: "step_commit"}
		ok, err = e.finish(fctx, run, e.owner, res)
	}
	if ok && err == nil {
		metrics.StepRun(run.Step, string(res.Outcome))
	}
	switch {
	case err != nil:
		e.log.Error("recording step result failed; the lease will expire", "job_id", run.JobID,
			"step", run.Step, "err", err)
	case !ok:
		e.log.Warn("step was no longer claimed; result dropped", "job_id", run.JobID, "step", run.Step)
	default:
		e.log.Info("step finished", "job_id", run.JobID, "step", run.Step, "attempt", run.Attempt,
			"outcome", string(res.Outcome), "reason", res.Reason, "error_class", res.ErrorClass,
			"error_code", res.ErrorCode, "duration_ms", time.Since(start).Milliseconds())
	}
}

// heartbeat extends the lease every HeartbeatInterval. When the step is no
// longer this run's (other owner, other state, or a later attempt after a
// reap and re-claim) it cancels the step with errLeaseLost.
func (e *Engine) heartbeat(ctx context.Context, run Run, lost context.CancelCauseFunc) {
	t := time.NewTicker(e.heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tag, err := e.pool.Exec(ctx, `
			UPDATE steps SET lease_expires_at = now() + $4 * interval '1 millisecond'
			WHERE job_id = $1 AND name = $2 AND owner = $3 AND state = 'claimed' AND attempt = $5`,
			run.JobID, run.Step, e.owner, LeaseDuration.Milliseconds(), run.Attempt)
		if err != nil {
			if ctx.Err() == nil {
				e.log.Warn("heartbeat failed", "job_id", run.JobID, "step", run.Step, "err", err)
			}
			continue
		}
		if tag.RowsAffected() == 0 {
			e.log.Warn("lease lost; cancelling step", "job_id", run.JobID, "step", run.Step)
			lost(errLeaseLost)
			return
		}
	}
}

// claim takes the oldest runnable step that has a handler (architecture.md
// §6.2) and marks its job running. It returns nil when there is none.
func (e *Engine) claim(ctx context.Context) (*Run, error) {
	if len(e.names) == 0 {
		return nil, nil
	}
	var run *Run
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var r Run
		// The outer WHERE repeats the state predicate, so a lost race
		// updates nothing. Locking the job row too keeps Cancel and the
		// claim of the same job apart. A delivered job runs only
		// cleanup (architecture.md §6.2).
		err := tx.QueryRow(ctx, `
			UPDATE steps SET state = 'claimed', owner = $1, attempt = attempt + 1,
				lease_expires_at = now() + $3 * interval '1 millisecond'
			WHERE (job_id, name) = (
				SELECT s.job_id, s.name
				FROM steps s JOIN jobs j ON j.job_id = s.job_id
				WHERE s.state IN ('pending', 'retrying') AND s.run_after <= now()
					AND s.name = j.current_step AND s.name = ANY($2)
					AND (j.state IN ('received', 'running') OR (j.state = 'delivered' AND s.name = 'cleanup'))
				ORDER BY j.created_at, j.job_id
				LIMIT 1
				FOR UPDATE OF s, j SKIP LOCKED)
			AND state IN ('pending', 'retrying')
			RETURNING job_id::text, name, attempt`,
			e.owner, e.names, LeaseDuration.Milliseconds()).Scan(&r.JobID, &r.Step, &r.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE jobs SET state = 'running', updated_at = now()
			WHERE job_id = $1 AND state = 'received'`, r.JobID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err := audit(ctx, tx, System, r.JobID, "running"); err != nil {
				return err
			}
		}
		run = &r
		return nil
	})
	return run, err
}

// reap ends every step whose lease has expired as a transient failure: the
// attempt counts (architecture.md §6.2). It is safe to run in every worker.
func (e *Engine) reap(ctx context.Context) {
	rows, err := e.pool.Query(ctx, `
		SELECT job_id::text, name, attempt FROM steps
		WHERE state = 'claimed' AND lease_expires_at < now()
		ORDER BY lease_expires_at LIMIT 100`)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("reaper query failed", "err", err)
		}
		return
	}
	var expired []Run
	var r Run
	_, err = pgx.ForEachRow(rows, []any{&r.JobID, &r.Step, &r.Attempt}, func() error {
		expired = append(expired, r)
		return nil
	})
	if err != nil {
		e.log.Warn("reaper query failed", "err", err)
		return
	}
	for _, r := range expired {
		ok, err := e.finish(ctx, r, "", Result{
			Outcome: Transient, ErrorClass: "lease", ErrorCode: "lease_expired",
		})
		if err != nil {
			e.log.Warn("reaping step failed", "job_id", r.JobID, "step", r.Step, "err", err)
		} else if ok {
			metrics.LeaseExpired(r.Step)
			e.log.Warn("lease expired; step reaped", "job_id", r.JobID, "step", r.Step)
		}
	}
}

// releaseOwned puts this worker's claimed steps back without costing an
// attempt (clean shutdown, architecture.md §9). A released step of a
// cancelled job then moves the job to cleanup, as any step boundary does.
func (e *Engine) releaseOwned(ctx context.Context) (int, error) {
	rows, err := e.pool.Query(ctx, `
		UPDATE steps SET state = CASE WHEN attempt > 1 THEN 'retrying' ELSE 'pending' END,
			attempt = attempt - 1, owner = NULL, lease_expires_at = NULL, run_after = now()
		WHERE owner = $1 AND state = 'claimed'
		RETURNING job_id::text`, e.owner)
	if err != nil {
		return 0, err
	}
	jobs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range jobs {
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			var stop bool
			if err := tx.QueryRow(ctx, `
				SELECT (cancel_requested_at IS NOT NULL OR purge_requested_at IS NOT NULL)
					AND current_step <> 'cleanup'
				FROM jobs WHERE job_id = $1 FOR UPDATE`, id).Scan(&stop); err != nil {
				return err
			}
			if stop {
				return toCleanup(ctx, tx, id)
			}
			return nil
		})
		if err != nil {
			return len(jobs), err
		}
	}
	return len(jobs), nil
}

// finish records the result of a claimed run of a step in one transaction
// (architecture.md §6.3). The guard is state = 'claimed', the run's attempt
// and owner = owner; an empty owner is the reaper, whose guard is an expired
// lease instead. The attempt keeps a stale result from matching a later
// claim of the same step by the same owner. finish reports false, and
// changes nothing, when the guard matches no row.
func (e *Engine) finish(ctx context.Context, run Run, owner string, r Result) (bool, error) {
	jobID, step, attempt := run.JobID, run.Step, run.Attempt
	done := false
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT 1 FROM steps
			WHERE job_id = $1 AND name = $2 AND state = 'claimed' AND attempt = $4
				AND (owner = $3 OR ($3 = '' AND lease_expires_at < now()))
			FOR UPDATE`, jobID, step, owner, attempt).Scan(new(int))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var cancelled, purging bool
		if err := tx.QueryRow(ctx, `
			SELECT cancel_requested_at IS NOT NULL, purge_requested_at IS NOT NULL
			FROM jobs WHERE job_id = $1 FOR UPDATE`,
			jobID).Scan(&cancelled, &purging); err != nil {
			return err
		}
		if r.Commit != nil {
			if err := r.Commit(ctx, tx); err != nil {
				return fmt.Errorf("%w: %w", errStepCommit, err)
			}
		}
		if purging {
			// A job replaced by a rerun never holds the registration
			// (Rerun). A validate that ran during the rerun may just
			// have joined it again; undo that.
			if _, err := tx.Exec(ctx, `
				UPDATE jobs SET registration_id = NULL
				WHERE job_id = $1 AND registration_id IS NOT NULL
					AND EXISTS (SELECT 1 FROM jobs WHERE rerun_of = $1)`, jobID); err != nil {
				return err
			}
		}
		if r.Cancel && !cancelled {
			if _, err := tx.Exec(ctx, `UPDATE jobs SET cancel_requested_at = now() WHERE job_id = $1`,
				jobID); err != nil {
				return err
			}
			cancelled = true
		}
		// A cancelled or purged job stops at this step boundary and goes
		// to cleanup; the step's outcome no longer changes the job.
		stopping := (cancelled || purging) && step != "cleanup"
		outcome := "delivered" // of a cleanup that succeeds
		switch {
		case purging:
			outcome = "purged"
		case cancelled:
			outcome = "cancelled"
		}
		if err := e.apply(ctx, tx, jobID, step, attempt, r, stopping, outcome); err != nil {
			return err
		}
		if stopping {
			if err := toCleanup(ctx, tx, jobID); err != nil {
				return err
			}
		}
		done = true
		return nil
	})
	return done && err == nil, err
}

// apply writes the outcome to the step and, unless the job is stopping, to
// the job. outcome is the job outcome if cleanup succeeds.
func (e *Engine) apply(ctx context.Context, tx pgx.Tx, jobID, step string, attempt int, r Result,
	stopping bool, outcome string) error {
	switch r.Outcome {
	case OK:
		if err := setStep(ctx, tx, jobID, step, stepUpdate{state: "succeeded", attempt: attempt,
			output: string(r.Output)}); err != nil {
			return err
		}
		if stopping {
			return nil
		}
		return e.advance(ctx, tx, jobID, step, outcome)

	case Transient:
		if attempt < MaxAttempts(step) {
			return setStep(ctx, tx, jobID, step, stepUpdate{state: "retrying", attempt: attempt,
				delay: Backoff(attempt, r.RetryAfter, e.jitter()), errClass: r.ErrorClass, errCode: r.ErrorCode})
		}
		if err := setStep(ctx, tx, jobID, step, stepUpdate{state: "failed", attempt: attempt,
			errClass: r.ErrorClass, errCode: r.ErrorCode}); err != nil {
			return err
		}
		if stopping {
			return nil
		}
		// Also for cleanup of a delivered job: delivered → failed
		// (retries_exhausted), architecture.md §5.1.
		return setJob(ctx, tx, jobID, "failed", "retries_exhausted", r.Detail)

	case Permanent:
		if err := setStep(ctx, tx, jobID, step, stepUpdate{state: "failed", attempt: attempt,
			errClass: r.ErrorClass, errCode: r.ErrorCode}); err != nil {
			return err
		}
		if stopping {
			return nil
		}
		return setJob(ctx, tx, jobID, "failed", r.Reason, r.Detail)

	case Hold:
		// The step waits in pending until the hold is lifted; a hold is not
		// a failed attempt, so the attempt is given back.
		if err := setStep(ctx, tx, jobID, step, stepUpdate{state: "pending", attempt: attempt - 1}); err != nil {
			return err
		}
		if stopping {
			return nil
		}
		return setJob(ctx, tx, jobID, "held", r.Reason, r.Detail)
	}
	return fmt.Errorf("unknown step outcome %q", r.Outcome)
}

// advance moves a job past a succeeded step: to the next step, to delivered
// after deliver (cleanup waits for the retention), or to cleaned with
// outcome after cleanup.
func (e *Engine) advance(ctx context.Context, tx pgx.Tx, jobID, step, outcome string) error {
	switch step {
	case "cleanup":
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET state = 'cleaned', outcome = $2, cleaned_at = now(), updated_at = now()
			WHERE job_id = $1`, jobID, outcome); err != nil {
			return err
		}
		return audit(ctx, tx, System, jobID, "cleaned/"+outcome)

	case "deliver":
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET state = 'delivered', delivered_at = now(), current_step = 'cleanup', updated_at = now()
			WHERE job_id = $1`, jobID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE steps SET run_after = now() + $2 * interval '1 millisecond'
			WHERE job_id = $1 AND name = 'cleanup'`, jobID, e.retention.Milliseconds()); err != nil {
			return err
		}
		return audit(ctx, tx, System, jobID, "delivered")
	}

	next := Steps[slices.Index(Steps, step)+1]
	if _, err := tx.Exec(ctx, `UPDATE jobs SET current_step = $2, updated_at = now() WHERE job_id = $1`,
		jobID, next); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE steps SET run_after = now() WHERE job_id = $1 AND name = $2`, jobID, next)
	return err
}

// setJob puts a job into failed or held with a reason and a detail code,
// and writes the audit row.
func setJob(ctx context.Context, tx pgx.Tx, jobID, state, reason, detail string) error {
	column := "failure_reason"
	if state == "held" {
		column = "held_reason"
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state = $2, `+column+` = $3, detail = $4, updated_at = now()
		WHERE job_id = $1`, jobID, state, nullIfEmpty(reason), nullIfEmpty(detail)); err != nil {
		return err
	}
	if reason != "" {
		state += "/" + reason
	}
	return audit(ctx, tx, System, jobID, state)
}

type stepUpdate struct {
	state             string
	attempt           int
	delay             time.Duration // run_after = now() + delay
	output            string        // JSON, kept when empty
	errClass, errCode string
}

// setStep writes a step's new state and releases its lease. Output and the
// last error are kept when the update does not carry new ones.
func setStep(ctx context.Context, tx pgx.Tx, jobID, step string, u stepUpdate) error {
	_, err := tx.Exec(ctx, `
		UPDATE steps SET state = $3, attempt = $4, run_after = now() + $5 * interval '1 millisecond',
			owner = NULL, lease_expires_at = NULL,
			output = coalesce($6::jsonb, output),
			last_error_class = coalesce($7, last_error_class),
			last_error_code = coalesce($8, last_error_code)
		WHERE job_id = $1 AND name = $2`,
		jobID, step, u.state, u.attempt, u.delay.Milliseconds(),
		nullIfEmpty(u.output), nullIfEmpty(u.errClass), nullIfEmpty(u.errCode))
	return err
}

// nullIfEmpty maps "" to SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
