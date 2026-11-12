package queue

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
)

// The admin actions (actions.go). Their audit rows are admin action rows,
// so these tests use their own database helper instead of newDB.

var admin = Actor{Kind: "admin", ID: "test-admin"}

func actionDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.New(t)
}

func inTx(t *testing.T, pool *pgxpool.Pool, f func(tx pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginFunc(ctx, pool, f)
}

// actionRows returns the details of the audit rows of action for jobID.
func actionRows(t *testing.T, pool *pgxpool.Pool, jobID, action string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT coalesce(detail, '') FROM audit WHERE job_id = $1 AND action = $2
		ORDER BY audit_id`, jobID, action)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// toHeld runs a job up to step and holds it there with reason.
func toHeld(t *testing.T, e *Engine, id, step, reason string) {
	t.Helper()
	for _, s := range Steps[:indexOf(step)] {
		mustFinish(t, e, mustClaim(t, e, id, s), Result{Outcome: OK})
	}
	mustFinish(t, e, mustClaim(t, e, id, step), Result{Outcome: Hold, Reason: reason})
}

func indexOf(step string) int {
	for i, s := range Steps {
		if s == step {
			return i
		}
	}
	panic(step)
}

// toUncertain holds a job delivery_uncertain with its record in sending, as
// deliver does after a timeout (sinks.md §1.4).
func toUncertain(t *testing.T, pool *pgxpool.Pool, e *Engine, id string) {
	t.Helper()
	run := toDeliver(t, e, id)
	exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-test', 'sending')`, id)
	mustFinish(t, e, run, Result{Outcome: Hold, Reason: "delivery_uncertain"})
}

func TestRetryFailedJob(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	mustFinish(t, e, mustClaim(t, e, id, "validate"), Result{Outcome: OK})
	mustFinish(t, e, mustClaim(t, e, id, "normalize"), Result{Outcome: Permanent, Reason: "events_mismatch",
		ErrorClass: "input", ErrorCode: "x"})
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.State != "running" || j.Step != "normalize" || j.Failure != "" {
		t.Fatalf("job = %+v, want running at normalize", j)
	}
	if s := getStep(t, pool, id, "normalize"); s.State != "pending" || s.Attempt != 0 || s.DueIn > 0 {
		t.Fatalf("normalize = %+v, want pending, attempts reset, due now", s)
	}
	if s := getStep(t, pool, id, "validate"); s.State != "succeeded" {
		t.Errorf("validate = %+v, earlier output must be kept", s)
	}
	run := mustClaim(t, e, id, "normalize")
	if run.Attempt != 1 {
		t.Errorf("attempt %d, want 1", run.Attempt)
	}
	if got := actionRows(t, pool, id, "retry"); len(got) != 1 || got[0] != "running" {
		t.Errorf("retry audit rows %v", got)
	}
}

func TestRetryDeliveryUncertainDeletesSending(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	toUncertain(t, pool, e, id)
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Retry(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE job_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deliveries rows %d (%v), want the sending row deleted", n, err)
	}
	mustClaim(t, e, id, "deliver")
}

func TestReleaseDeliveryUncertain(t *testing.T) {
	for _, doc := range []string{"", "doc-test-1"} {
		t.Run("document_id="+doc, func(t *testing.T) {
			pool := actionDB(t)
			id := seedJob(t, pool)
			e := newEngine(pool, "w1", Steps...)
			toUncertain(t, pool, e, id)
			err := inTx(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, doc, 2*time.Hour) })
			if err != nil {
				t.Fatal(err)
			}
			if j := getJob(t, pool, id); j.State != "delivered" || j.Step != "cleanup" || j.Held != "" {
				t.Fatalf("job = %+v, want delivered at cleanup", j)
			}
			var state, gotDoc string
			var delivered bool
			if err := pool.QueryRow(ctx, `
				SELECT d.state, coalesce(d.document_id, ''), j.delivered_at IS NOT NULL
				FROM deliveries d JOIN jobs j USING (job_id) WHERE job_id = $1`, id).
				Scan(&state, &gotDoc, &delivered); err != nil {
				t.Fatal(err)
			}
			if state != "confirmed_by_admin" || gotDoc != doc || !delivered {
				t.Fatalf("delivery %s %q delivered_at %v", state, gotDoc, delivered)
			}
			if s := getStep(t, pool, id, "deliver"); s.State != "succeeded" {
				t.Errorf("deliver = %+v, want succeeded", s)
			}
			if s := getStep(t, pool, id, "cleanup"); s.DueIn < 2*time.Hour-5*time.Second || s.DueIn > 2*time.Hour {
				t.Errorf("cleanup due in %v, want the retention (2h)", s.DueIn)
			}
			mustNotClaim(t, e)
			if got := actionRows(t, pool, id, "release"); len(got) != 1 || got[0] != "delivered" {
				t.Errorf("release audit rows %v", got)
			}
		})
	}
}

func TestReleaseOtherHold(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	toHeld(t, e, id, "validate", "unknown_type")
	err := inTx(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "doc-1", 0) })
	if !errors.Is(err, ErrDocumentIDNotAllowed) {
		t.Fatalf("release with document_id: %v, want ErrDocumentIDNotAllowed", err)
	}
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Release(ctx, tx, admin, id, "", 0) }); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.State != "running" || j.Held != "" {
		t.Fatalf("job = %+v, want running", j)
	}
	mustClaim(t, e, id, "validate")
	if got := actionRows(t, pool, id, "release"); len(got) != 1 || got[0] != "running" {
		t.Errorf("release audit rows %v", got)
	}
	if got := actionRows(t, pool, id, "job_state"); len(got) != 2 { // running, held/unknown_type
		t.Errorf("job_state rows %v: release must not add one", got)
	}
}

// A purged held job runs only cleanup, under a real engine with a test
// cleanup handler, and ends cleaned/purged.
func TestPurgeHeldJobRunsCleanupOnly(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate")
	toHeld(t, e, id, "validate", "no_registration")
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Purge(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.State != "running" || j.Step != "cleanup" || j.Held != "" {
		t.Fatalf("job = %+v, want running at cleanup", j)
	}
	ran := make(chan string, 10)
	h := func(_ context.Context, run Run) Result {
		ran <- run.Step
		return Result{Outcome: OK}
	}
	w := New(pool, Config{Owner: "w2", Slots: 1, Log: slog.New(slog.DiscardHandler),
		Handlers: map[string]Handler{"validate": h, "normalize": h, "deliver": h, "cleanup": h}})
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(rctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for getJob(t, pool, id).State != "cleaned" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
	close(ran)
	var steps []string
	for s := range ran {
		steps = append(steps, s)
	}
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "purged" {
		t.Fatalf("job = %+v, want cleaned/purged", j)
	}
	if len(steps) != 1 || steps[0] != "cleanup" {
		t.Errorf("steps run %v, want cleanup only", steps)
	}
	if got := actionRows(t, pool, id, "purge"); len(got) != 1 {
		t.Errorf("purge audit rows %v", got)
	}
	if got := auditDetails(t, pool, id); got[len(got)-1] != "cleaned/purged" {
		t.Errorf("audit %v, want cleaned/purged last", got)
	}
}

// A purged job whose cleanup failed (attempts used up) can be purged again.
func TestPurgeAfterCleanupExhausted(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	toHeld(t, e, id, "validate", "unknown_sink")
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Purge(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	for range MaxAttempts("cleanup") {
		exec(t, pool, `UPDATE steps SET run_after = now() WHERE job_id = $1`, id)
		mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: Transient, ErrorClass: "blob"})
	}
	if j := getJob(t, pool, id); j.State != "failed" || j.Failure != "retries_exhausted" {
		t.Fatalf("job = %+v, want failed", j)
	}
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Purge(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.State != "cleaned" || j.Outcome != "purged" {
		t.Fatalf("job = %+v, want cleaned/purged", j)
	}
}

// A registration DELETE after a purge leaves the purge alone (Cancel is a
// no-op), so the job still ends purged.
func TestCancelOfPurgingJob(t *testing.T) {
	pool := actionDB(t)
	id := seedJob(t, pool)
	e := newEngine(pool, "w1", "validate", "cleanup")
	toHeld(t, e, id, "validate", "owner_unresolved")
	if err := inTx(t, pool, func(tx pgx.Tx) error { return Purge(ctx, tx, admin, id) }); err != nil {
		t.Fatal(err)
	}
	if err := cancel(t, pool, id); err != nil {
		t.Fatal(err)
	}
	if j := getJob(t, pool, id); j.CancelRequested {
		t.Fatalf("job = %+v, cancel must not be recorded", j)
	}
	mustFinish(t, e, mustClaim(t, e, id, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, id); j.Outcome != "purged" {
		t.Fatalf("job = %+v, want purged", j)
	}
}

// register inserts a registration for the job's record and joins the job,
// as validate does. It returns the registration_id.
func register(t *testing.T, pool *pgxpool.Pool, jobID string) int64 {
	t.Helper()
	var rid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status)
		SELECT record_id, 'fe-test', meeting_id, 'TEST-sub', 'test@example.org', 'registered'
		FROM packages p JOIN jobs j USING (package_id) WHERE j.job_id = $1
		RETURNING registration_id`, jobID).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE jobs SET registration_id = $2 WHERE job_id = $1`, jobID, rid)
	return rid
}

func registrationOf(t *testing.T, pool *pgxpool.Pool, jobID string) *int64 {
	t.Helper()
	var rid *int64
	if err := pool.QueryRow(ctx, `SELECT registration_id FROM jobs WHERE job_id = $1`, jobID).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

func rerun(t *testing.T, pool *pgxpool.Pool, id string) (string, error) {
	t.Helper()
	next := NewJobID()
	return next, inTx(t, pool, func(tx pgx.Tx) error {
		return Rerun(ctx, tx, admin, id, NewJob{JobID: next, ConfigHash: "hash-2"})
	})
}

func TestRerunTakesOverRegistration(t *testing.T) {
	pool := actionDB(t)
	old := seedJob(t, pool)
	rid := register(t, pool, old)
	e := newEngine(pool, "w1", Steps...)
	toHeld(t, e, old, "deliver", "owner_unresolved")
	next, err := rerun(t, pool, old)
	if err != nil {
		t.Fatal(err)
	}
	var rerunOf, pkgOld, pkgNew, hash, state, step string
	if err := pool.QueryRow(ctx, `
		SELECT n.rerun_of::text, o.package_id, n.package_id, n.config_hash, n.state, n.current_step
		FROM jobs n JOIN jobs o ON o.job_id = n.rerun_of WHERE n.job_id = $1`, next).
		Scan(&rerunOf, &pkgOld, &pkgNew, &hash, &state, &step); err != nil {
		t.Fatal(err)
	}
	if rerunOf != old || pkgNew != pkgOld || hash != "hash-2" || state != "received" || step != "validate" {
		t.Fatalf("new job rerun_of %s package %s hash %s %s at %s", rerunOf, pkgNew, hash, state, step)
	}
	if r := registrationOf(t, pool, next); r == nil || *r != rid {
		t.Fatalf("new job registration %v, want %d", r, rid)
	}
	if r := registrationOf(t, pool, old); r != nil {
		t.Fatalf("old job registration %v, want none", *r)
	}
	if j := getJob(t, pool, old); j.State != "running" || j.Step != "cleanup" {
		t.Fatalf("old job = %+v, want running at cleanup", j)
	}
	// Oldest job first: the old job's cleanup, then the new job's validate.
	mustFinish(t, e, mustClaim(t, e, old, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, old); j.State != "cleaned" || j.Outcome != "purged" {
		t.Fatalf("old job = %+v, want cleaned/purged", j)
	}
	var status, sub string
	if err := pool.QueryRow(ctx, `SELECT status, owner_sub FROM registrations WHERE registration_id = $1`, rid).
		Scan(&status, &sub); err != nil || status != "registered" || sub != "TEST-sub" {
		t.Fatalf("registration %s %s (%v), want unchanged", status, sub, err)
	}
	mustClaim(t, e, next, "validate")
	if got := actionRows(t, pool, old, "rerun"); len(got) != 1 || got[0] != next {
		t.Errorf("rerun audit rows %v, want [%s]", got, next)
	}
	if got := actionRows(t, pool, next, "job_state"); len(got) == 0 || got[0] != "received" {
		t.Errorf("new job audit rows %v", got)
	}
	// The old job is stopping or cleaned: no second rerun.
	if _, err := rerun(t, pool, old); !errors.Is(err, ErrInvalidState) {
		t.Errorf("rerun of a cleaned job: %v, want ErrInvalidState", err)
	}
}

// A rerun of a delivered job (retention > 0) purges the old job at once.
func TestRerunDeliveredJob(t *testing.T) {
	pool := actionDB(t)
	old := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	for _, s := range Steps[:7] {
		mustFinish(t, e, mustClaim(t, e, old, s), Result{Outcome: OK})
	}
	if _, err := rerun(t, pool, old); err != nil {
		t.Fatal(err)
	}
	if s := getStep(t, pool, old, "cleanup"); s.DueIn > 0 {
		t.Fatalf("cleanup due in %v, want now", s.DueIn)
	}
	mustFinish(t, e, mustClaim(t, e, old, "cleanup"), Result{Outcome: OK})
	if j := getJob(t, pool, old); j.State != "cleaned" || j.Outcome != "purged" {
		t.Fatalf("old job = %+v, want cleaned/purged", j)
	}
}

// A validate that is claimed while the rerun commits stops at its boundary,
// and its join of the registration is undone: the new job holds it.
func TestRerunDuringClaimedValidate(t *testing.T) {
	pool := actionDB(t)
	old := seedJob(t, pool)
	rid := register(t, pool, old)
	exec(t, pool, `UPDATE jobs SET registration_id = NULL WHERE job_id = $1`, old) // not joined yet
	e := newEngine(pool, "w1", Steps...)
	run := mustClaim(t, e, old, "validate")
	if _, err := rerun(t, pool, old); err != nil {
		t.Fatal(err)
	}
	join := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE jobs SET registration_id = $2 WHERE job_id = $1`, old, rid)
		return err
	}
	mustFinish(t, e, run, Result{Outcome: OK, Commit: join})
	if r := registrationOf(t, pool, old); r != nil {
		t.Fatalf("old job registration %d, want none", *r)
	}
	if j := getJob(t, pool, old); j.Step != "cleanup" {
		t.Fatalf("old job = %+v, want at cleanup", j)
	}
}

func TestRerunRollbackLeavesNothing(t *testing.T) {
	pool := actionDB(t)
	old := seedJob(t, pool)
	e := newEngine(pool, "w1", Steps...)
	toHeld(t, e, old, "validate", "unknown_type")
	boom := errors.New("boom")
	next := NewJobID()
	err := inTx(t, pool, func(tx pgx.Tx) error {
		if err := Rerun(ctx, tx, admin, old, NewJob{JobID: next, ConfigHash: "h"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE job_id = $1`, next).Scan(&n); err != nil || n != 0 {
		t.Fatalf("new job rows %d", n)
	}
	if j := getJob(t, pool, old); j.State != "held" {
		t.Fatalf("old job = %+v, want unchanged", j)
	}
	if got := actionRows(t, pool, old, "rerun"); len(got) != 0 {
		t.Fatalf("audit rows %v after rollback", got)
	}
}
