// Package ops serves the ops listener of api and worker (docs/reference/README.md
// "HTTP": :9090, not public): /healthz, /readyz and /metrics
// (architecture.md §9, §12). It also holds the shutdown budget that every
// process keeps.
package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/metrics"
)

// Addr is the ops listener (fixed port, docs/reference/README.md "HTTP").
const Addr = ":9090"

// ReadyURL is what recap healthcheck probes (deployment.md §1).
const ReadyURL = "http://127.0.0.1:9090/readyz"

// Shutdown budget (architecture.md §9): everything after SIGTERM fits into
// ShutdownBudget, below the 30 s grace period of deployment.md §4. The api
// drains its public listener and the worker its steps for ShutdownBudget −
// PoolCloseTime; PoolCloseTime is left for closing the database pool. The
// ops listener is closed last, at once.
const (
	ShutdownBudget = 25 * time.Second
	PoolCloseTime  = 2 * time.Second
)

// ReadyCacheTime is how long a readiness result is reused (§9: cached 5 s).
const ReadyCacheTime = 5 * time.Second

// checkTimeout bounds one readiness check.
const checkTimeout = 3 * time.Second

// Readiness codes: the body of a 503 from /readyz.
const (
	NotReadyStarting  = "starting"  // the database is not open yet
	NotReadyStopping  = "stopping"  // shutdown has begun
	NotReadyPostgres  = "postgres"  // ping failed
	NotReadySchema    = "schema"    // schema older than this binary, or unreadable
	NotReadyBlobstore = "blobstore" // blob store ping failed
)

// Check reports why the process is not ready, as one of the readiness
// codes, or "" when it is ready. It gets a context without cancellation and
// must bound its own run time.
type Check func(ctx context.Context) string

// Dependencies returns the readiness check of §9: database ping, schema
// version compatible, blob store reachable. It also sets
// recap_dependency_up.
func Dependencies(pool *pgxpool.Pool, blobs blob.Store) Check {
	return func(ctx context.Context) string {
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		code := ""
		pgUp := pool.Ping(ctx) == nil
		if !pgUp {
			code = NotReadyPostgres
		} else if ok, err := db.SchemaCompatible(ctx, pool); !ok || err != nil {
			code = NotReadySchema
		}
		blobUp := blobs.Ping(ctx) == nil
		if !blobUp && code == "" {
			code = NotReadyBlobstore
		}
		metrics.SetDependencyUp(metrics.DependencyPostgres, pgUp)
		metrics.SetDependencyUp(metrics.DependencyBlobstore, blobUp)
		return code
	}
}

// Server is the ops listener.
type Server struct {
	log      *slog.Logger
	metrics  prometheus.Gatherer
	stopping <-chan struct{}
	now      func() time.Time

	mu      sync.Mutex
	check   Check
	checked time.Time // when code was computed; zero: never
	code    string

	srv *http.Server
}

// New returns an ops server that serves g on /metrics. Once stopping is
// closed (the process got SIGTERM), /readyz answers 503 at once.
func New(log *slog.Logger, g prometheus.Gatherer, stopping <-chan struct{}) *Server {
	return &Server{log: log, metrics: g, stopping: stopping, now: time.Now}
}

// SetCheck sets the readiness check. Until it is set, the process is not
// ready (NotReadyStarting).
func (s *Server) SetCheck(c Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.check, s.checked = c, time.Time{}
}

// Ready returns "" when the process is ready, else a readiness code. The
// check result is reused for ReadyCacheTime; a shutdown is seen at once.
func (s *Server) Ready(ctx context.Context) string {
	select {
	case <-s.stopping:
		return NotReadyStopping
	default:
	}
	return s.checkCached(ctx)
}

// checkCached runs the check unless its last result is younger than
// ReadyCacheTime. The check does not inherit the caller's cancellation: a
// probe that hangs up must not leave a false result in the cache.
func (s *Server) checkCached(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.check == nil {
		return NotReadyStarting
	}
	if s.checked.IsZero() || s.now().Sub(s.checked) >= ReadyCacheTime {
		s.code = s.check(context.WithoutCancel(ctx))
		s.checked = s.now()
	}
	return s.code
}

// Handler returns the routes of the ops listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		code := s.Ready(r.Context())
		if code != "" {
			s.log.Debug("not ready", "reason", code)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, code)
			return
		}
		_, _ = fmt.Fprintln(w, "ready")
	})
	scrape := promhttp.HandlerFor(s.metrics, promhttp.HandlerOpts{})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		// Keeps recap_dependency_up current without a readiness probe
		// (same cached check).
		s.checkCached(r.Context())
		scrape.ServeHTTP(w, r)
	})
	return mux
}

// Start listens on addr and serves in the background. It returns the
// address it listens on.
func (s *Server) Start(addr string) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second, // slow-header protection (architecture.md §9)
	}
	go func() {
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("ops listener failed", "err", err)
		}
	}()
	return ln.Addr(), nil
}

// Close stops the listener and drops open connections at once: probes and
// scrapes are short, and the shutdown budget is spent by then.
func (s *Server) Close() {
	if s != nil && s.srv != nil {
		_ = s.srv.Close()
	}
}

// Probe GETs url and returns nil on 200 (recap healthcheck).
func Probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
