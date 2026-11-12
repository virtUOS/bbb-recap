package validate_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/validate"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

var ctx = context.Background()

type env struct {
	pool  *pgxpool.Pool
	store blob.Store
	deps  validate.Deps
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &env{pool: pool, store: store, deps: validate.Deps{
		Pool:       pool,
		Blobs:      store,
		Log:        slog.New(slog.DiscardHandler),
		MetaPrefix: "recap",
		Configured: validate.Configured{MeetingTypes: []string{"meeting-summary"}, Sinks: []string{"docs-main"}},
		FrontendServers: []config.FrontendServer{
			{FrontendID: "pilos-fixture", BBBServerID: "bbb-fixture-a"},
		},
	}}
}

func (e *env) run(t *testing.T, jobID string) queue.Result {
	t.Helper()
	return validate.Handler(e.deps)(ctx, queue.Run{JobID: jobID, Step: "validate", Attempt: 1})
}

// complete runs the result's Commit the way the queue's completion does:
// in a transaction that has locked the job row first.
func (e *env) complete(t *testing.T, jobID string, r queue.Result) error {
	t.Helper()
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
			return err
		}
		if r.Commit == nil {
			return nil
		}
		return r.Commit(ctx, tx)
	})
}

type jobCols struct {
	MeetingType, SinkProfile *string
	RegistrationID           *int64
}

func (e *env) jobCols(t *testing.T, jobID string) jobCols {
	t.Helper()
	var c jobCols
	if err := e.pool.QueryRow(ctx, `SELECT meeting_type, sink_profile, registration_id FROM jobs WHERE job_id = $1`,
		jobID).Scan(&c.MeetingType, &c.SinkProfile, &c.RegistrationID); err != nil {
		t.Fatal(err)
	}
	return c
}

func codes(r queue.Result) [5]any {
	return [5]any{r.Outcome, r.Reason, r.Detail, r.Cancel, string(r.Output)}
}

const (
	f01Record  = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	f01Meeting = "fixture-meeting-01"
)

func TestValidateOKSetsJobColumns(t *testing.T) {
	e := newEnv(t)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
	regID := validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "registered")

	r := e.run(t, jobID)
	if r.Outcome != queue.OK || r.Cancel || r.Commit == nil {
		t.Fatalf("result %+v", codes(r))
	}
	if err := e.complete(t, jobID, r); err != nil {
		t.Fatal(err)
	}
	c := e.jobCols(t, jobID)
	if c.MeetingType == nil || *c.MeetingType != "meeting-summary" || c.SinkProfile == nil || *c.SinkProfile != "docs-main" ||
		c.RegistrationID == nil || *c.RegistrationID != regID {
		t.Fatalf("job columns %+v", c)
	}

	// A repeat gives the same result and the same columns.
	again := e.run(t, jobID)
	if codes(again) != codes(r) {
		t.Fatalf("repeat: %v, first %v", codes(again), codes(r))
	}
	if err := e.complete(t, jobID, again); err != nil {
		t.Fatal(err)
	}
	if c2 := e.jobCols(t, jobID); *c2.RegistrationID != regID || *c2.MeetingType != *c.MeetingType {
		t.Fatalf("repeat columns %+v", c2)
	}
}

func TestValidateTombstoneCancels(t *testing.T) {
	e := newEnv(t)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
	validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "cancelled")
	r := e.run(t, jobID)
	if r.Outcome != queue.OK || !r.Cancel {
		t.Fatalf("result %v, want ok with cancel", codes(r))
	}
	if err := e.complete(t, jobID, r); err != nil {
		t.Fatal(err)
	}
	if c := e.jobCols(t, jobID); c.RegistrationID != nil {
		t.Fatalf("tombstone joined: %+v", c)
	}
}

func TestValidateHolds(t *testing.T) {
	cases := []struct {
		name     string
		server   string // package server; default bbb-fixture-a
		register bool
		meeting  string
		noTypes  bool
		reason   string
		detail   string
	}{
		{name: "no-registration", reason: "no_registration", detail: "no-registration"},
		{name: "server-not-allowed", server: "bbb-fixture-b", register: true, meeting: "other-meeting",
			reason: "no_registration", detail: "server-not-allowed"},
		{name: "meeting-id-mismatch", register: true, meeting: "other-meeting",
			reason: "no_registration", detail: "meeting-id-mismatch"},
		{name: "unknown-type-before-registration", noTypes: true, reason: "unknown_type", detail: "type-not-configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.noTypes {
				e.deps.Configured.MeetingTypes = nil
			}
			jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{BBBServerID: c.server})
			if c.register {
				validatetest.Register(t, e.pool, f01Record, "pilos-fixture", c.meeting, "registered")
			}
			r := e.run(t, jobID)
			if r.Outcome != queue.Hold || r.Reason != c.reason || r.Detail != c.detail {
				t.Fatalf("result %v, want hold %s/%s", codes(r), c.reason, c.detail)
			}
			if err := e.complete(t, jobID, r); err != nil {
				t.Fatal(err)
			}
			if cols := e.jobCols(t, jobID); cols.RegistrationID != nil || cols.MeetingType != nil {
				t.Fatalf("held job joined: %+v", cols)
			}
		})
	}
}

// TestValidateWithoutServerMapping: with RECAP_FRONTEND_SERVERS unset, a
// package of a server that no mapping names joins its registration.
func TestValidateWithoutServerMapping(t *testing.T) {
	e := newEnv(t)
	e.deps.FrontendServers = nil
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{BBBServerID: "bbb-fixture-b"})
	regID := validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "registered")
	r := e.run(t, jobID)
	if r.Outcome != queue.OK || r.Cancel {
		t.Fatalf("result %v, want ok", codes(r))
	}
	if err := e.complete(t, jobID, r); err != nil {
		t.Fatal(err)
	}
	if c := e.jobCols(t, jobID); c.RegistrationID == nil || *c.RegistrationID != regID {
		t.Fatalf("job columns %+v, want registration %d", c, regID)
	}
}

// copyF01 copies F-01's package files into a temp dir, renaming paths in
// rename (old → new).
func copyF01(t *testing.T, m *manifest.Manifest, rename map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for i, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(validatetest.F01Dir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if to, ok := rename[f.Path]; ok {
			m.Files[i].Path = to
		}
		p := filepath.Join(dir, filepath.FromSlash(m.Files[i].Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidatePackageRules(t *testing.T) {
	const a1 = "audio/microphone-w_fixada0001-TR_fixA1-1761732005200.webm"
	cases := []struct {
		name   string
		mutate func(m *manifest.Manifest) map[string]string
		want   [3]any // outcome, reason, detail
	}{
		{"F-01", func(*manifest.Manifest) map[string]string { return nil }, [3]any{queue.OK, "", ""}},
		{"M-09", func(m *manifest.Manifest) map[string]string { m.Metadata["recap-sink"] = "docs-m09"; return nil },
			[3]any{queue.Permanent, "manifest_metadata_mismatch", "V-S04"}},
		{"M-10", func(m *manifest.Manifest) map[string]string { m.MeetingID = "fixture-meeting-m10"; return nil },
			[3]any{queue.Permanent, "events_mismatch", "V-S01"}},
		{"M-11", func(*manifest.Manifest) map[string]string {
			return map[string]string{a1: "audio/microphone-w_fixada0001-TR_fixM11-1761732005200.webm"}
		}, [3]any{queue.OK, "", ""}},
		{"none-matches", func(m *manifest.Manifest) map[string]string {
			r := map[string]string{}
			for _, f := range m.Files {
				if f.Role == manifest.RoleMicrophone {
					r[f.Path] = f.Path[:len("audio/microphone-w_fixada0001-")] + "TR_nomatch" + f.Path[len("audio/microphone-w_fixada0001-TR_fix"):]
				}
			}
			return r
		}, [3]any{queue.Permanent, "all_tracks_failed", "V-S05"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			m := validatetest.F01(t)
			rename := c.mutate(m)
			dir := copyF01(t, m, rename)
			sortFiles(m)
			jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{Dir: dir, Manifest: m})
			validatetest.Register(t, e.pool, m.RecordID, "pilos-fixture", m.MeetingID, "registered")
			r := e.run(t, jobID)
			if got := [3]any{r.Outcome, r.Reason, r.Detail}; got != c.want {
				t.Fatalf("result %v, want %v", got, c.want)
			}
		})
	}
}

func sortFiles(m *manifest.Manifest) {
	for i := 1; i < len(m.Files); i++ {
		for j := i; j > 0 && m.Files[j].Path < m.Files[j-1].Path; j-- {
			m.Files[j], m.Files[j-1] = m.Files[j-1], m.Files[j]
		}
	}
}

// A registration committed between the handler's read and the completion
// is seen by the commit: the completion rolls back and the step runs again.
func TestValidateCommitSeesNewRegistration(t *testing.T) {
	e := newEnv(t)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
	r := e.run(t, jobID)
	if r.Outcome != queue.Hold || r.Reason != "no_registration" {
		t.Fatalf("result %v", codes(r))
	}
	validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "registered")
	if err := e.complete(t, jobID, r); !errors.Is(err, validate.ErrDecisionChanged) {
		t.Fatalf("complete: %v, want ErrDecisionChanged", err)
	}
	if r := e.run(t, jobID); r.Outcome != queue.OK {
		t.Fatalf("rerun: %v", codes(r))
	}
}

// A registration PUT concurrent with the completion of a no_registration
// hold, in the lock order of architecture.md §6.2 (registration first, then
// the job row). Two shapes of PUT:
//   - lockJob: the PUT locks the job row before ReleaseHeld. It waits for
//     the completion, then finds the job held and releases it.
//   - plain ReleaseHeld: its UPDATE … WHERE state = 'held' does not wait for
//     the job lock (the visible row is running), so the PUT commits first;
//     the commit's second decision sees the registration and rolls back,
//     and the step runs again.
//
// Either way nothing deadlocks and the job does not stay held.
func TestValidateConcurrentRegistrationPut(t *testing.T) {
	for _, lockJob := range []bool{true, false} {
		name := "plain-release"
		if lockJob {
			name = "put-locks-job"
		}
		t.Run(name, func(t *testing.T) { testConcurrentPut(t, lockJob) })
	}
}

func testConcurrentPut(t *testing.T, lockJob bool) {
	e := newEnv(t)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
	r := e.run(t, jobID)
	if r.Outcome != queue.Hold || r.Reason != "no_registration" {
		t.Fatalf("result %v", codes(r))
	}

	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	completion, err := e.pool.Begin(tctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = completion.Rollback(ctx) }()
	if _, err := completion.Exec(tctx, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
		t.Fatal(err)
	}

	inserted := make(chan struct{})
	putDone := make(chan bool, 1) // whether the PUT released the job
	putErr := make(chan error, 1)
	go func() {
		var ok bool
		err := pgx.BeginFunc(tctx, e.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(tctx, `
				INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status)
				VALUES ($1, 'pilos-fixture', $2, 'fixture-sub-ada-0001', 'ada.testperson@example.org', 'registered')`,
				f01Record, f01Meeting); err != nil {
				return err
			}
			close(inserted)
			if lockJob {
				if _, err := tx.Exec(tctx, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
					return err
				}
			}
			var err error
			ok, err = queue.ReleaseHeld(tctx, tx, queue.Actor{Kind: "client", ID: "pilos-fixture"}, jobID,
				"no_registration", "owner_unresolved")
			return err
		})
		putErr <- err
		putDone <- ok
	}()
	<-inserted

	if !lockJob {
		// The PUT does not wait: let it commit before the completion's commit.
		if err := <-putErr; err != nil {
			t.Fatal(err)
		}
		if <-putDone {
			t.Fatal("PUT released a job that was not held yet")
		}
		if err := r.Commit(tctx, completion); !errors.Is(err, validate.ErrDecisionChanged) {
			t.Fatalf("commit: %v, want ErrDecisionChanged", err)
		}
		_ = completion.Rollback(ctx)
		if r := e.run(t, jobID); r.Outcome != queue.OK {
			t.Fatalf("validate again: %v", codes(r))
		}
		return
	}

	time.Sleep(200 * time.Millisecond) // let the PUT block on the job row
	// The PUT is not committed: the decision holds and the engine holds the job.
	if err := r.Commit(tctx, completion); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := completion.Exec(tctx, `
		UPDATE jobs SET state = 'held', held_reason = 'no_registration', detail = 'no-registration' WHERE job_id = $1`,
		jobID); err != nil {
		t.Fatal(err)
	}
	if err := completion.Commit(tctx); err != nil {
		t.Fatal(err)
	}
	if err := <-putErr; err != nil {
		t.Fatal(err)
	}
	if !<-putDone {
		t.Fatal("PUT did not release the held job")
	}
	var state string
	if err := e.pool.QueryRow(ctx, `SELECT state FROM jobs WHERE job_id = $1`, jobID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("job state %q (%v), want running", state, err)
	}
	if r := e.run(t, jobID); r.Outcome != queue.OK {
		t.Fatalf("released validate: %v", codes(r))
	}
}

// End to end through the queue engine: validate succeeds, the job moves on
// to normalize with its columns set.
func TestValidateThroughEngine(t *testing.T) {
	e := newEnv(t)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
	regID := validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "registered")

	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		queue.New(e.pool, queue.Config{
			Owner: "t016", Slots: 1, Handlers: map[string]queue.Handler{"validate": validate.Handler(e.deps)},
			Log: slog.New(slog.DiscardHandler),
		}).Run(rctx)
		close(done)
	}()
	defer func() { stop(); <-done }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var step, state string
		if err := e.pool.QueryRow(ctx, `SELECT current_step, state FROM jobs WHERE job_id = $1`, jobID).Scan(&step, &state); err != nil {
			t.Fatal(err)
		}
		if step == "normalize" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still at %s/%s", step, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c := e.jobCols(t, jobID); c.RegistrationID == nil || *c.RegistrationID != regID {
		t.Fatalf("columns %+v", c)
	}
}

// runEngine runs the queue engine with the validate handler of deps until
// the job's (current_step, state, held_reason, detail) satisfies done.
func (e *env) runEngine(t *testing.T, deps validate.Deps, jobID string, done func(step, state, reason, detail string) bool) {
	t.Helper()
	rctx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		queue.New(e.pool, queue.Config{
			Owner: "t037", Slots: 1, Handlers: map[string]queue.Handler{"validate": validate.Handler(deps)},
			Log: slog.New(slog.DiscardHandler),
		}).Run(rctx)
		close(finished)
	}()
	defer func() { stop(); <-finished }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var step, state, reason, detail string
		if err := e.pool.QueryRow(ctx, `
			SELECT current_step, state, coalesce(held_reason, ''), coalesce(detail, '') FROM jobs WHERE job_id = $1`,
			jobID).Scan(&step, &state, &reason, &detail); err != nil {
			t.Fatal(err)
		}
		if done(step, state, reason, detail) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job at %s/%s (%s/%s)", step, state, reason, detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Row 9a (metadata-interface.md §3): a front end whose allow-list does not
// name the requested type or sink is held at validate, before the job is
// joined, so deliver (row 10) never runs for it. After the admin fixes the
// configuration, release runs validate again and the job proceeds.
func TestValidateAllowListHoldThenRelease(t *testing.T) {
	cases := []struct {
		name           string
		types, sinks   []config.FrontendName
		reason, detail string
	}{
		{"type-not-allowed", []config.FrontendName{{FrontendID: "pilos-fixture", Name: "lecture"}},
			[]config.FrontendName{{FrontendID: "pilos-fixture", Name: "docs-other"}}, "unknown_type", "type-not-allowed"},
		{"sink-not-allowed", nil,
			[]config.FrontendName{{FrontendID: "pilos-fixture", Name: "docs-other"}}, "unknown_sink", "sink-not-allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.deps.Configured.MeetingTypes = append(e.deps.Configured.MeetingTypes, "lecture")
			e.deps.Configured.Sinks = append(e.deps.Configured.Sinks, "docs-other")
			jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{})
			regID := validatetest.Register(t, e.pool, f01Record, "pilos-fixture", f01Meeting, "registered")

			restricted := e.deps
			restricted.Configured.FrontendTypes, restricted.Configured.FrontendSinks = c.types, c.sinks
			e.runEngine(t, restricted, jobID, func(step, state, reason, detail string) bool {
				if state != "held" {
					return false
				}
				if step != "validate" || reason != c.reason || detail != c.detail {
					t.Fatalf("held at %s with %s/%s, want validate with %s/%s", step, reason, detail, c.reason, c.detail)
				}
				return true
			})
			if cols := e.jobCols(t, jobID); cols.RegistrationID != nil || cols.MeetingType != nil {
				t.Fatalf("held job joined: %+v", cols)
			}

			// The admin adds the requested names to the lists and releases.
			fixed := e.deps
			fixed.Configured.FrontendTypes = append(c.types, config.FrontendName{FrontendID: "pilos-fixture", Name: "meeting-summary"})
			fixed.Configured.FrontendSinks = append(c.sinks, config.FrontendName{FrontendID: "pilos-fixture", Name: "docs-main"})
			if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
				return queue.Release(ctx, tx, queue.Actor{Kind: "admin", ID: "fixture-admin"}, jobID, "", 0)
			}); err != nil {
				t.Fatalf("release: %v", err)
			}
			e.runEngine(t, fixed, jobID, func(step, state, _, _ string) bool { return step == "normalize" })
			if cols := e.jobCols(t, jobID); cols.RegistrationID == nil || *cols.RegistrationID != regID ||
				*cols.MeetingType != "meeting-summary" || *cols.SinkProfile != "docs-main" {
				t.Fatalf("released job columns %+v", cols)
			}
		})
	}
}
