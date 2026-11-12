package deliver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/adapters/docs"
	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/render"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

var ctx = context.Background()

// Fixture owner (fixtures.md §5) and content; canaries for the logs and rows.
const (
	ownerSub   = "fixture-sub-ada-0001"
	ownerEmail = "ada.testperson@example.org"
	title      = "Fixture Widget Meeting 2025-10-29"
	summaryMD  = "## Overview\n\nThe team chose blue for the test widget.\n"
	profile    = "docs-main"
	key        = "FIXTURE-NOT-A-SECRET-deliver-test"
)

var canaries = []string{ownerSub, ownerEmail, title, "chose blue", key, "mock-docs:"}

// fakeDocs is an httptest Docs. Each request runs the next queued answer
// (or 201); it records, at request time, the job's deliveries row, so a
// test sees that sending was committed before the request.
type fakeDocs struct {
	t       *testing.T
	pool    *pgxpool.Pool
	srv     *httptest.Server
	mu      sync.Mutex
	answers []func(http.ResponseWriter, *http.Request)
	seen    []string // deliveries.state at request time, per request
	bodies  [][]byte
	ids     int

	inflight, maxInflight atomic.Int32
}

func newFakeDocs(t *testing.T, pool *pgxpool.Pool) *fakeDocs {
	f := &fakeDocs{t: t, pool: pool}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDocs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxInflight.Load()
		if n <= m || f.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	var answer func(http.ResponseWriter, *http.Request)
	if len(f.answers) > 0 {
		answer, f.answers = f.answers[0], f.answers[1:]
	}
	f.ids++
	id := "doc-" + string(rune('0'+f.ids))
	f.mu.Unlock()
	f.record(r)
	if answer != nil {
		answer(w, r)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"id": "`+id+`"}`)
}

// record notes the deliveries rows in sending at request time.
func (f *fakeDocs) record(r *http.Request) {
	var n int
	if err := f.pool.QueryRow(r.Context(), `SELECT count(*) FROM deliveries WHERE state = 'sending'`).Scan(&n); err != nil {
		f.t.Errorf("read deliveries: %v", err)
	}
	f.mu.Lock()
	f.seen = append(f.seen, strings.Repeat("sending,", n))
	f.mu.Unlock()
}

func (f *fakeDocs) queue(a ...func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, a...)
}

func (f *fakeDocs) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func status(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func drop(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// env is a database, a blob store, a fake Docs and the handler's deps.
type env struct {
	deps Deps
	docs *fakeDocs
	logs *bytes.Buffer
	mu   *sync.Mutex

	seeded int
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func newEnv(t *testing.T, maxInflight int, timeout time.Duration) *env {
	t.Helper()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{logs: &bytes.Buffer{}, mu: &sync.Mutex{}}
	log := slog.New(slog.NewJSONHandler(lockedWriter{e.mu, e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.docs = newFakeDocs(t, pool)
	e.deps = Deps{Pool: pool, Blobs: store, Log: log, Sinks: map[string]Sink{
		profile: {Sender: docs.New(profile, e.docs.srv.URL, key, timeout, log), MaxInflight: maxInflight},
	}}
	t.Cleanup(func() { e.expectNoCanary(t) })
	return e
}

// expectNoCanary: no owner data, title, content, key or response body in
// the log, the step outputs or the deliveries rows.
func (e *env) expectNoCanary(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	logs := e.logs.String()
	e.mu.Unlock()
	var rows string
	if err := e.deps.Pool.QueryRow(ctx, `
		SELECT coalesce(string_agg(s::text, ' '), '') || coalesce((SELECT string_agg(d::text, ' ') FROM deliveries d), '')
		FROM steps s`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	for _, c := range canaries {
		if strings.Contains(logs, c) {
			t.Errorf("log contains %q", c)
		}
		if strings.Contains(rows, c) {
			t.Errorf("steps or deliveries rows contain %q", c)
		}
	}
}

func (e *env) put(t *testing.T, jobID string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	k := hex.EncodeToString(sum[:])
	if err := e.deps.Blobs.Put(ctx, jobID, k, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.deps.Pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seed creates a job that render has finished: running at deliver, the
// render output and blobs, and a registration with the owner. The deliver
// step is pending.
func (e *env) seed(t *testing.T, md string) string {
	t.Helper()
	// A fresh record_id per job gives each its own package.
	m := validatetest.F01(t)
	e.seeded++
	m.RecordID += fmt.Sprintf("-%d", e.seeded)
	jobID := validatetest.Seed(t, e.deps.Pool, e.deps.Blobs, validatetest.Package{Manifest: m})
	var regID int64
	if err := e.deps.Pool.QueryRow(ctx, `
		INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status)
		VALUES ($1, 'pilos-fixture', $2, $3, $4, 'registered') RETURNING registration_id`,
		m.RecordID, m.MeetingID, ownerSub, ownerEmail).Scan(&regID); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(render.Output{SummarySHA256: e.put(t, jobID, []byte(md)),
		TranscriptSHA256: e.put(t, jobID, []byte("WEBVTT\n")), TitleSHA256: e.put(t, jobID, []byte(title)),
		FormatID: "meeting-summary", FormatVersion: "1.0.0", Language: "en"})
	e.exec(t, `UPDATE jobs SET state = 'running', current_step = 'deliver', registration_id = $2,
		meeting_type = 'meeting-summary', sink_profile = $3 WHERE job_id = $1`, jobID, regID, profile)
	e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name NOT IN ('deliver', 'cleanup')`, jobID)
	e.exec(t, `UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'render'`, jobID, out)
	return jobID
}

// claim claims the deliver step as the engine does and returns the run.
func (e *env) claim(t *testing.T, jobID string) queue.Run {
	t.Helper()
	run := queue.Run{JobID: jobID, Step: Step}
	if err := e.deps.Pool.QueryRow(ctx, `
		UPDATE steps SET state = 'claimed', owner = 'test', attempt = attempt + 1,
			lease_expires_at = now() + interval '60 seconds'
		WHERE job_id = $1 AND name = 'deliver' RETURNING attempt`, jobID).Scan(&run.Attempt); err != nil {
		t.Fatal(err)
	}
	return run
}

// complete runs the result's Commit in a transaction, as the completion
// does, and gives the step back.
func (e *env) complete(t *testing.T, run queue.Run, r queue.Result) {
	t.Helper()
	if r.Commit != nil {
		if err := pgx.BeginFunc(ctx, e.deps.Pool, func(tx pgx.Tx) error { return r.Commit(ctx, tx) }); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(t, `UPDATE steps SET state = 'pending', owner = NULL, lease_expires_at = NULL
		WHERE job_id = $1 AND name = 'deliver'`, run.JobID)
}

// row returns the deliveries row as "state document_id", or "".
func (e *env) row(t *testing.T, jobID string) string {
	t.Helper()
	var state string
	var doc *string
	err := e.deps.Pool.QueryRow(ctx, `SELECT state, document_id FROM deliveries WHERE job_id = $1`, jobID).Scan(&state, &doc)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	if doc != nil {
		return state + " " + *doc
	}
	return state
}

// Every row of the outcome table (contracts/sink/README.md), each with the
// deliveries row before the request, at request time and after the
// completion.
func TestOutcomes(t *testing.T) {
	for _, c := range []struct {
		name     string
		answer   func(http.ResponseWriter, *http.Request)
		md       string
		requests int
		outcome  queue.Outcome
		reason   string
		class    string
		after    string
	}{
		{"201", nil, summaryMD, 1, queue.OK, "", "", "sent doc-1"},
		{"429", status(429, `{"detail": "mock-docs: throttled"}`), summaryMD, 1, queue.Transient, "", "not_sent", ""},
		{"500", status(500, `{"detail": "mock-docs: boom"}`), summaryMD, 1, queue.Hold, HeldDeliveryUncertain, "uncertain", "sending"},
		{"503", status(503, ``), summaryMD, 1, queue.Hold, HeldDeliveryUncertain, "uncertain", "sending"},
		{"drop", drop, summaryMD, 1, queue.Hold, HeldDeliveryUncertain, "uncertain", "sending"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, summaryMD, 1, queue.Hold,
			HeldDeliveryUncertain, "uncertain", "sending"},
		{"401", status(401, `{"detail": "mock-docs: bad token"}`), summaryMD, 1, queue.Permanent, ReasonConfigError, "config", ""},
		{"403", status(403, `{"detail": "mock-docs: forbidden"}`), summaryMD, 1, queue.Permanent, ReasonConfigError, "config", ""},
		{"400-email", status(400, `{"email": ["mock-docs: two users"]}`), summaryMD, 1, queue.Hold, HeldOwnerUnresolved,
			"recipient_unresolved", ""},
		{"400-content", status(400, `{"content": ["mock-docs: Could not convert content"]}`), summaryMD, 1,
			queue.Permanent, ReasonRejectedBySink, "rejected", ""},
		{"404", status(404, `{"detail": "mock-docs: not found"}`), summaryMD, 1, queue.Permanent, ReasonRejectedBySink,
			"rejected", ""},
		{"pre-check", nil, summaryMD + "\n<div>x</div>\n", 0, queue.Permanent, ReasonRejectedBySink, "rejected", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 0, 300*time.Millisecond)
			if c.answer != nil {
				e.docs.queue(c.answer)
			}
			jobID := e.seed(t, c.md)
			if got := e.row(t, jobID); got != "" {
				t.Fatalf("row before: %q", got)
			}
			run := e.claim(t, jobID)
			r := Handler(e.deps)(ctx, run)
			if e.docs.requests() != c.requests {
				t.Fatalf("%d requests, want %d", e.docs.requests(), c.requests)
			}
			if c.requests == 1 && e.docs.seen[0] != "sending," {
				t.Errorf("deliveries at request time: %q, want one row in sending", e.docs.seen[0])
			}
			if r.Outcome != c.outcome || r.Reason != c.reason || r.ErrorClass != c.class {
				t.Fatalf("result %+v, want %s %s %s", r, c.outcome, c.reason, c.class)
			}
			e.complete(t, run, r)
			if got := e.row(t, jobID); got != c.after {
				t.Errorf("row after: %q, want %q", got, c.after)
			}
			if c.outcome == queue.OK && string(r.Output) != `{"delivery_state":"sent","document_id":"doc-1"}` {
				t.Errorf("output %s", r.Output)
			}
			// The next attempt sends again only where the row was deleted.
			run = e.claim(t, jobID)
			r = Handler(e.deps)(ctx, run)
			want := c.requests
			if c.after == "" && c.requests == 1 {
				want++
			}
			if e.docs.requests() != want {
				t.Errorf("second attempt: %d requests, want %d", e.docs.requests(), want)
			}
			if c.after == "sending" && (r.Outcome != queue.Hold || r.Reason != HeldDeliveryUncertain) {
				t.Errorf("second attempt: %+v, want hold delivery_uncertain", r)
			}
		})
	}
}

// The request carries the registration's owner, the title and the
// summary unchanged.
func TestRequestFromRegistrationAndRender(t *testing.T) {
	e := newEnv(t, 0, 0)
	jobID := e.seed(t, summaryMD)
	Handler(e.deps)(ctx, e.claim(t, jobID))
	var body map[string]string
	if err := json.Unmarshal(e.docs.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"title": title, "content": summaryMD, "sub": ownerSub, "email": ownerEmail}
	if len(body) != 4 {
		t.Errorf("body has %d fields", len(body))
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s differs", k)
		}
	}
}

// A crash after the sending commit: the handler's context ends while the
// request is in flight. The result is dropped; the next run sends nothing
// and holds delivery_uncertain.
func TestCrashAfterSending(t *testing.T) {
	e := newEnv(t, 0, 0)
	arrived := make(chan struct{})
	e.docs.queue(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})
	jobID := e.seed(t, summaryMD)
	run := e.claim(t, jobID)
	hctx, cancel := context.WithCancel(ctx)
	go func() { <-arrived; cancel() }()
	Handler(e.deps)(hctx, run) // dropped by the engine: no completion
	if got := e.row(t, jobID); got != "sending" {
		t.Fatalf("row after the crash: %q, want sending", got)
	}
	// The engine released or reaped the step; a new run claims it.
	e.exec(t, `UPDATE steps SET state = 'retrying', owner = NULL WHERE job_id = $1 AND name = 'deliver'`, jobID)
	r := Handler(e.deps)(ctx, e.claim(t, jobID))
	if r.Outcome != queue.Hold || r.Reason != HeldDeliveryUncertain || e.docs.requests() != 1 {
		t.Fatalf("re-entry: %+v after %d requests, want hold delivery_uncertain after 1", r, e.docs.requests())
	}
}

// A step that is no longer claimed by this run (released, reaped, or
// claimed again) never sends and writes no row.
func TestNotClaimedNeverSends(t *testing.T) {
	e := newEnv(t, 0, 0)
	jobID := e.seed(t, summaryMD)
	run := e.claim(t, jobID)
	for name, sql := range map[string]string{
		"released":      `UPDATE steps SET state = 'pending', owner = NULL, lease_expires_at = NULL WHERE job_id = $1 AND name = 'deliver'`,
		"lease expired": `UPDATE steps SET state = 'claimed', lease_expires_at = now() - interval '1 second' WHERE job_id = $1 AND name = 'deliver'`,
		"later attempt": `UPDATE steps SET lease_expires_at = now() + interval '60 seconds', attempt = attempt + 1 WHERE job_id = $1 AND name = 'deliver'`,
	} {
		e.exec(t, sql, jobID)
		r := Handler(e.deps)(ctx, run)
		if r.Outcome != queue.Transient || r.ErrorCode != "not_claimed" || e.docs.requests() != 0 || e.row(t, jobID) != "" {
			t.Errorf("%s: %+v, %d requests, row %q", name, r, e.docs.requests(), e.row(t, jobID))
		}
	}
}

// A job that is being purged or that a rerun replaced does not send.
func TestStoppingJobNeverSends(t *testing.T) {
	e := newEnv(t, 0, 0)
	for name, sql := range map[string]string{
		"purge":     `UPDATE jobs SET purge_requested_at = now() WHERE job_id = $1`,
		"cancel":    `UPDATE jobs SET cancel_requested_at = now() WHERE job_id = $1`,
		"rerun":     `UPDATE jobs SET registration_id = NULL WHERE job_id = $1`,
		"tombstone": `UPDATE registrations SET status = 'cancelled', owner_sub = NULL, owner_email = NULL WHERE registration_id = (SELECT registration_id FROM jobs WHERE job_id = $1)`,
	} {
		jobID := e.seed(t, summaryMD)
		run := e.claim(t, jobID)
		e.exec(t, sql, jobID)
		r := Handler(e.deps)(ctx, run)
		if r.Outcome == queue.OK || e.docs.requests() != 0 || e.row(t, jobID) != "" {
			t.Errorf("%s: %+v, %d requests, row %q", name, r, e.docs.requests(), e.row(t, jobID))
		}
	}
}

// max_inflight 1: two jobs at once never have two requests in flight.
func TestMaxInflight(t *testing.T) {
	e := newEnv(t, 1, 0)
	slow := func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": "doc-slow"}`)
	}
	e.docs.queue(slow, slow)
	var wg sync.WaitGroup
	h := Handler(e.deps)
	for range 2 {
		run := e.claim(t, e.seed(t, summaryMD))
		wg.Go(func() {
			if r := h(ctx, run); r.Outcome != queue.OK {
				t.Errorf("result %+v", r)
			}
		})
	}
	wg.Wait()
	if e.docs.requests() != 2 || e.docs.maxInflight.Load() != 1 {
		t.Fatalf("%d requests, at most %d in flight; want 2 and 1", e.docs.requests(), e.docs.maxInflight.Load())
	}
}

// End to end with the queue engine: two workers and three jobs. Each job
// is delivered with exactly one request, the row is sent with the
// document ID and the job is delivered with cleanup scheduled.
func TestEngineConcurrentWorkers(t *testing.T) {
	e := newEnv(t, 0, 0)
	var jobs []string
	for range 3 {
		jobID := e.seed(t, summaryMD)
		e.exec(t, `UPDATE steps SET state = 'pending', run_after = now() WHERE job_id = $1 AND name = 'deliver'`, jobID)
		jobs = append(jobs, jobID)
	}
	runEngines(t, e, 2, func() bool { return delivered(t, e, jobs) == len(jobs) })
	if n := e.docs.requests(); n != len(jobs) {
		t.Fatalf("%d requests for %d jobs", n, len(jobs))
	}
	for _, j := range jobs {
		if got := e.row(t, j); !strings.HasPrefix(got, "sent doc-") {
			t.Errorf("job %s row %q", j, got)
		}
	}
}

// A worker shutdown while the request is in flight: the step is cancelled
// and released; the next claim holds delivery_uncertain without sending
// (architecture.md §9).
func TestEngineShutdownDuringSend(t *testing.T) {
	e := newEnv(t, 0, 0)
	arrived := make(chan struct{})
	e.docs.queue(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})
	jobID := e.seed(t, summaryMD)
	e.exec(t, `UPDATE steps SET state = 'pending', run_after = now() WHERE job_id = $1 AND name = 'deliver'`, jobID)
	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine(e, "w1").Run(wctx)
	}()
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("no request")
	}
	stop()
	<-done
	if got := e.row(t, jobID); got != "sending" {
		t.Fatalf("row after shutdown: %q", got)
	}
	runEngines(t, e, 1, func() bool { return jobState(t, e, jobID) == "held/delivery_uncertain" })
	if e.docs.requests() != 1 {
		t.Fatalf("%d requests, want 1", e.docs.requests())
	}
}

func engine(e *env, owner string) *queue.Engine {
	return queue.New(e.deps.Pool, queue.Config{Owner: owner, Slots: 2, Log: e.deps.Log, Drain: 100 * time.Millisecond,
		RetentionAfterDelivery: time.Hour, Handlers: map[string]queue.Handler{Step: Handler(e.deps)}})
}

// runEngines runs n engines until done reports true (at most 30 s).
func runEngines(t *testing.T, e *env, n int, done func() bool) {
	t.Helper()
	wctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { engine(e, "w"+string(rune('a'+i))).Run(wctx) })
	}
	deadline := time.Now().Add(30 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	wg.Wait()
	if !done() {
		t.Fatal("engines did not finish")
	}
}

func delivered(t *testing.T, e *env, jobs []string) int {
	n := 0
	for _, j := range jobs {
		if jobState(t, e, j) == "delivered" {
			n++
		}
	}
	return n
}

func jobState(t *testing.T, e *env, jobID string) string {
	t.Helper()
	var state string
	var held *string
	if err := e.deps.Pool.QueryRow(ctx, `SELECT state, held_reason FROM jobs WHERE job_id = $1`, jobID).Scan(&state, &held); err != nil {
		t.Fatal(err)
	}
	if held != nil {
		return state + "/" + *held
	}
	return state
}
