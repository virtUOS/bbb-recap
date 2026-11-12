package docs

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/sink"
)

const key = "FIXTURE-NOT-A-SECRET-docs-test"

var req = sink.Request{JobID: "019a2f8c-3b40-7000-8000-000000000009", SinkProfile: "docs-main",
	Title: "Fixture Widget Meeting 2025-10-29", SummaryMD: "# Summary\n\nThe widget is blue.\n",
	Owner: sink.Owner{Sub: "fixture-sub-ada-0001", Email: "ada.testperson@example.org"}}

// server answers with h and counts requests.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func client(url string, timeout time.Duration, logs io.Writer) *Client {
	return New("docs-main", url, key, timeout, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestRequestShape(t *testing.T) {
	var got *http.Request
	var body []byte
	srv, n := server(t, func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		answer(http.StatusCreated, `{"id": "doc-1"}`)(w, r)
	})
	id, err := client(srv.URL+"/", 0, io.Discard).Send(context.Background(), req)
	if err != nil || id != "doc-1" || n.Load() != 1 {
		t.Fatalf("id %q err %v requests %d", id, err, n.Load())
	}
	if got.Method != http.MethodPost || got.URL.Path != CreatePath || got.URL.RawQuery != "" {
		t.Errorf("request %s %s", got.Method, got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer "+key || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", got.Header)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"title": req.Title, "content": req.SummaryMD, "sub": req.Owner.Sub, "email": req.Owner.Email}
	if len(m) != len(want) {
		t.Errorf("body fields %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("field %s = %q", k, m[k])
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestStatusClasses(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		class  sink.Class
	}{
		{201, `not json`, sink.Uncertain},
		{201, `{"id": ""}`, sink.Uncertain},
		{200, `{"id": "x"}`, sink.Uncertain},
		{302, ``, sink.Uncertain},
		{429, `{"detail": "throttled"}`, sink.NotSent},
		{401, `{"detail": "bad token"}`, sink.Config},
		{403, `{"detail": "forbidden"}`, sink.Config},
		{400, `{"email": ["two users"]}`, sink.RecipientUnresolved},
		{400, `{"content": ["Could not convert content"]}`, sink.Rejected},
		{400, `not json`, sink.Rejected},
		{404, `{"detail": "not found"}`, sink.Rejected},
		{500, `{"detail": "boom"}`, sink.Uncertain},
		{503, ``, sink.Uncertain},
	} {
		h := answer(c.status, c.body)
		if c.status == 302 {
			h = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }
		}
		srv, n := server(t, h)
		var logs bytes.Buffer
		_, err := client(srv.URL, 0, &logs).Send(context.Background(), req)
		var serr *sink.Error
		if !errors.As(err, &serr) || serr.Class != c.class || serr.Code == "" {
			t.Errorf("%d %s: got %v, want %s", c.status, c.body, err, c.class)
		}
		if n.Load() != 1 {
			t.Errorf("%d: %d requests, want 1 (no retry, no redirect)", c.status, n.Load())
		}
		expectNoContent(t, logs.String(), c.body)
	}
}

func TestRetryAfter(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := client(srv.URL, 0, io.Discard).Send(context.Background(), req)
	var serr *sink.Error
	if !errors.As(err, &serr) || serr.Class != sink.NotSent || serr.RetryAfter != 2*time.Minute {
		t.Fatalf("got %+v", err)
	}
}

func TestConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = client("http://"+addr, 0, io.Discard).Send(context.Background(), req)
	var serr *sink.Error
	if !errors.As(err, &serr) || serr.Class != sink.NotSent {
		t.Fatalf("got %v, want not_sent", err)
	}
}

func TestTimeoutAfterSending(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv, n := server(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	_, err := client(srv.URL, 200*time.Millisecond, io.Discard).Send(context.Background(), req)
	var serr *sink.Error
	if !errors.As(err, &serr) || serr.Class != sink.Uncertain || serr.Code != "timeout" || n.Load() != 1 {
		t.Fatalf("got %v (%d requests), want uncertain timeout", err, n.Load())
	}
}

func TestConnectionLostAfterSending(t *testing.T) {
	srv, n := server(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	_, err := client(srv.URL, 0, io.Discard).Send(context.Background(), req)
	var serr *sink.Error
	if !errors.As(err, &serr) || serr.Class != sink.Uncertain || n.Load() != 1 {
		t.Fatalf("got %v (%d requests), want uncertain", err, n.Load())
	}
}

// The log has IDs and codes, never content, owner, key or response body.
func expectNoContent(t *testing.T, logs, body string) {
	t.Helper()
	for _, s := range []string{req.Title, "widget", req.Owner.Sub, req.Owner.Email, key} {
		if strings.Contains(logs, s) {
			t.Errorf("log contains %q", s)
		}
	}
	if body != "" && strings.Contains(logs, body) {
		t.Errorf("log contains the response body")
	}
}
