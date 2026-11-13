package sink

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The webhook sink (sinks.md §4b, ADR-016): CT-SINK-20 to CT-SINK-24. They
// drive `recap tool sink` (RECAP_BIN) with testdata/sink/sinks-webhook.yaml
// against a mock receiver that the test serves over HTTPS. Written before the
// implementation: they fail until the service has the webhook sink type.

const (
	webhookProfile = "webhook-main"
	// webhookPath and webhookQuery are the profile URL's path and query on
	// the mock. The query is a log canary: the URL is never logged.
	webhookPath  = "/hooks/recap"
	webhookQuery = "channel=fixture-query-canary"
)

// TestWebhookSink holds the webhook sink tests of sinks.md §4b.5.
func TestWebhookSink(t *testing.T) {
	t.Run("CT-SINK-20 signed notice", testWebhookNotice)
	t.Run("CT-SINK-21 summary content and key rotation", testWebhookSummary)
	t.Run("CT-SINK-22 error classes", testWebhookClasses)
	t.Run("CT-SINK-23 re-entry and stable webhook-id", testWebhookReentry)
	t.Run("CT-SINK-24 declaration required and no content in logs", testWebhookConfigAndLogs)
}

// --- mock receiver ---

type receivedHook struct {
	method, path, query string
	header              http.Header
	body                []byte
}

// hookFault is one queued answer: a status (with Location for 3xx), a hang
// until the client gives up, or a dropped connection after reading the body.
type hookFault struct {
	status   int
	location string
	hang     bool
	drop     bool
}

type mockReceiver struct {
	hold     time.Duration
	mu       sync.Mutex
	faults   []hookFault
	requests []receivedHook
}

func (m *mockReceiver) queue(f ...hookFault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = append(m.faults, f...)
}

func (m *mockReceiver) received() []receivedHook {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]receivedHook(nil), m.requests...)
}

func (m *mockReceiver) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults, m.requests = nil, nil
}

func (m *mockReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, receivedHook{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
	var f hookFault
	if len(m.faults) > 0 {
		f, m.faults = m.faults[0], m.faults[1:]
	}
	m.mu.Unlock()
	switch {
	case f.hang:
		select {
		case <-r.Context().Done():
		case <-time.After(m.hold):
		}
	case f.drop:
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
			}
		}
	case f.status != 0:
		if f.location != "" {
			w.Header().Set("Location", f.location)
		}
		if f.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, `{"detail":"mock-receiver: refused"}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// webhookSetup starts a mock receiver over TLS and a tool environment whose
// profile URL points at it.
func webhookSetup(t *testing.T) (*mockReceiver, *httptest.Server, sinkEnv) {
	t.Helper()
	m := &mockReceiver{hold: 10 * time.Second}
	srv := httptest.NewUnstartedServer(m)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return m, srv, newProfileEnv(t, "sinks-webhook.yaml", srv, "url", srv.URL+webhookPath+"?"+webhookQuery)
}

func webhookReq(t *testing.T) hookRequest { return hookReq(t, webhookProfile, nil) }

func fixtureSigningSecret(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", "webhook-signing.key"))))
}

// oldSigningSecret is a second, synthetic secret for the rotation case:
// whsec_ + base64 of 40 bytes starting with the fixture prefix.
var oldSigningSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("FIXTURE-NOT-A-SECRET-webhook-old-000000"))

// --- checks ---

// expectSignedPost checks one request against sinks.md §4b.2: POST to the
// profile URL, JSON, the three Standard Webhooks headers, one valid v1
// signature per secret in order, no Authorization header.
func expectSignedPost(t *testing.T, h receivedHook, req hookRequest, secrets []string) {
	t.Helper()
	if h.method != http.MethodPost || h.path != webhookPath || h.query != webhookQuery {
		t.Fatalf("request %s %s?%s, want POST %s?%s (the profile URL)", h.method, h.path, h.query, webhookPath, webhookQuery)
	}
	if ct := h.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	if h.header.Get("Authorization") != "" {
		t.Error("the webhook sink sends no Authorization header")
	}
	id := h.header.Get("webhook-id")
	if want := req.JobID + ":" + req.SinkProfile; id != want {
		t.Errorf("webhook-id %q, want %q", id, want)
	}
	ts := h.header.Get("webhook-timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("webhook-timestamp %q is not Unix seconds", ts)
	}
	if d := time.Since(time.Unix(sec, 0)); d < -time.Minute || d > 5*time.Minute {
		t.Errorf("webhook-timestamp %s is not the time of the attempt", ts)
	}
	sigs := strings.Fields(h.header.Get("webhook-signature"))
	if len(sigs) != len(secrets) {
		t.Fatalf("webhook-signature has %d signatures, want %d (one per secret line)", len(sigs), len(secrets))
	}
	for i, sec := range secrets {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sec, "whsec_"))
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(id + "." + ts + "."))
		mac.Write(h.body)
		if want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); sigs[i] != want {
			t.Errorf("signature %d does not verify with secret line %d over id.timestamp.body", i+1, i+1)
		}
	}
}

// expectWebhookBody checks the body of sinks.md §4b.2 and returns its data.
func expectWebhookBody(t *testing.T, h receivedHook, req hookRequest, content string) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(h.body, &top); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	expectKeys(t, "body", top, "type", "timestamp", "data")
	var typ, ts string
	_ = json.Unmarshal(top["type"], &typ)
	_ = json.Unmarshal(top["timestamp"], &ts)
	if typ != "recap.summary.delivered" {
		t.Errorf("type %q, want recap.summary.delivered", typ)
	}
	if at, err := time.Parse(time.RFC3339Nano, ts); err != nil || !strings.HasSuffix(ts, "Z") || time.Since(at) > 5*time.Minute {
		t.Errorf("timestamp %q is not the attempt's time in RFC 3339 UTC", ts)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		t.Fatalf("data is not an object: %v", err)
	}
	expectKeys(t, "data", data, "job_id", "sink_profile", "title", "links", "content", "summary_md", "summary_sha256")
	for k, want := range map[string]string{"job_id": req.JobID, "sink_profile": req.SinkProfile, "title": req.Title, "content": content} {
		var got string
		if err := json.Unmarshal(data[k], &got); err != nil || got != want {
			t.Errorf("data.%s = %s, want %q", k, data[k], want)
		}
	}
	var links []link
	if err := json.Unmarshal(data["links"], &links); err != nil || !slices.Equal(links, req.Links) {
		t.Errorf("data.links = %s, want the request's links %v", data["links"], req.Links)
	}
	for _, v := range []string{req.Owner.Sub, req.Owner.Email} {
		if strings.Contains(string(h.body), v) {
			t.Errorf("body contains the owner value %q (sinks.md §4b.2: never owner data)", v)
		}
	}
	return data
}

func expectKeys(t *testing.T, what string, m map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q", what, k)
		}
	}
	for k := range m {
		if !slices.Contains(keys, k) {
			t.Errorf("%s has field %q not in sinks.md §4b.2", what, k)
		}
	}
}

// --- tests ---

func testWebhookNotice(t *testing.T) {
	m, _, e := webhookSetup(t)
	req := webhookReq(t)
	o := e.runHook(t, hookInput{Request: req})
	got := m.received()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	expectSignedPost(t, got[0], req, []string{fixtureSigningSecret(t)})
	data := expectWebhookBody(t, got[0], req, "notice")
	if string(data["summary_md"]) != "null" || string(data["summary_sha256"]) != "null" {
		t.Errorf("notice carries summary_md %s, summary_sha256 %s; want null", data["summary_md"], data["summary_sha256"])
	}
	if strings.Contains(string(got[0].body), contentCanaries[0]) {
		t.Error("notice body contains summary text")
	}
	expectSentNoDocument(t, o, req)
}

func testWebhookSummary(t *testing.T) {
	m, _, e := webhookSetup(t)
	e = e.withValue(t, "content", "summary")
	secrets := []string{fixtureSigningSecret(t), oldSigningSecret}
	keyFile := filepath.Join(t.TempDir(), "webhook-signing.key")
	writeFile(t, keyFile, []byte(strings.Join(secrets, "\n")+"\n"))
	e = e.withValue(t, "signing_secret_file", keyFile)
	req := webhookReq(t)
	o := e.runHook(t, hookInput{Request: req})
	got := m.received()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	expectSignedPost(t, got[0], req, secrets)
	data := expectWebhookBody(t, got[0], req, "summary")
	var md, sum string
	if err := json.Unmarshal(data["summary_md"], &md); err != nil || md != req.SummaryMD {
		t.Errorf("summary_md differs from the request's summary.md (%d vs %d bytes)", len(md), len(req.SummaryMD))
	}
	h := sha256.Sum256([]byte(req.SummaryMD))
	if err := json.Unmarshal(data["summary_sha256"], &sum); err != nil || sum != hex.EncodeToString(h[:]) {
		t.Errorf("summary_sha256 %s, want the SHA-256 of summary_md", data["summary_sha256"])
	}
	expectSentNoDocument(t, o, req)
}

func testWebhookClasses(t *testing.T) {
	m, srv, e := webhookSetup(t)
	req := webhookReq(t)
	cases := []struct {
		name  string
		fault hookFault
		class string
	}{
		{"429", hookFault{status: http.StatusTooManyRequests}, "not_sent"},
		{"500", hookFault{status: http.StatusInternalServerError}, "uncertain"},
		{"503", hookFault{status: http.StatusServiceUnavailable}, "uncertain"},
		{"hang", hookFault{hang: true}, "uncertain"},
		{"drop", hookFault{drop: true}, "uncertain"},
		{"301", hookFault{status: http.StatusMovedPermanently, location: srv.URL + "/redirected"}, "config"},
		{"401", hookFault{status: http.StatusUnauthorized}, "config"},
		{"403", hookFault{status: http.StatusForbidden}, "config"},
		{"404", hookFault{status: http.StatusNotFound}, "config"},
		{"410", hookFault{status: http.StatusGone}, "config"},
		{"400", hookFault{status: http.StatusBadRequest}, "rejected"},
		{"422", hookFault{status: http.StatusUnprocessableEntity}, "rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.reset()
			m.queue(c.fault)
			o := e.runHook(t, hookInput{Request: req, RequestTimeoutMS: ms(1000)})
			expectClassRecord(t, o, c.class)
			got := m.received()
			if len(got) != 1 {
				t.Fatalf("receiver got %d requests, want exactly 1 (redirects are not followed)", len(got))
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		closed := httptest.NewUnstartedServer(http.NotFoundHandler())
		closed.StartTLS()
		u := closed.URL
		closed.Close()
		o := e.withValue(t, "url", u+webhookPath).runHook(t, hookInput{Request: req})
		expectClassRecord(t, o, "not_sent")
	})
}

func testWebhookReentry(t *testing.T) {
	m, _, e := webhookSetup(t)
	req := webhookReq(t)
	o := e.runHook(t, hookInput{Request: req, Delivery: &record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: "sending"}})
	expectClassRecord(t, o, "uncertain")
	if n := len(m.received()); n != 0 {
		t.Fatalf("re-entry in sending: receiver got %d requests, want none (sinks.md §1.4)", n)
	}
	m.queue(hookFault{status: http.StatusTooManyRequests})
	expectClassRecord(t, e.runHook(t, hookInput{Request: req}), "not_sent")
	expectSentNoDocument(t, e.runHook(t, hookInput{Request: req}), req)
	got := m.received()
	if len(got) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(got))
	}
	if a, b := got[0].header.Get("webhook-id"), got[1].header.Get("webhook-id"); a == "" || a != b {
		t.Errorf("webhook-id %q then %q: want the same id on every attempt (sinks.md §4b.2)", a, b)
	}
}

func testWebhookConfigAndLogs(t *testing.T) {
	_, _, e := webhookSetup(t)
	req := webhookReq(t)
	t.Run("declaration required", func(t *testing.T) {
		expectDeclarationRequired(t, e, hookInput{Request: req})
	})
	t.Run("no content in logs", func(t *testing.T) {
		canaries := append(append([]string{webhookQuery, fixtureSigningSecret(t), oldSigningSecret, req.Title},
			contentCanaries...), identityCanaries...)
		scanHookLogs(t, canaries)
	})
}
