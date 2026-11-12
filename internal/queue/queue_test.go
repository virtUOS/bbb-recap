package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
)

// These tests need TEST_DATABASE_URL (internal/db/dbtest); without it they
// skip. Leases always run on the database clock: a test expires a lease by
// moving lease_expires_at into the past, never by changing a Go clock.

var ctx = context.Background()

// newDB returns a fresh migrated database. When the test ends it checks that
// every audit row uses an action and detail of architecture.md §6.1.
func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.New(t)
	t.Cleanup(func() { checkAudit(t, pool) })
	return pool
}

var auditDetailRE = regexp.MustCompile(`^(running|delivered|failed|held|cleaned)(/[a-z_]+)?$`)

func checkAudit(t *testing.T, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx, `SELECT action, coalesce(detail, '') FROM audit`)
	if err != nil {
		t.Errorf("read audit: %v", err)
		return
	}
	var action, detail string
	_, err = pgx.ForEachRow(rows, []any{&action, &detail}, func() error {
		// The engine changes job states only, so it writes job_state rows.
		if action != "job_state" || !auditDetailRE.MatchString(detail) {
			t.Errorf("audit row action=%q detail=%q is not a job_state row of architecture.md §6.1", action, detail)
		}
		return nil
	})
	if err != nil {
		t.Errorf("read audit: %v", err)
	}
}

func auditDetails(t *testing.T, pool *pgxpool.Pool, jobID string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT detail FROM audit WHERE job_id = $1 ORDER BY audit_id`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// seedJob creates a committed package and its job, as the ingest commit does.
func seedJob(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := NewJobID()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		pkg := randomHex(32)
		if _, err := tx.Exec(ctx, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, 'bbb-fixture-a', $3, 'meeting-fixture', 'committed', 0)`, pkg, id, id); err != nil {
			return err
		}
		return CreateJob(ctx, tx, NewJob{JobID: id, PackageID: pkg, ConfigHash: randomHex(32)})
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

func okHandler(context.Context, Run) Result { return Result{Outcome: OK} }

// newEngine returns an engine with a no-op handler for each named step and
// no jitter.
func newEngine(pool *pgxpool.Pool, owner string, steps ...string) *Engine {
	h := map[string]Handler{}
	for _, s := range steps {
		h[s] = okHandler
	}
	e := New(pool, Config{Owner: owner, Slots: 2, RetentionAfterDelivery: time.Hour, Handlers: h,
		Log: slog.New(slog.DiscardHandler)})
	e.jitter = func() float64 { return 0.5 }
	return e
}

type stepRow struct {
	State, Owner, ErrorClass, ErrorCode string
	Attempt                             int
	DueIn                               time.Duration // run_after − now()
}

func getStep(t *testing.T, pool *pgxpool.Pool, jobID, name string) stepRow {
	t.Helper()
	var s stepRow
	var dueMs float64
	err := pool.QueryRow(ctx, `
		SELECT state, coalesce(owner, ''), coalesce(last_error_class, ''), coalesce(last_error_code, ''), attempt,
			extract(epoch FROM run_after - now()) * 1000
		FROM steps WHERE job_id = $1 AND name = $2`, jobID, name).
		Scan(&s.State, &s.Owner, &s.ErrorClass, &s.ErrorCode, &s.Attempt, &dueMs)
	if err != nil {
		t.Fatalf("read step %s: %v", name, err)
	}
	s.DueIn = time.Duration(dueMs) * time.Millisecond
	return s
}

type jobRow struct {
	State, Step, Held, Failure, Outcome, Detail string
	CancelRequested                             bool
}

func getJob(t *testing.T, pool *pgxpool.Pool, jobID string) jobRow {
	t.Helper()
	var j jobRow
	err := pool.QueryRow(ctx, `
		SELECT state, coalesce(current_step, ''), coalesce(held_reason, ''), coalesce(failure_reason, ''),
			coalesce(outcome, ''), coalesce(detail, ''), cancel_requested_at IS NOT NULL
		FROM jobs WHERE job_id = $1`, jobID).
		Scan(&j.State, &j.Step, &j.Held, &j.Failure, &j.Outcome, &j.Detail, &j.CancelRequested)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	return j
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustClaim(t *testing.T, e *Engine, wantJob, wantStep string) Run {
	t.Helper()
	run, err := e.claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if run == nil {
		t.Fatalf("claim: nothing to claim, want %s", wantStep)
	}
	if run.JobID != wantJob || run.Step != wantStep {
		t.Fatalf("claimed %s/%s, want %s/%s", run.JobID, run.Step, wantJob, wantStep)
	}
	return *run
}

func mustNotClaim(t *testing.T, e *Engine) {
	t.Helper()
	run, err := e.claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if run != nil {
		t.Fatalf("claimed %s/%s, want nothing", run.JobID, run.Step)
	}
}

func mustFinish(t *testing.T, e *Engine, run Run, r Result) {
	t.Helper()
	ok, err := e.finish(ctx, run, e.owner, r)
	if err != nil || !ok {
		t.Fatalf("finish %s: ok=%v err=%v", run.Step, ok, err)
	}
}

func expireLease(t *testing.T, pool *pgxpool.Pool, jobID, step string) {
	t.Helper()
	exec(t, pool, `UPDATE steps SET lease_expires_at = now() - interval '1 second' WHERE job_id = $1 AND name = $2`,
		jobID, step)
}

func TestCreateJob(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	if j := getJob(t, pool, id); j.State != "received" || j.Step != "validate" {
		t.Errorf("job = %+v, want received at validate", j)
	}
	for _, name := range Steps {
		if s := getStep(t, pool, id, name); s.State != "pending" || s.Attempt != 0 {
			t.Errorf("step %s = %+v, want pending, attempt 0", name, s)
		}
	}
}

func TestCreateJobRollback(t *testing.T) {
	pool := newDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, pkg := NewJobID(), randomHex(32)
	if _, err := tx.Exec(ctx, `
		INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
		VALUES ($1, $2, 'bbb-fixture-a', 'rec-1', 'meeting-fixture', 'committed', 0)`, pkg, id); err != nil {
		t.Fatal(err)
	}
	if err := CreateJob(ctx, tx, NewJob{JobID: id, PackageID: pkg, ConfigHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs, steps int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM steps)`).
		Scan(&jobs, &steps); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || steps != 0 {
		t.Errorf("after rollback: %d jobs, %d steps, want none", jobs, steps)
	}
}

func TestClaimRace(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *Run, n)
	for i := range n {
		e := newEngine(pool, "w"+string(rune('a'+i)), "validate")
		wg.Go(func() {
			<-start
			run, err := e.claim(ctx)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			results <- run
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for r := range results {
		if r != nil {
			winners++
			if r.JobID != id || r.Step != "validate" || r.Attempt != 1 {
				t.Errorf("claimed %+v", r)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want exactly 1", winners)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "claimed" || s.Attempt != 1 {
		t.Errorf("step = %+v", s)
	}
	if j := getJob(t, pool, id); j.State != "running" {
		t.Errorf("job state %s, want running", j.State)
	}
	if got := auditDetails(t, pool, id); len(got) != 1 || got[0] != "running" {
		t.Errorf("audit = %v, want [running]", got)
	}
}

func TestClaimOnlyRegisteredSteps(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "normalize")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: OK, Output: []byte(`{"k":"v"}`)})
	mustFinish(t, e, mustClaim(t, e, id, "normalize"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "running" || j.Step != "transcribe" {
		t.Errorf("job = %+v, want running at transcribe", j)
	}
	mustNotClaim(t, e) // no transcribe handler in this worker
	var out string
	if err := pool.QueryRow(ctx, `SELECT output::text FROM steps WHERE job_id = $1 AND name = 'validate'`, id).
		Scan(&out); err != nil || out != `{"k": "v"}` {
		t.Errorf("validate output = %q, %v", out, err)
	}
}

func TestOldestJobFirst(t *testing.T) {
	pool := newDB(t)
	first := seedJob(t, pool)
	second := seedJob(t, pool)
	exec(t, pool, `UPDATE jobs SET created_at = now() - interval '1 hour' WHERE job_id = $1`, second)
	e := newEngine(pool, "w1", "validate")
	mustClaim(t, e, second, "validate")
	mustClaim(t, e, first, "validate")
}

func TestReapedStepLateCompletionCommitsNothing(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	slow := newEngine(pool, "slow", "validate")
	run := mustClaim(t, slow, id, "validate")

	expireLease(t, pool, id, "validate")
	newEngine(pool, "reaper", "validate").reap(ctx)
	if s := getStep(t, pool, id, "validate"); s.State != "retrying" || s.Attempt != 1 || s.ErrorCode != "lease_expired" {
		t.Fatalf("reaped step = %+v, want retrying, attempt 1 counted", s)
	}

	committed := false
	ok, err := slow.finish(ctx, run, slow.owner, Result{
		Outcome: OK,
		Commit: func(context.Context, pgx.Tx) error {
			committed = true
			return nil
		},
	})
	if err != nil || ok {
		t.Fatalf("late finish: ok=%v err=%v, want ok=false", ok, err)
	}
	if committed {
		t.Error("Commit ran for a reaped step")
	}
	if j := getJob(t, pool, id); j.Step != "validate" {
		t.Errorf("job advanced to %s", j.Step)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "retrying" {
		t.Errorf("step = %+v, want still retrying", s)
	}
}

func TestReaperExhaustsAttempts(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	for attempt := 1; attempt <= 3; attempt++ {
		exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
		mustClaim(t, e, id, "validate")
		expireLease(t, pool, id, "validate")
		e.reap(ctx)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "failed" || s.Attempt != 3 {
		t.Errorf("step = %+v, want failed after 3 attempts", s)
	}
	if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "retries_exhausted" {
		t.Errorf("job = %+v, want failed/retries_exhausted", j)
	}
}

func TestCleanReleaseCostsNoAttempt(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	mustClaim(t, e, id, "validate")
	n, err := e.releaseOwned(ctx)
	if err != nil || n != 1 {
		t.Fatalf("releaseOwned = %d, %v", n, err)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "pending" || s.Attempt != 0 || s.Owner != "" {
		t.Errorf("released step = %+v, want pending, attempt 0, no owner", s)
	}

	// A hard kill: the lease expires and the reaper counts the attempt.
	mustClaim(t, e, id, "validate")
	expireLease(t, pool, id, "validate")
	e.reap(ctx)
	if s := getStep(t, pool, id, "validate"); s.State != "retrying" || s.Attempt != 1 {
		t.Errorf("reaped step = %+v, want retrying, attempt 1", s)
	}
}

// Run stops claiming on cancel, cancels a step that does not finish within
// the drain time and releases it without costing an attempt.
func TestRunShutdownReleasesSteps(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	started := make(chan struct{})
	e := newEngine(pool, "w1")
	e.handlers = map[string]Handler{
		"validate": okHandler,
		"normalize": func(ctx context.Context, _ Run) Result {
			close(started)
			<-ctx.Done()
			return Result{Outcome: Transient, ErrorClass: "retryable", ErrorCode: "cancelled"}
		},
	}
	e.names = []string{"normalize", "validate"}
	e.drain = 100 * time.Millisecond

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.Run(runCtx); close(done) }()
	select {
	case <-started: // validate ran and normalize was claimed at once, without waiting for a poll
	case <-time.After(PollInterval):
		t.Fatal("normalize did not start")
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if s := getStep(t, pool, id, "validate"); s.State != "succeeded" {
		t.Errorf("validate = %+v, want succeeded", s)
	}
	if s := getStep(t, pool, id, "normalize"); s.State != "pending" || s.Attempt != 0 || s.ErrorCode != "" {
		t.Errorf("normalize = %+v, want pending, attempt 0, result dropped", s)
	}
}

// When the heartbeat finds the step taken, the step context is cancelled and
// the result is dropped.
func TestLostLeaseCancelsStep(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1")
	e.heartbeatEvery = 50 * time.Millisecond
	cause := make(chan error, 1)
	e.handlers = map[string]Handler{"validate": func(ctx context.Context, _ Run) Result {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return Result{Outcome: OK}
	}}
	e.names = []string{"validate"}
	run := mustClaim(t, e, id, "validate")
	exec(t, pool, `UPDATE steps SET owner = 'other' WHERE job_id = $1`, id) // reaped and claimed elsewhere

	done := make(chan struct{})
	go func() { e.runStep(ctx, ctx, run); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("step was not cancelled")
	}
	if err := <-cause; !errors.Is(err, errLeaseLost) {
		t.Errorf("cause = %v, want errLeaseLost", err)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "claimed" || s.Owner != "other" {
		t.Errorf("step = %+v, want untouched", s)
	}
}

func TestTransientBackoffAndMaxAttempts(t *testing.T) {
	for _, tt := range []struct {
		step string
		max  int
	}{{"validate", 3}, {"transcribe", 5}, {"deliver", 10}} {
		t.Run(tt.step, func(t *testing.T) {
			pool := newDB(t)
			id := seedJob(t, pool)
			exec(t, pool, `UPDATE jobs SET current_step = $2 WHERE job_id = $1`, id, tt.step)
			e := newEngine(pool, "w1", tt.step)
			for attempt := 1; attempt <= tt.max; attempt++ {
				run := mustClaim(t, e, id, tt.step)
				if run.Attempt != attempt {
					t.Fatalf("attempt %d, want %d", run.Attempt, attempt)
				}
				mustFinish(t, e, run, Result{Outcome: Transient, ErrorClass: "retryable", ErrorCode: "http_503"})
				s := getStep(t, pool, id, tt.step)
				if attempt == tt.max {
					if s.State != "failed" {
						t.Fatalf("after %d attempts: step %+v, want failed", attempt, s)
					}
					break
				}
				want := Backoff(attempt, 0, 0.5)
				if s.State != "retrying" || s.ErrorClass != "retryable" || s.ErrorCode != "http_503" ||
					s.DueIn < want-2*time.Second || s.DueIn > want {
					t.Fatalf("attempt %d: step %+v, want retrying due in %v", attempt, s, want)
				}
				mustNotClaim(t, e) // not due yet
				exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
			}
			if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "retries_exhausted" {
				t.Errorf("job = %+v, want failed/retries_exhausted", j)
			}
			got := auditDetails(t, pool, id)
			if len(got) != 2 || got[0] != "running" || got[1] != "failed/retries_exhausted" {
				t.Errorf("audit = %v", got)
			}
		})
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Transient, RetryAfter: 10 * time.Minute})
	if s := getStep(t, pool, id, "validate"); s.DueIn < 10*time.Minute-2*time.Second || s.DueIn > 10*time.Minute {
		t.Errorf("due in %v, want 10m", s.DueIn)
	}
}

func TestPermanentFailsJob(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Permanent, Reason: "events_mismatch",
		Detail: "malformed_xml", ErrorClass: "input", ErrorCode: "events_xml"})
	if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "events_mismatch" || j.Detail != "malformed_xml" {
		t.Errorf("job = %+v", j)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "failed" || s.ErrorCode != "events_xml" {
		t.Errorf("step = %+v", s)
	}
	mustNotClaim(t, e)
	if got := auditDetails(t, pool, id); len(got) != 2 || got[1] != "failed/events_mismatch" {
		t.Errorf("audit = %v", got)
	}
}

func TestHoldAndReleaseHeld(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Hold, Reason: "no_registration"})
	if s := getStep(t, pool, id, "validate"); s.State != "pending" || s.Attempt != 0 {
		t.Errorf("held step = %+v, want pending, attempt 0", s)
	}
	if j := getJob(t, pool, id); j.State != "held" || j.Held != "no_registration" {
		t.Errorf("job = %+v, want held/no_registration", j)
	}
	mustNotClaim(t, e)

	release := func(reasons ...string) bool {
		t.Helper()
		var released bool
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			released, err = ReleaseHeld(ctx, tx, Actor{Kind: "client", ID: "frontend.pilos-fixture"}, id, reasons...)
			return err
		})
		if err != nil {
			t.Fatalf("ReleaseHeld: %v", err)
		}
		return released
	}
	if release("owner_unresolved") {
		t.Fatal("released a job held for another reason")
	}
	if !release("no_registration", "owner_unresolved") {
		t.Fatal("job not released")
	}
	if j := getJob(t, pool, id); j.State != "running" || j.Held != "" || j.Detail != "" {
		t.Errorf("job = %+v, want running without held reason", j)
	}
	if release("no_registration") {
		t.Error("released a job that is not held")
	}
	if run := mustClaim(t, e, id, "validate"); run.Attempt != 1 {
		t.Errorf("attempt = %d, want 1: hold and release cost no attempt", run.Attempt)
	}
	want := []string{"running", "held/no_registration", "running"}
	if got := auditDetails(t, pool, id); len(got) != 3 || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("audit = %v, want %v", got, want)
	}
	var kind, actor string
	if err := pool.QueryRow(ctx, `SELECT actor_kind, actor_id FROM audit WHERE job_id = $1 ORDER BY audit_id DESC LIMIT 1`,
		id).Scan(&kind, &actor); err != nil || kind != "client" || actor != "frontend.pilos-fixture" {
		t.Errorf("release audit actor = %s %s, %v", kind, actor, err)
	}
}

func TestDeliverThenCleanup(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	for _, step := range Steps[:7] {
		mustFinish(t, e, mustClaim(t, e, id, step), Result{Outcome: OK})
	}
	if j := getJob(t, pool, id); j.State != "delivered" || j.Step != "cleanup" {
		t.Fatalf("job = %+v, want delivered at cleanup", j)
	}
	if s := getStep(t, pool, id, "cleanup"); s.DueIn < time.Hour-2*time.Second || s.DueIn > time.Hour {
		t.Errorf("cleanup due in %v, want the retention (1h)", s.DueIn)
	}
	mustNotClaim(t, e)
	exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "delivered" {
		t.Errorf("job = %+v, want cleaned/delivered", j)
	}
	got := auditDetails(t, pool, id)
	if len(got) != 3 || got[0] != "running" || got[1] != "delivered" || got[2] != "cleaned/delivered" {
		t.Errorf("audit = %v", got)
	}
}

func cancel(t *testing.T, pool *pgxpool.Pool, id string) error {
	t.Helper()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return Cancel(ctx, tx, Actor{Kind: "client", ID: "frontend.pilos-fixture"}, id)
	})
}

func TestCancelBeforeClaim(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	if err := cancel(t, pool, id); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if j := getJob(t, pool, id); !j.CancelRequested || j.Step != "cleanup" {
		t.Fatalf("job = %+v, want at cleanup", j)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
		t.Errorf("job = %+v, want cleaned/cancelled", j)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "pending" {
		t.Errorf("validate = %+v, want never run", s)
	}
}

func TestCancelHeldJob(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Hold, Reason: "no_registration"})
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
		t.Errorf("job = %+v, want cleaned/cancelled", j)
	}
	want := []string{"running", "held/no_registration", "running", "cleaned/cancelled"}
	if got := auditDetails(t, pool, id); len(got) != 4 || got[2] != want[2] || got[3] != want[3] {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestCancelDuringClaimedStep(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "normalize", "cleanup")
	run := mustClaim(t, e, id, "validate")
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.Step != "validate" || !j.CancelRequested {
		t.Fatalf("job = %+v, want still at validate until the step ends", j)
	}
	// The step ends with a hold, but the job stops at the boundary instead.
	mustFinish(t, e, run, Result{Outcome: Hold, Reason: "no_registration"})
	if j := getJob(t, pool, id); j.State != "running" || j.Step != "cleanup" || j.Held != "" {
		t.Fatalf("job = %+v, want running at cleanup", j)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
		t.Errorf("job = %+v, want cleaned/cancelled", j)
	}
}

func TestCancelAtShutdownRelease(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	mustClaim(t, e, id, "validate")
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.releaseOwned(ctx); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.Step != "cleanup" {
		t.Errorf("job = %+v, want at cleanup", j)
	}
	mustClaim(t, e, id, "cleanup")
}

// toDeliver runs a job's steps up to deliver and claims deliver.
func toDeliver(t *testing.T, e *Engine, id string) Run {
	t.Helper()
	for _, step := range Steps[:6] {
		mustFinish(t, e, mustClaim(t, e, id, step), Result{Outcome: OK})
	}
	return mustClaim(t, e, id, "deliver")
}

// Once deliver has been claimed, a job can no longer be cancelled
// (architecture.md §5.1).
func TestCancelNotAllowed(t *testing.T) {
	pool := newDB(t)
	e := newEngine(pool, "w1", Steps...)

	claimed := seedJob(t, pool)
	toDeliver(t, e, claimed)
	if err := cancel(t, pool, claimed); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel with deliver claimed: %v, want ErrNotCancellable", err)
	}

	// deliver commits its sending row before the request (sinks.md §1.4).
	uncertain := seedJob(t, pool)
	run := toDeliver(t, e, uncertain)
	exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-main', 'sending')`, uncertain)
	mustFinish(t, e, run, Result{Outcome: Hold, Reason: "delivery_uncertain"})
	if err := cancel(t, pool, uncertain); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel held delivery_uncertain: %v, want ErrNotCancellable", err)
	}

	delivered := seedJob(t, pool)
	mustFinish(t, e, toDeliver(t, e, delivered), Result{Outcome: OK})
	if err := cancel(t, pool, delivered); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel delivered job: %v, want ErrNotCancellable", err)
	}

	// A clean shutdown gives the deliver attempt back, but its sending row stays.
	sending := seedJob(t, pool)
	stopping := newEngine(pool, "w2", Steps...)
	toDeliver(t, stopping, sending)
	exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-main', 'sending')`, sending)
	if _, err := stopping.releaseOwned(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cancel(t, pool, sending); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel with a deliveries row: %v, want ErrNotCancellable", err)
	}

	if err := cancel(t, pool, NewJobID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancel unknown job: %v, want ErrNotFound", err)
	}
}

func TestCancelFailedJob(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Permanent, Reason: "events_mismatch"})
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.State != "running" || j.Step != "cleanup" || j.Failure != "" {
		t.Fatalf("job = %+v, want running at cleanup", j)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
		t.Errorf("job = %+v, want cleaned/cancelled", j)
	}
	want := []string{"running", "failed/events_mismatch", "running", "cleaned/cancelled"}
	if got := auditDetails(t, pool, id); len(got) != 4 || got[2] != want[2] || got[3] != want[3] {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

// A held step that is released and held again keeps its attempt count, so
// holds never use up attempts.
func TestHoldReleaseKeepsAttempts(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: Transient})
	exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
	for range 5 {
		run := mustClaim(t, e, id, "validate")
		if run.Attempt != 2 {
			t.Fatalf("attempt = %d, want 2 (one transient failure, holds not counted)", run.Attempt)
		}
		mustFinish(t, e, run, Result{Outcome: Hold, Reason: "no_registration"})
		if s := getStep(t, pool, id, "validate"); s.Attempt != 1 {
			t.Fatalf("held step attempt = %d, want 1", s.Attempt)
		}
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := ReleaseHeld(ctx, tx, System, id, "no_registration")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A deliveries row alone makes a job not cancellable, even with the deliver
// step pending and no attempt counted.
func TestCancelRefusedWithDeliveriesRow(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	exec(t, pool, `UPDATE jobs SET current_step = 'deliver' WHERE job_id = $1`, id)
	exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-main', 'sending')`, id)
	if s := getStep(t, pool, id, "deliver"); s.State != "pending" || s.Attempt != 0 {
		t.Fatalf("deliver = %+v", s)
	}
	if err := cancel(t, pool, id); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel: %v, want ErrNotCancellable", err)
	}
	if j := getJob(t, pool, id); j.CancelRequested {
		t.Error("cancel was recorded")
	}
}

// validate on a tombstone returns Cancel: the job goes to cleanup.
func TestResultCancel(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "normalize", "cleanup")
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: OK, Cancel: true})
	if j := getJob(t, pool, id); j.Step != "cleanup" || !j.CancelRequested {
		t.Fatalf("job = %+v, want at cleanup", j)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
		t.Errorf("job = %+v, want cleaned/cancelled", j)
	}
}

// A step's Commit runs inside the completion transaction: its error rolls
// back the whole result.
func TestResultCommit(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	run := mustClaim(t, e, id, "validate")
	ok, err := e.finish(ctx, run, e.owner, Result{Outcome: OK,
		Commit: func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE jobs SET meeting_type = 'meeting-summary' WHERE job_id = $1`, id); err != nil {
				return err
			}
			return errors.New("boom")
		}})
	if err == nil || ok {
		t.Fatalf("finish = %v, %v, want an error", ok, err)
	}
	var mt *string
	if err := pool.QueryRow(ctx, `SELECT meeting_type FROM jobs WHERE job_id = $1`, id).Scan(&mt); err != nil || mt != nil {
		t.Errorf("meeting_type = %v, %v, want rolled back", mt, err)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "claimed" {
		t.Errorf("step = %+v, want still claimed", s)
	}
	mustFinish(t, e, run, Result{Outcome: OK, Commit: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE jobs SET meeting_type = 'meeting-summary' WHERE job_id = $1`, id)
		return err
	}})
	if err := pool.QueryRow(ctx, `SELECT meeting_type FROM jobs WHERE job_id = $1`, id).Scan(&mt); err != nil ||
		mt == nil || *mt != "meeting-summary" {
		t.Errorf("meeting_type = %v, %v", mt, err)
	}
}

// After a reap and a new claim by the same owner, the old
// run's result must not match the new claim.
func TestStaleResultAfterReclaimBySameOwner(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	old := mustClaim(t, e, id, "validate")
	expireLease(t, pool, id, "validate")
	e.reap(ctx)
	exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
	current := mustClaim(t, e, id, "validate")
	if current.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", current.Attempt)
	}

	ok, err := e.finish(ctx, old, e.owner, Result{Outcome: OK, Commit: func(context.Context, pgx.Tx) error {
		t.Error("Commit of the stale run ran")
		return nil
	}})
	if err != nil || ok {
		t.Fatalf("stale finish: ok=%v err=%v, want ok=false", ok, err)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "claimed" || s.Attempt != 2 {
		t.Fatalf("step = %+v, want still claimed by attempt 2", s)
	}
	mustFinish(t, e, current, Result{Outcome: OK})
	if j := getJob(t, pool, id); j.Step != "normalize" {
		t.Errorf("job = %+v, want at normalize", j)
	}
}

// waitForLockWaiter returns once a session of this test database waits for
// a row lock.
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waits for a lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Cancel waits for the job lock of a completion that is in
// progress. After the completion commits, Cancel must see the new step state:
// the job exists and ends at cleanup, whatever the step's outcome was.
func TestCancelRacesCompletion(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  Result
	}{
		{"ok advances current_step", Result{Outcome: OK}},
		{"hold", Result{Outcome: Hold, Reason: "no_registration"}},
		{"transient", Result{Outcome: Transient, ErrorClass: "retryable", ErrorCode: "http_503"}},
		{"permanent", Result{Outcome: Permanent, Reason: "events_mismatch"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := newDB(t)
			id := seedJob(t, pool)
			e := newEngine(pool, "w1", "validate", "normalize", "cleanup")
			run := mustClaim(t, e, id, "validate")

			// The Commit hook runs while finish holds the step and job locks:
			// it is the barrier that keeps the completion open.
			locked, release := make(chan struct{}), make(chan struct{})
			res := tt.res
			res.Commit = func(context.Context, pgx.Tx) error {
				close(locked)
				<-release
				return nil
			}
			finished := make(chan error, 1)
			go func() {
				ok, err := e.finish(ctx, run, e.owner, res)
				if err == nil && !ok {
					err = errors.New("step was not claimed")
				}
				finished <- err
			}()
			<-locked
			cancelled := make(chan error, 1)
			go func() { cancelled <- cancel(t, pool, id) }()
			waitForLockWaiter(t, pool) // Cancel waits for the job lock
			close(release)

			if err := <-finished; err != nil {
				t.Fatalf("finish: %v", err)
			}
			if err := <-cancelled; err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if j := getJob(t, pool, id); j.State != "running" || j.Step != "cleanup" || !j.CancelRequested {
				t.Fatalf("job = %+v, want running at cleanup with a cancel request", j)
			}
			mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
			if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "cancelled" {
				t.Errorf("job = %+v, want cleaned/cancelled", j)
			}
		})
	}
}

// A handler that ignores cancellation cannot hold up the
// shutdown. Run returns after drain + cancelGrace and releases the step.
func TestRunShutdownWithStuckHandler(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	started, stuck := make(chan struct{}), make(chan struct{})
	defer close(stuck)
	e := newEngine(pool, "w1")
	e.handlers = map[string]Handler{"validate": func(context.Context, Run) Result {
		close(started)
		<-stuck // ignores ctx
		return Result{Outcome: OK}
	}}
	e.names = []string{"validate"}
	e.drain = 100 * time.Millisecond

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.Run(runCtx); close(done) }()
	<-started
	begin := time.Now()
	stop()
	select {
	case <-done:
	case <-time.After(e.drain + cancelGrace + releaseTime):
		t.Fatal("Run did not return within its budget")
	}
	t.Logf("Run returned after %v", time.Since(begin))
	if s := getStep(t, pool, id, "validate"); s.State != "pending" || s.Attempt != 0 {
		t.Errorf("validate = %+v, want released: pending, attempt 0", s)
	}
}

// After a reap and a re-claim by the same owner, the old
// run's heartbeat must not extend the new claim's lease; the old run is
// cancelled instead.
func TestHeartbeatOfStaleRunCancelsIt(t *testing.T) {
	pool := newDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1")
	e.heartbeatEvery = 50 * time.Millisecond
	cause := make(chan error, 1)
	e.handlers = map[string]Handler{"validate": func(ctx context.Context, _ Run) Result {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return Result{Outcome: OK}
	}}
	e.names = []string{"validate"}
	old := mustClaim(t, e, id, "validate")
	// Reaped and claimed again by the same owner: attempt 2, a short lease.
	exec(t, pool, `UPDATE steps SET attempt = 2, lease_expires_at = now() + interval '5 seconds'
		WHERE job_id = $1`, id)

	done := make(chan struct{})
	go func() { e.runStep(ctx, ctx, old); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale run was not cancelled")
	}
	if err := <-cause; !errors.Is(err, errLeaseLost) {
		t.Errorf("cause = %v, want errLeaseLost", err)
	}
	var left float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM lease_expires_at - now()) FROM steps
		WHERE job_id = $1 AND name = 'validate'`, id).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left > 5 {
		t.Errorf("lease of the new claim was extended (%.1f s left)", left)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "claimed" || s.Attempt != 2 || s.Owner != "w1" {
		t.Errorf("step = %+v, want the new claim untouched", s)
	}
}
