package ops

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

var discard = slog.New(slog.DiscardHandler)

// get sends GET path to h and returns the status and body.
func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// clock is a settable time for the readiness cache.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newServer(stopping <-chan struct{}) (*Server, *clock) {
	s := New(discard, prometheus.NewRegistry(), stopping)
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s.now = c.now
	return s, c
}

// Before the database is open: alive, not ready.
func TestStarting(t *testing.T) {
	s, _ := newServer(nil)
	h := s.Handler()
	if code, body := get(t, h, "/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("/healthz = %d %q", code, body)
	}
	if code, body := get(t, h, "/readyz"); code != 503 || body != "starting\n" {
		t.Errorf("/readyz = %d %q, want 503 starting", code, body)
	}
}

// The check result is reused for 5 s.
func TestReadyCache(t *testing.T) {
	s, c := newServer(nil)
	calls, result := 0, NotReadyPostgres
	s.SetCheck(func(context.Context) string { calls++; return result })
	h := s.Handler()

	if code, body := get(t, h, "/readyz"); code != 503 || body != "postgres\n" {
		t.Fatalf("/readyz = %d %q, want 503 postgres", code, body)
	}
	result = ""
	c.t = c.t.Add(ReadyCacheTime - time.Millisecond)
	if code, _ := get(t, h, "/readyz"); code != 503 || calls != 1 {
		t.Fatalf("within 5 s: /readyz = %d after %d checks, want the cached 503 after 1", code, calls)
	}
	c.t = c.t.Add(time.Millisecond)
	if code, body := get(t, h, "/readyz"); code != 200 || body != "ready\n" || calls != 2 {
		t.Fatalf("after 5 s: /readyz = %d %q after %d checks, want 200 after 2", code, body, calls)
	}
}

// On the shutdown signal /readyz answers 503 at once, also with a cached
// 200, while /healthz still answers 200 (architecture.md §9). The listener
// itself keeps serving until Close.
func TestStoppingOverListener(t *testing.T) {
	ctx, signal := context.WithCancel(context.Background())
	s, _ := newServer(ctx.Done())
	s.SetCheck(func(context.Context) string { return "" })
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := "http://" + addr.String()
	fetch := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := fetch("/readyz"); code != 200 {
		t.Fatalf("/readyz before the signal = %d", code)
	}
	signal()
	if code, body := fetch("/readyz"); code != 503 || body != "stopping\n" {
		t.Errorf("/readyz right after the signal = %d %q, want 503 stopping", code, body)
	}
	if code, _ := fetch("/healthz"); code != 200 {
		t.Errorf("/healthz after the signal = %d, want 200", code)
	}
	if code, _ := fetch("/metrics"); code != 200 {
		t.Errorf("/metrics after the signal = %d, want 200", code)
	}

	s.Close()
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Error("listener still answers after Close")
	}
}

func TestProbe(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	if err := Probe(context.Background(), srv.URL); err != nil {
		t.Errorf("200: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := Probe(context.Background(), srv.URL); err == nil {
		t.Error("503: no error")
	}
	srv.Close()
	if err := Probe(context.Background(), srv.URL); err == nil {
		t.Error("closed: no error")
	}
}

// The budgets of architecture.md §9 add up to 25 s: the api drains its
// public listener, the worker its steps, then the pool is closed.
func TestShutdownBudget(t *testing.T) {
	if ShutdownBudget != 25*time.Second {
		t.Errorf("ShutdownBudget = %v", ShutdownBudget)
	}
	if got := httpapi.ShutdownTimeout + PoolCloseTime; got != ShutdownBudget {
		t.Errorf("api: drain %v + pool close %v = %v, want %v", httpapi.ShutdownTimeout, PoolCloseTime, got,
			ShutdownBudget)
	}
	if drain := ShutdownBudget - PoolCloseTime - queue.StopTime; drain != queue.DefaultDrain {
		t.Errorf("worker drain = %v, want queue.DefaultDrain %v", drain, queue.DefaultDrain)
	}
}

// failingBlobs is a blob store whose Ping fails.
type failingBlobs struct{ blob.Store }

func (failingBlobs) Ping(context.Context) error { return blob.ErrTransient }

// The dependency check: database unreachable, schema older, blob store
// down, then ready; recap_dependency_up follows.
func TestDependencies(t *testing.T) {
	ctx := context.Background()
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewWorkerRegistry()
	up := func(dep string) float64 { return metricstest.Value(t, reg, "recap_dependency_up", "dependency", dep) }

	// Nothing listens on port 1; the pool connects lazily.
	down, err := pgxpool.New(ctx, "postgres://recap@127.0.0.1:1/recap?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	if got := Dependencies(down, fs)(ctx); got != NotReadyPostgres {
		t.Errorf("database unreachable: %q, want postgres", got)
	}
	if up("postgres") != 0 || up("blobstore") != 1 {
		t.Errorf("dependency_up postgres=%v blobstore=%v, want 0, 1", up("postgres"), up("blobstore"))
	}

	pool := dbtest.Open(t, dbtest.Empty(t)) // empty: schema older than the binary
	s, c := newServer(nil)
	s.SetCheck(Dependencies(pool, fs))
	if got := s.Ready(ctx); got != NotReadySchema {
		t.Errorf("empty database: %q, want schema", got)
	}
	if err := db.Migrate(ctx, pool, discard); err != nil {
		t.Fatal(err)
	}
	if got := s.Ready(ctx); got != NotReadySchema {
		t.Errorf("within the cache time: %q, want the cached schema", got)
	}
	c.t = c.t.Add(ReadyCacheTime)
	if got := s.Ready(ctx); got != "" {
		t.Errorf("migrated: %q, want ready", got)
	}
	if up("postgres") != 1 || up("blobstore") != 1 {
		t.Errorf("dependency_up postgres=%v blobstore=%v, want 1, 1", up("postgres"), up("blobstore"))
	}

	if got := Dependencies(pool, failingBlobs{fs})(ctx); got != NotReadyBlobstore {
		t.Errorf("blob store down: %q, want blobstore", got)
	}
	if up("blobstore") != 0 {
		t.Errorf("dependency_up blobstore = %v, want 0", up("blobstore"))
	}
}

// A probe that hangs up does not cancel the check, so no false result is
// cached.
func TestCheckIgnoresCallerCancel(t *testing.T) {
	s, _ := newServer(nil)
	s.SetCheck(func(ctx context.Context) string {
		if ctx.Err() != nil {
			return NotReadyPostgres
		}
		return ""
	})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.Ready(gone); got != "" {
		t.Errorf("Ready with a cancelled caller = %q, want ready", got)
	}
}

// A /metrics scrape runs the cached check, so recap_dependency_up stays
// current without readiness probes.
func TestMetricsRefreshesCheck(t *testing.T) {
	s, c := newServer(nil)
	calls := 0
	s.SetCheck(func(context.Context) string { calls++; return "" })
	h := s.Handler()
	get(t, h, "/metrics")
	get(t, h, "/metrics")
	if calls != 1 {
		t.Errorf("two scrapes within 5 s ran the check %d times, want 1", calls)
	}
	c.t = c.t.Add(ReadyCacheTime)
	get(t, h, "/metrics")
	if calls != 2 {
		t.Errorf("after 5 s: %d checks, want 2", calls)
	}
}
