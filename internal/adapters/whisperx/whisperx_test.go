package whisperx

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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

const listForm = "../../../testdata/providers/whisperx/list-form.spec-derived.json"

func profile(t *testing.T, baseURL string, extra string) *providers.Profile {
	t.Helper()
	text := `schema_version: "1.0.0"
defaults: {asr: asr-test, llm: llm-test}
profiles:
  - {name: asr-test, kind: asr, dialect: whisperx-family, base_url: "` + baseURL + `", model: large-v3` + extra + `,
     declaration: {data_categories: [voice], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-10-29"}}
  - {name: llm-test, kind: llm, dialect: openai-chat, base_url: "http://llm.recap.test/v1", model: m,
     context_tokens: 100, max_output_tokens: 10,
     declaration: {data_categories: [transcript], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-10-29"}}
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

// fake answers every request with status and body and records the form.
func fake(t *testing.T, status int, header map[string]string, body []byte) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart: %v", err)
		}
		got = append(got, r)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func run(t *testing.T, a *Adapter, req providers.AsrRequest) (*providers.AsrResult, error) {
	t.Helper()
	ctx := context.Background()
	h, err := a.Submit(ctx, req)
	if err == nil {
		_, _ = a.Status(ctx, h)
		var r *providers.AsrResult
		r, err = a.Result(ctx, h)
		if cerr := a.Cleanup(ctx, h); cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
		return r, err
	}
	return nil, err
}

func TestRequestAndResult(t *testing.T) {
	body, err := os.ReadFile(listForm)
	if err != nil {
		t.Fatal(err)
	}
	srv, got := fake(t, 200, nil, body)
	var logs bytes.Buffer
	a := New(profile(t, srv.URL, ""), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	req := request()
	req.LanguageHint = "de"
	r, err := run(t, a, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("%d requests", len(*got))
	}
	hr := (*got)[0]
	if hr.URL.Path != "/v1/audio/transcriptions" || hr.Header.Get("X-Request-ID") != req.IdempotencyKey ||
		hr.Header.Get("Authorization") != "" {
		t.Errorf("request %s %v", hr.URL.Path, hr.Header)
	}
	for k, want := range map[string]string{"model": "large-v3", "language": "de", "response_format": "verbose_json",
		"align": "true", "suppress_numerals": "false", "diarize": "false"} {
		if v := hr.MultipartForm.Value[k]; len(v) != 1 || v[0] != want {
			t.Errorf("field %s = %v, want %s", k, v, want)
		}
	}
	if fh := hr.MultipartForm.File["file"]; len(fh) != 1 || fh[0].Filename != "trk-3.webm" ||
		fh[0].Header.Get("Content-Type") != "audio/webm" || fh[0].Size != int64(len(audio)) {
		t.Errorf("file part %+v", fh)
	}

	want := `{"language":"en","model":"large-v3","segments":[` +
		`{"start_ms":1000,"end_ms":5500,"text":"Welcome to the fixture meeting. Today we decide the colour of the test widget."},` +
		`{"start_ms":6063,"end_ms":7563,"text":"I propose blue."},` +
		`{"start_ms":9500,"end_ms":9500,"text":"Agreed."},` +
		`{"start_ms":10008,"end_ms":12492,"text":"Yes, I will review it on Monday."}]}`
	if b, _ := json.Marshal(r); string(b) != want {
		t.Errorf("result\n%s\nwant\n%s", b, want)
	}
	if strings.Contains(logs.String(), "widget") || !strings.Contains(logs.String(), `"msg":"asr request done"`) {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestErrorClasses(t *testing.T) {
	for _, c := range []struct {
		status int
		header map[string]string
		class  providers.ErrorClass
		retry  time.Duration
	}{
		{429, map[string]string{"Retry-After": "120"}, providers.Retryable, 120 * time.Second},
		{503, map[string]string{"Retry-After": "7200"}, providers.Retryable, 30 * time.Minute},
		{502, nil, providers.Retryable, 0},
		{507, nil, providers.Retryable, 0},
		{413, nil, providers.PermanentInput, 0},
		{404, nil, providers.PermanentConfig, 0},
		{302, map[string]string{"Location": "/elsewhere"}, providers.PermanentConfig, 0},
		{418, nil, providers.PermanentConfig, 0},
	} {
		srv, _ := fake(t, c.status, c.header, []byte(`{"detail":"CANARY-BODY"}`))
		var logs bytes.Buffer
		a := New(profile(t, srv.URL, ""), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		_, err := run(t, a, request())
		var e *providers.Error
		if !errors.As(err, &e) || e.Class != c.class || e.RetryAfter != c.retry {
			t.Errorf("HTTP %d: error %+v, want %s retry %v", c.status, e, c.class, c.retry)
		}
		if strings.Contains(logs.String(), "CANARY") || strings.Contains(err.Error(), "CANARY") {
			t.Errorf("HTTP %d: error body leaked", c.status)
		}
	}
}

func TestInvalidResponses(t *testing.T) {
	for _, body := range []string{
		`not json`, `null`, `{"language":"en"}`, `{"segments":"x"}`, `{"segments":{"segments":3}}`,
		`{"segments":[{"start":1,"text":"a"}]}`, `{"segments":[{"start":-1,"end":2,"text":"a"}]}`,
		`{"segments":[{"start":1,"end":2}]}`, `{"segments":[{"start":1,"end":2,"text":7}]}`,
	} {
		srv, _ := fake(t, 200, nil, []byte(body))
		a := New(profile(t, srv.URL, ""), slog.New(slog.DiscardHandler))
		_, err := run(t, a, request())
		var e *providers.Error
		if !errors.As(err, &e) || e.Class != providers.InvalidResponse {
			t.Errorf("%s: error %v, want invalid_response", body, err)
		}
	}
}

func TestTimeoutAndLimits(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	a := New(profile(t, srv.URL, ""), slog.New(slog.DiscardHandler))
	req := request()
	req.AttemptTimeout = 200 * time.Millisecond
	_, err := run(t, a, req)
	var e *providers.Error
	if !errors.As(err, &e) || e.Class != providers.Retryable || e.Code != "timeout" || e.RetryAfter < 150*time.Second {
		t.Errorf("timeout: %+v", e)
	}

	// Above max_file_bytes: permanent_input without a request.
	small, got := fake(t, 200, nil, []byte(`{"segments":[]}`))
	a = New(profile(t, small.URL, ", max_file_bytes: 4"), slog.New(slog.DiscardHandler))
	if _, err := run(t, a, request()); !errors.As(err, &e) || e.Class != providers.PermanentInput || len(*got) != 0 {
		t.Errorf("max_file_bytes: %v, %d requests", err, len(*got))
	}

	if AttemptTimeout(9000) != 600*time.Second || AttemptTimeout(3_600_000) != 1800*time.Second {
		t.Error("AttemptTimeout does not follow max(600 s, 0.5 × duration)")
	}
}

// The file part carries the request's media type and a matching file
// extension; a media type outside Capabilities is permanent_input without a
// request.
func TestMediaType(t *testing.T) {
	if c := New(profile(t, "http://asr.recap.test", ""), slog.New(slog.DiscardHandler)).Capabilities(); !slices.Equal(c.MediaTypes, []string{"audio/webm"}) {
		t.Errorf("media types %v", c.MediaTypes)
	}

	fileExtensions["audio/ogg"] = ".ogg" // a second type, for this test only
	defer delete(fileExtensions, "audio/ogg")
	srv, got := fake(t, 200, nil, []byte(`{"segments":[]}`))
	a := New(profile(t, srv.URL, ""), slog.New(slog.DiscardHandler))
	req := request()
	req.Audio.MediaType = "audio/ogg"
	if _, err := run(t, a, req); err != nil {
		t.Fatal(err)
	}
	if fh := (*got)[0].MultipartForm.File["file"]; len(fh) != 1 || fh[0].Filename != "trk-3.ogg" ||
		fh[0].Header.Get("Content-Type") != "audio/ogg" {
		t.Errorf("file part %+v", fh)
	}

	for _, mt := range []string{"audio/mpeg", "", "audio/webm; codecs=opus"} {
		srv, got := fake(t, 200, nil, []byte(`{"segments":[]}`))
		a := New(profile(t, srv.URL, ""), slog.New(slog.DiscardHandler))
		req := request()
		req.Audio.MediaType = mt
		_, err := run(t, a, req)
		var e *providers.Error
		if !errors.As(err, &e) || e.Class != providers.PermanentInput || e.Code != "unsupported_media_type" ||
			len(*got) != 0 {
			t.Errorf("%q: %v, %d requests", mt, err, len(*got))
		}
	}
}

// Status and Result make no network call; Cleanup works twice in any state.
func TestHandleWithoutNetwork(t *testing.T) {
	srv, got := fake(t, 503, nil, nil)
	a := New(profile(t, srv.URL, ""), slog.New(slog.DiscardHandler))
	ctx := context.Background()
	h, err := a.Submit(ctx, request())
	if err == nil {
		t.Fatal("want an error from 503")
	}
	if st, err := a.Status(ctx, h); st != providers.StatusFailed || err != nil {
		t.Errorf("status %s %v", st, err)
	}
	if _, err := a.Result(ctx, h); err == nil {
		t.Error("result after a failed submit: want the error")
	}
	for range 2 {
		if err := a.Cleanup(ctx, h); err != nil {
			t.Error(err)
		}
	}
	if len(*got) != 1 {
		t.Errorf("%d requests, want 1", len(*got))
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	d := retryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat))
	if d < 85*time.Second || d > 91*time.Second {
		t.Errorf("retryAfter(date) = %v", d)
	}
	if retryAfter("soon") != 0 || retryAfter("") != 0 {
		t.Error("invalid Retry-After must give 0")
	}
}
