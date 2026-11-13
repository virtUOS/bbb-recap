package transkribierbar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

const token = "tw_FIXTURE-NOT-A-SECRET-transkribierbar"

func profile(t *testing.T, baseURL string, extra string) *providers.Profile {
	t.Helper()
	key := filepath.Join(t.TempDir(), "asr.key")
	if err := os.WriteFile(key, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := `schema_version: "1.0.0"
defaults: {asr: asr-test, llm: llm-test}
profiles:
  - {name: asr-test, kind: asr, dialect: transkribierbar, base_url: "` + baseURL + `", model: large-v3,
     credentials_file: "` + key + `"` + extra + `,
     declaration: {data_categories: [voice], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-11-06"}}
  - {name: llm-test, kind: llm, dialect: openai-chat, base_url: "http://llm.recap.test/v1", model: m,
     context_tokens: 100, max_output_tokens: 10,
     declaration: {data_categories: [transcript], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-11-06"}}
`
	s, err := providers.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Get(providers.KindASR, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var audio = []byte("fixture audio bytes")

func request() providers.AsrRequest {
	return providers.AsrRequest{
		JobID: "0190f5a0-0000-7000-8000-000000000001", TrackRef: "trk-3",
		IdempotencyKey: "0190f5a0-0000-7000-8000-000000000001:trk-3:2",
		Audio: providers.Audio{
			Open:      func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(audio)), nil },
			Size:      int64(len(audio)),
			MediaType: "audio/webm",
		},
		DurationMS: 9000,
	}
}

type reply struct {
	status int
	header map[string]string
	body   string
}

// seen is one request the fake received.
type seen struct {
	method, path  string
	header        http.Header
	body          []byte // JSON body of /api/transcribe
	part, file    string // multipart part and file name of /api/upload
	fileType      string
	fileSize      int
	transcribeRaw map[string]any
}

// fake mimics the job API of §2a. statuses are the replies of successive
// GET /api/status calls (the last one repeats).
type fake struct {
	t                                   *testing.T
	upload, transcribe, result, deleted reply
	statuses                            []reply

	mu   sync.Mutex
	seen []seen
}

func newFake(t *testing.T) *fake {
	return &fake{
		t:          t,
		upload:     reply{200, nil, `{"id":"file-1","original_filename":"trk-3.webm","media_type":"webm","file_size":19,"has_video":false}`},
		transcribe: reply{200, nil, `{"id":"tr-1","status":"pending"}`},
		statuses: []reply{
			{200, nil, `{"id":"tr-1","status":"pending","progress":null,"error":null,"error_code":null}`},
			{200, nil, `{"id":"tr-1","status":"processing","progress":null,"error":null,"error_code":null}`},
			{200, nil, `{"id":"tr-1","status":"completed","progress":null,"error":null,"error_code":null}`},
		},
		result: reply{200, nil, `{"id":"tr-1","status":"completed","utterances":[` +
			`{"start":813,"end":2340,"text":" Welcome to the fixture meeting. ","speaker":"SPEAKER_00"},` +
			`{"start":3000,"end":2900,"text":"Agreed.","speaker":null},` +
			`{"start":4000,"end":4500,"text":"   ","speaker":null}],` +
			`"text":"Welcome to the fixture meeting. Agreed.","language":"de","speaker_mappings":{},"edited":false}`},
		deleted: reply{200, nil, `{"status":"deleted"}`},
	}
}

func (f *fake) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	s := seen{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
	var rep reply
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/upload":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			f.t.Errorf("multipart: %v", err)
		}
		for name, fhs := range r.MultipartForm.File {
			s.part, s.file, s.fileType, s.fileSize = name, fhs[0].Filename, fhs[0].Header.Get("Content-Type"), int(fhs[0].Size)
		}
		rep = f.upload
	case r.Method == http.MethodPost && r.URL.Path == "/api/transcribe":
		s.body, _ = io.ReadAll(r.Body)
		if err := json.Unmarshal(s.body, &s.transcribeRaw); err != nil {
			f.t.Errorf("transcribe body: %v", err)
		}
		f.mu.Lock()
		rep = f.transcribe
		f.mu.Unlock()
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/status/"):
		f.mu.Lock()
		rep = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
		f.mu.Unlock()
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/transcription/"):
		rep = f.result
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/transcription/"):
		rep = f.deleted
	default:
		rep = reply{status: 404, body: `{"detail":"Not Found"}`}
	}
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write([]byte(rep.body))
}

// calls lists "METHOD path" of every request.
func (f *fake) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.seen {
		out = append(out, s.method+" "+s.path)
	}
	return out
}

func adapter(t *testing.T, srv *httptest.Server, logs io.Writer) *Adapter {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	a := New(profile(t, srv.URL, ""), slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	a.interval = time.Millisecond
	return a
}

// run does Submit, Status, Result and Cleanup as the transcribe step does.
func run(t *testing.T, a *Adapter, req providers.AsrRequest) (*providers.AsrResult, error) {
	t.Helper()
	ctx := context.Background()
	h, err := a.Submit(ctx, req)
	var r *providers.AsrResult
	if err == nil {
		if st, _ := a.Status(ctx, h); st != providers.StatusSucceeded {
			t.Errorf("status %s after a good submit", st)
		}
		r, err = a.Result(ctx, h)
	}
	if cerr := a.Cleanup(ctx, h); cerr != nil {
		t.Errorf("cleanup: %v", cerr)
	}
	return r, err
}

func TestFlowAndResult(t *testing.T) {
	f := newFake(t)
	srv := f.server()
	var logs bytes.Buffer
	a := adapter(t, srv, &logs)
	req := request()
	req.LanguageHint = "de"
	r, err := run(t, a, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /api/upload", "POST /api/transcribe", "GET /api/status/tr-1", "GET /api/status/tr-1",
		"GET /api/status/tr-1", "GET /api/transcription/tr-1", "DELETE /api/transcription/tr-1"}
	if got := f.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range f.seen {
		if s.header.Get("Authorization") != "Bearer "+token || s.header.Get("X-Request-ID") != req.IdempotencyKey {
			t.Errorf("%s %s: headers %v", s.method, s.path, s.header)
		}
	}
	up := f.seen[0]
	if up.part != "file" || up.file != "trk-3.webm" || up.fileType != "audio/webm" || up.fileSize != len(audio) {
		t.Errorf("upload part %q file %q type %q size %d", up.part, up.file, up.fileType, up.fileSize)
	}
	tr := f.seen[1]
	if tr.header.Get("Content-Type") != "application/json" || len(tr.transcribeRaw) != 3 ||
		tr.transcribeRaw["file_id"] != "file-1" || tr.transcribeRaw["model"] != "large-v3" || tr.transcribeRaw["language"] != "de" {
		t.Errorf("transcribe body %s", tr.body)
	}

	wantResult := `{"language":"de","model":"large-v3","segments":[` +
		`{"start_ms":813,"end_ms":2340,"text":"Welcome to the fixture meeting."},` +
		`{"start_ms":3000,"end_ms":3000,"text":"Agreed."}]}`
	if b, _ := json.Marshal(r); string(b) != wantResult {
		t.Errorf("result\n%s\nwant\n%s", b, wantResult)
	}
	if strings.Contains(logs.String(), "fixture meeting") || strings.Contains(logs.String(), token) ||
		!strings.Contains(logs.String(), `"msg":"asr request done"`) {
		t.Errorf("log:\n%s", logs.String())
	}
}

// Without a language hint the request has no language field, and a
// language that is no ISO code becomes null.
func TestNoLanguage(t *testing.T) {
	f := newFake(t)
	f.result.body = `{"id":"tr-1","status":"completed","utterances":[{"start":0,"end":5,"text":"a"}],"language":null}`
	r, err := run(t, adapter(t, f.server(), nil), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.seen[1].transcribeRaw["language"]; ok || len(f.seen[1].transcribeRaw) != 2 {
		t.Errorf("transcribe body %s", f.seen[1].body)
	}
	if r.Language != nil {
		t.Errorf("language %q, want null", *r.Language)
	}
	f = newFake(t)
	f.result.body = `{"utterances":[],"language":"German"}`
	if r, err = run(t, adapter(t, f.server(), nil), request()); err != nil || r.Language != nil || len(r.Segments) != 0 {
		t.Errorf("language German: %+v %v", r, err)
	}
}

func TestErrorClasses(t *testing.T) {
	const canary = `{"detail":"CANARY-BODY"}`
	for _, c := range []struct {
		name   string
		setup  func(f *fake)
		class  providers.ErrorClass
		code   string
		retry  time.Duration
		delete bool // a transcription exists, so Cleanup deletes it
	}{
		{"upload 401", func(f *fake) { f.upload = reply{401, nil, canary} }, providers.PermanentConfig, "http_401", 0, false},
		{"upload 413", func(f *fake) { f.upload = reply{413, nil, canary} }, providers.PermanentInput, "http_413", 0, false},
		{"upload 429", func(f *fake) { f.upload = reply{429, map[string]string{"Retry-After": "120"}, canary} },
			providers.Retryable, "http_429", 120 * time.Second, false},
		{"upload 503 capped", func(f *fake) { f.upload = reply{503, map[string]string{"Retry-After": "7200"}, canary} },
			providers.Retryable, "http_503", 30 * time.Minute, false},
		{"upload 500", func(f *fake) { f.upload = reply{500, nil, canary} }, providers.Retryable, "http_500", 0, false},
		{"upload 302", func(f *fake) { f.upload = reply{302, map[string]string{"Location": "/elsewhere"}, canary} },
			providers.PermanentConfig, "http_302", 0, false},
		{"upload no id", func(f *fake) { f.upload = reply{200, nil, `{"media_type":"webm"}`} },
			providers.InvalidResponse, "no_file_id", 0, false},
		{"transcribe 404", func(f *fake) { f.transcribe = reply{404, nil, canary} }, providers.PermanentConfig, "http_404", 0, false},
		{"transcribe 422 model", func(f *fake) {
			f.transcribe = reply{422, nil, `{"detail":"Model 'CANARY' is not available on this instance"}`}
		}, providers.PermanentConfig, "model_unavailable", 0, false},
		{"transcribe 422 other", func(f *fake) {
			f.transcribe = reply{422, nil, `{"detail":"Could not prepare the audio for transcription. CANARY"}`}
		}, providers.PermanentInput, "http_422", 0, false},
		{"transcribe 422 validation", func(f *fake) {
			f.transcribe = reply{422, nil, `{"detail":[{"loc":["body","model"],"msg":"is not available on this instance CANARY"}]}`}
		}, providers.PermanentInput, "http_422", 0, false},
		{"status 500", func(f *fake) { f.statuses = []reply{{500, nil, canary}} }, providers.Retryable, "http_500", 0, true},
		{"status 404", func(f *fake) { f.statuses = []reply{{404, nil, canary}} }, providers.PermanentConfig, "http_404", 0, true},
		{"status unknown", func(f *fake) { f.statuses = []reply{{200, nil, `{"id":"tr-1","status":"CANARY"}`}} },
			providers.InvalidResponse, "unknown_status", 0, true},
		{"status not json", func(f *fake) { f.statuses = []reply{{200, nil, `CANARY`}} },
			providers.InvalidResponse, "not_json_object", 0, true},
		{"result 401", func(f *fake) { f.result = reply{401, nil, canary} }, providers.PermanentConfig, "http_401", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			c.setup(f)
			var logs bytes.Buffer
			_, err := run(t, adapter(t, f.server(), &logs), request())
			var e *providers.Error
			if !errors.As(err, &e) || e.Class != c.class || e.Code != c.code || e.RetryAfter != c.retry {
				t.Errorf("error %+v, want %s %s retry %v", e, c.class, c.code, c.retry)
			}
			if strings.Contains(logs.String(), "CANARY") || (err != nil && strings.Contains(err.Error(), "CANARY")) {
				t.Errorf("error body leaked: %v\n%s", err, logs.String())
			}
			calls := f.calls()
			if deleted := calls[len(calls)-1] == "DELETE /api/transcription/tr-1"; deleted != c.delete {
				t.Errorf("calls %v, want delete %v", calls, c.delete)
			}
		})
	}
}

func TestFailedStatus(t *testing.T) {
	for code, want := range map[string]struct {
		class providers.ErrorClass
		code  string
	}{
		`"asr_unreachable"`:  {providers.Retryable, "asr_unreachable"},
		`"timeout"`:          {providers.Retryable, "timeout"},
		`"server_restarted"`: {providers.Retryable, "server_restarted"},
		`"asr_failed"`:       {providers.PermanentInput, "asr_failed"},
		`"unknown"`:          {providers.Retryable, "remote_failed"},
		`"CANARY"`:           {providers.Retryable, "remote_failed"},
		`null`:               {providers.Retryable, "remote_failed"},
	} {
		f := newFake(t)
		f.statuses = []reply{
			{200, nil, `{"id":"tr-1","status":"processing"}`},
			{200, nil, `{"id":"tr-1","status":"failed","error":"CANARY message","error_code":` + code + `}`},
		}
		var logs bytes.Buffer
		_, err := run(t, adapter(t, f.server(), &logs), request())
		var e *providers.Error
		if !errors.As(err, &e) || e.Class != want.class || e.Code != want.code || e.RetryAfter != 0 {
			t.Errorf("error_code %s: %+v, want %+v", code, e, want)
		}
		if strings.Contains(logs.String(), "CANARY") {
			t.Errorf("error_code %s: log leaks the reply", code)
		}
		if calls := f.calls(); calls[len(calls)-1] != "DELETE /api/transcription/tr-1" {
			t.Errorf("error_code %s: no cleanup: %v", code, calls)
		}
	}
}

func TestInvalidUtterances(t *testing.T) {
	for _, body := range []string{
		`not json`, `null`, `{"language":"en"}`, `{"utterances":"x"}`,
		`{"utterances":[{"start":1,"text":"a"}]}`,
		`{"utterances":[{"start":1.5,"end":2,"text":"a"}]}`,
		`{"utterances":[{"start":1,"end":2e3,"text":"a"}]}`,
		`{"utterances":[{"start":"1","end":2,"text":"a"}]}`,
		`{"utterances":[{"start":-1,"end":2,"text":"a"}]}`,
		`{"utterances":[{"start":null,"end":2,"text":"a"}]}`,
		`{"utterances":[{"start":1,"end":2}]}`,
		`{"utterances":[{"start":1,"end":2,"text":7}]}`,
	} {
		f := newFake(t)
		f.result.body = body
		_, err := run(t, adapter(t, f.server(), nil), request())
		var e *providers.Error
		if !errors.As(err, &e) || e.Class != providers.InvalidResponse {
			t.Errorf("%s: error %v, want invalid_response", body, err)
		}
	}
}

// Cancelling the context while polling ends Submit at once with a
// retryable error; Cleanup still deletes the transcription.
func TestCancelDuringPolling(t *testing.T) {
	f := newFake(t)
	f.statuses = []reply{{200, nil, `{"id":"tr-1","status":"processing"}`}}
	a := adapter(t, f.server(), nil)
	a.interval = time.Hour // only cancellation can end the wait
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if len(f.calls()) >= 3 { // upload, transcribe, first status
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	start := time.Now()
	h, err := a.Submit(ctx, request())
	var e *providers.Error
	if !errors.As(err, &e) || e.Class != providers.Retryable || e.Code != "cancelled" {
		t.Errorf("submit: %+v", e)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("submit took %v after cancel", d)
	}
	if st, _ := a.Status(ctx, h); st != providers.StatusFailed {
		t.Errorf("status %s", st)
	}
	if err := a.Cleanup(ctx, h); err != nil { // ctx is cancelled: Cleanup runs anyway
		t.Errorf("cleanup: %v", err)
	}
	if calls := f.calls(); calls[len(calls)-1] != "DELETE /api/transcription/tr-1" {
		t.Errorf("calls %v", calls)
	}
}

// The attempt timeout bounds the whole flow.
func TestAttemptTimeout(t *testing.T) {
	f := newFake(t)
	f.statuses = []reply{{200, nil, `{"id":"tr-1","status":"pending"}`}}
	a := adapter(t, f.server(), nil)
	req := request()
	req.AttemptTimeout = 50 * time.Millisecond
	_, err := run(t, a, req)
	var e *providers.Error
	if !errors.As(err, &e) || e.Class != providers.Retryable || e.Code != "timeout" || e.RetryAfter < 150*time.Second {
		t.Errorf("timeout: %+v", e)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()

	// DELETE once with the transcription ID; a second Cleanup is a no-op.
	f := newFake(t)
	a := adapter(t, f.server(), nil)
	h, err := a.Submit(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.Cleanup(ctx, h); err != nil {
			t.Error(err)
		}
	}
	deletes := 0
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "DELETE ") {
			deletes++
			if c != "DELETE /api/transcription/tr-1" {
				t.Errorf("delete %s", c)
			}
		}
	}
	if deletes != 1 {
		t.Errorf("%d deletes, want 1", deletes)
	}
	if h := f.seen[len(f.seen)-1].header; h.Get("Authorization") != "Bearer "+token || h.Get("X-Request-ID") == "" {
		t.Errorf("delete headers %v", h)
	}

	// 404: already gone, done.
	f = newFake(t)
	f.deleted = reply{404, nil, `{"detail":"Transcription not found"}`}
	a = adapter(t, f.server(), nil)
	h, _ = a.Submit(ctx, request())
	if err := a.Cleanup(ctx, h); err != nil {
		t.Errorf("cleanup 404: %v", err)
	}

	// Other errors are reported with their class.
	f = newFake(t)
	f.deleted = reply{503, nil, `{"detail":"CANARY"}`}
	a = adapter(t, f.server(), nil)
	h, _ = a.Submit(ctx, request())
	var e *providers.Error
	if err := a.Cleanup(ctx, h); !errors.As(err, &e) || e.Class != providers.Retryable {
		t.Errorf("cleanup 503: %v", err)
	}

	// No transcription ID (upload or start failed): no request, no log.
	for _, setup := range []func(*fake){
		func(f *fake) { f.upload = reply{500, nil, ""} },
		func(f *fake) { f.transcribe = reply{500, nil, ""} },
	} {
		f = newFake(t)
		setup(f)
		var logs bytes.Buffer
		a = adapter(t, f.server(), &logs)
		h, _ = a.Submit(ctx, request())
		n, logged := len(f.calls()), logs.Len()
		if err := a.Cleanup(ctx, h); err != nil {
			t.Error(err)
		}
		if len(f.calls()) != n || logs.Len() != logged {
			t.Errorf("cleanup without ID: calls %v, log\n%s", f.calls(), logs.String())
		}
	}
	if err := a.Cleanup(ctx, nil); err != nil {
		t.Errorf("cleanup(nil): %v", err)
	}
}

// Above the size limit: permanent_input without a request.
func TestMaxFileBytes(t *testing.T) {
	f := newFake(t)
	srv := f.server()
	a := New(profile(t, srv.URL, ", max_file_bytes: 4"), slog.New(slog.DiscardHandler))
	if _, err := run(t, a, request()); err == nil || len(f.calls()) != 0 {
		t.Errorf("max_file_bytes: %v, calls %v", err, f.calls())
	}
	c := adapter(t, srv, nil).Capabilities()
	if c.Async || c.WordTimestamps || c.WordConfidence || c.Diarization || *c.MaxFileBytes != 1<<30 ||
		len(c.MediaTypes) != 1 || c.MediaTypes[0] != "audio/webm" || c.RemoteRetention != "until_cleanup" {
		t.Errorf("capabilities %+v", c)
	}
}

// setTranscribe changes the reply of POST /api/transcribe between requests.
func (f *fake) setTranscribe(r reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transcribe = r
}

// startFileIDs lists the file_id of every POST /api/transcribe.
func (f *fake) startFileIDs() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, s := range f.seen {
		if s.path == "/api/transcribe" {
			out = append(out, s.transcribeRaw["file_id"])
		}
	}
	return out
}

// When the start fails with a class the queue retries, the next attempt
// starts the transcription on the file already uploaded; Cleanup of the
// completed transcription then deletes that file too (§2a.4).
func TestRetryReusesUpload(t *testing.T) {
	f := newFake(t)
	f.transcribe = reply{503, nil, `{"detail":"CANARY"}`}
	a := adapter(t, f.server(), nil)
	req := request()
	req.Audio.SHA256 = strings.Repeat("a", 64)
	var e *providers.Error
	if _, err := run(t, a, req); !errors.As(err, &e) || e.Class != providers.Retryable {
		t.Fatalf("first attempt: %v", err)
	}
	f.setTranscribe(reply{200, nil, `{"id":"tr-1","status":"pending"}`})
	req.IdempotencyKey = "0190f5a0-0000-7000-8000-000000000001:trk-3:3"
	if _, err := run(t, a, req); err != nil {
		t.Fatalf("retry: %v", err)
	}
	want := []string{"POST /api/upload", "POST /api/transcribe", "POST /api/transcribe", "GET /api/status/tr-1",
		"GET /api/status/tr-1", "GET /api/status/tr-1", "GET /api/transcription/tr-1", "DELETE /api/transcription/tr-1"}
	if got := f.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if ids := f.startFileIDs(); len(ids) != 2 || ids[0] != "file-1" || ids[1] != "file-1" {
		t.Errorf("file IDs %v", ids)
	}
	if len(a.uploads) != 0 {
		t.Errorf("uploads kept after success: %v", a.uploads)
	}
}

// A kept file the instance no longer has (404) is uploaded again within
// the same attempt.
func TestRetryUploadsAgainWhenFileGone(t *testing.T) {
	f := newFake(t)
	f.transcribe = reply{500, nil, ""}
	a := adapter(t, f.server(), nil)
	if _, err := run(t, a, request()); err == nil {
		t.Fatal("first attempt succeeded")
	}
	f.setTranscribe(reply{404, nil, `{"detail":"File not found"}`})
	h, _ := a.Submit(context.Background(), request())
	if got := f.calls(); len(got) != 5 || got[2] != "POST /api/transcribe" || got[3] != "POST /api/upload" ||
		got[4] != "POST /api/transcribe" {
		t.Errorf("calls %v", got)
	}
	// The second start on a fresh upload answers 404 too: that is a
	// configuration error, and the file is not kept.
	var e *providers.Error
	if err := h.(*handle).err; !errors.As(err, &e) || e.Class != providers.PermanentConfig || e.Code != "http_404" {
		t.Errorf("error %+v", err)
	}
	if len(a.uploads) != 0 {
		t.Errorf("uploads kept: %v", a.uploads)
	}
}

// A file is not kept after a start error the queue does not retry, nor
// reused for other audio or after keepUploadFor.
func TestUploadNotReused(t *testing.T) {
	f := newFake(t)
	f.transcribe = reply{422, nil, `{"detail":"Could not prepare the audio for transcription."}`}
	a := adapter(t, f.server(), nil)
	if _, err := run(t, a, request()); err == nil {
		t.Fatal("422 succeeded")
	}
	if len(a.uploads) != 0 {
		t.Errorf("kept after permanent_input: %v", a.uploads)
	}

	f.setTranscribe(reply{500, nil, ""})
	_, _ = run(t, a, request())
	other := request()
	other.Audio.SHA256 = strings.Repeat("b", 64)
	if id := a.takeUpload(other); id != "" {
		t.Errorf("reused for other audio: %q", id)
	}

	_, _ = run(t, a, request())
	a.mu.Lock()
	for k, u := range a.uploads {
		u.at = time.Now().Add(-keepUploadFor - time.Minute)
		a.uploads[k] = u
	}
	a.mu.Unlock()
	if id := a.takeUpload(request()); id != "" {
		t.Errorf("reused after keepUploadFor: %q", id)
	}
}
