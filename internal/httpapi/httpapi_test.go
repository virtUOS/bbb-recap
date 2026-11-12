package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/ops"
)

const (
	frontendToken     = "FIXTURE-NOT-A-SECRET-frontend-current"
	frontendNextToken = "FIXTURE-NOT-A-SECRET-frontend-next"
	serverToken       = "FIXTURE-NOT-A-SECRET-bbb-server"
	adminToken        = "FIXTURE-NOT-A-SECRET-admin"
)

func writeSecrets(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testCredentials(t *testing.T) *Credentials {
	t.Helper()
	dir := writeSecrets(t, map[string]string{
		"frontend.pilos-fixture":      frontendToken + "\n",
		"frontend.pilos-fixture.next": frontendNextToken,
		"bbb_server.bbb-fixture-a":    serverToken + "\n",
		"admin.fixture-admin":         adminToken,
	})
	if err := os.Mkdir(filepath.Join(dir, "..data"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	return c
}

// whoami answers with the authenticated client.
var whoami = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	c, _ := ClientFrom(r.Context())
	_, _ = io.WriteString(w, string(c.Kind)+"/"+c.ID)
})

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("problem body %q: %v", rec.Body.String(), err)
	}
	for _, k := range []string{"type", "title", "status"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("problem lacks %q: %s", k, rec.Body.String())
		}
	}
	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Status != rec.Code {
		t.Errorf("problem status %d != HTTP status %d", p.Status, rec.Code)
	}
	return p
}

func TestRequire(t *testing.T) {
	creds := testCredentials(t)
	h := creds.Require(KindFrontend, whoami)
	tests := []struct {
		name, auth string
		want       int
		body       string
	}{
		{"current credential", "Bearer " + frontendToken, 200, "frontend/pilos-fixture"},
		{"next credential", "Bearer " + frontendNextToken, 200, "frontend/pilos-fixture"},
		{"missing header", "", 401, ""},
		{"empty token", "Bearer ", 401, ""},
		{"not bearer", "Basic " + frontendToken, 401, ""},
		{"unknown token", "Bearer FIXTURE-NOT-A-SECRET-unknown", 401, ""},
		{"token with newline", "Bearer " + frontendToken + "\n", 401, ""},
		{"wrong kind: bbb_server", "Bearer " + serverToken, 401, ""},
		{"wrong kind: admin", "Bearer " + adminToken, 401, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/registrations/rec-1", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == 200 {
				if rec.Body.String() != tt.body {
					t.Errorf("client = %q, want %q", rec.Body.String(), tt.body)
				}
				return
			}
			if p := decodeProblem(t, rec); p.Type != "urn:recap:problem:unauthorized" {
				t.Errorf("type = %q", p.Type)
			}
		})
	}
}

func TestLoadCredentialsErrors(t *testing.T) {
	dir := writeSecrets(t, map[string]string{
		"frontend.Bad_ID":        "x",
		"robot.one":              "x",
		"admin.fixture-admin":    "\n",
		"bbb_server.bbb-fixture": "ok",
	})
	_, err := LoadCredentials(dir)
	if err == nil {
		t.Fatal("want error")
	}
	for _, w := range []string{"frontend.Bad_ID", "robot.one", "admin.fixture-admin is empty"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
	if strings.Contains(err.Error(), "bbb_server.bbb-fixture") {
		t.Errorf("valid file reported: %v", err)
	}
}

func TestLoadCredentialsDuplicateToken(t *testing.T) {
	const shared = "FIXTURE-NOT-A-SECRET-shared"
	dir := writeSecrets(t, map[string]string{
		"frontend.pilos-fixture":   shared + "\n",
		"bbb_server.bbb-fixture-a": shared,
		"admin.fixture-admin":      adminToken,
	})
	_, err := LoadCredentials(dir)
	if err == nil {
		t.Fatal("want error for a token shared by two files")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bbb_server.bbb-fixture-a") || !strings.Contains(msg, "frontend.pilos-fixture") {
		t.Errorf("error %q does not name both files", msg)
	}
	if strings.Contains(msg, "FIXTURE-NOT-A-SECRET") || strings.Contains(msg, "admin.fixture-admin") {
		t.Errorf("error %q leaks the token or names an unrelated file", msg)
	}
}

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusConflict, "record-already-ingested", "")
	p := decodeProblem(t, rec)
	want := Problem{Type: "urn:recap:problem:record-already-ingested", Title: "Conflict", Status: 409}
	if p != want {
		t.Errorf("problem = %+v, want %+v", p, want)
	}
	if strings.Contains(rec.Body.String(), "detail") {
		t.Errorf("empty detail must be omitted: %s", rec.Body.String())
	}
}

func TestReadJSON(t *testing.T) {
	handler := func(limit int64) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v map[string]any
			if ReadJSON(w, r, limit, &v) {
				w.WriteHeader(http.StatusNoContent)
			}
		})
	}
	tests := []struct {
		name, body string
		limit      int64
		want       int
		slug       string
	}{
		{"fits", `{"a":1}`, 7, 204, ""},
		{"one byte too large", `{"a":12}`, 7, 413, "payload-too-large"},
		{"far too large", `{"a":"` + strings.Repeat("x", 10000) + `"}`, 8192, 413, "payload-too-large"},
		{"not JSON", `{"a":`, 100, 400, "invalid-request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(tt.limit).ServeHTTP(rec, httptest.NewRequest("PUT", "/x", strings.NewReader(tt.body)))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.slug != "" {
				if p := decodeProblem(t, rec); p.Type != ProblemTypePrefix+tt.slug {
					t.Errorf("type = %q, want slug %s", p.Type, tt.slug)
				}
			}
		})
	}
}

func TestLogRequestsNeverLogsTokenOrBody(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	creds := testCredentials(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if ReadJSON(w, r, 1024, &v) {
			_, _ = io.WriteString(w, `{"echo":"ada.testperson@example.org"}`)
		}
	})
	h := LogRequests(log, creds.Require(KindFrontend, inner))

	body := `{"owner":{"sub":"fixture-sub-ada-0001","email":"ada.testperson@example.org"}}`
	for _, token := range []string{frontendToken, "FIXTURE-NOT-A-SECRET-wrong"} {
		req := httptest.NewRequest("PUT", "/api/v1/registrations/rec-1?secret=FIXTURE-NOT-A-SECRET-query", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	out := logBuf.String()
	for _, banned := range []string{"FIXTURE-NOT-A-SECRET", "fixture-sub-ada", "example.org", "Bearer", "secret="} {
		if strings.Contains(out, banned) {
			t.Errorf("log contains %q:\n%s", banned, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d:\n%s", len(lines), out)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["level"] != "DEBUG" || first["method"] != "PUT" || first["path"] != "/api/v1/registrations/rec-1" ||
		first["status"] != float64(200) || first["request_bytes"] != float64(len(body)) {
		t.Errorf("log line = %v", first)
	}
	for _, k := range []string{"response_bytes", "duration_ms"} {
		if _, ok := first[k]; !ok {
			t.Errorf("log line lacks %s: %v", k, first)
		}
	}
}

func TestLogRequestsSilentAboveDebug(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	LogRequests(log, whoami).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if logBuf.Len() != 0 {
		t.Errorf("logged at INFO: %s", logBuf.String())
	}
}

func TestServeDrainsInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, h, 5*time.Second) }()

	got := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()
	<-started
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if body := <-got; body != "done" {
		t.Errorf("in-flight request got %q, want done", body)
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Error("server still accepts after shutdown")
	}
}

// The api's drain plus the pool close fit into the 25 s of architecture.md §9.
func TestShutdownTimeoutFitsBudget(t *testing.T) {
	if got := ShutdownTimeout + ops.PoolCloseTime; got > ops.ShutdownBudget {
		t.Errorf("drain %v + pool close %v = %v, want at most %v",
			ShutdownTimeout, ops.PoolCloseTime, got, ops.ShutdownBudget)
	}
}

func TestServeStopsAfterDrainBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, h, 100*time.Millisecond) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("serve = %v, want deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after the drain budget")
	}
}
