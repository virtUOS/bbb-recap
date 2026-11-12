package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ProblemTypePrefix starts every problem type (README "HTTP").
const ProblemTypePrefix = "urn:recap:problem:"

// Problem is an RFC 9457 problem details object. Detail never contains
// personal data.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an application/problem+json response with type
// urn:recap:problem:<slug>.
func WriteProblem(w http.ResponseWriter, status int, slug, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:   ProblemTypePrefix + slug,
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}

// ReadBody reads the request body up to limit bytes. A larger body gets
// 413 payload-too-large, any other read error 400 invalid-request; then it
// returns false and the handler should stop.
func ReadBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteProblem(w, http.StatusRequestEntityTooLarge, "payload-too-large", "")
		} else {
			WriteProblem(w, http.StatusBadRequest, "invalid-request", "")
		}
		return nil, false
	}
	return b, true
}

// ReadJSON reads a body of at most limit bytes and decodes it into v. On
// failure it writes the problem (413 or 400 invalid-request) and returns false.
func ReadJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	b, ok := ReadBody(w, r, limit)
	if !ok {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-request", "body is not valid JSON")
		return false
	}
	return true
}

// statusRecorder remembers the status code and counts the response bytes.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// LogRequests logs each request at DEBUG: method, path without the query
// string, status, body lengths and duration. Never bodies, headers or the
// query string (privacy.md §6).
func LogRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"request_bytes", r.ContentLength,
			"response_bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// ShutdownTimeout is the drain budget after cancel. architecture.md §9 gives
// the whole shutdown 25 s; the drain leaves 2 s of it for closing the
// database pool afterwards (ops.PoolCloseTime, used by cmd/recap).
const ShutdownTimeout = 23 * time.Second

// Run serves h on addr until ctx is cancelled, then stops accepting and lets
// in-flight requests finish for at most ShutdownTimeout. If they do not, it
// closes their connections and returns context.DeadlineExceeded.
func Run(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, h, ShutdownTimeout)
}

func serve(ctx context.Context, ln net.Listener, h http.Handler, drain time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second, // slow-header protection (architecture.md §9)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}
