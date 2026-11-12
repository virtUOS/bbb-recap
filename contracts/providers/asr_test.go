package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Fixed request values. The job_id is a UUIDv7 shape (README "Identifiers").
const (
	jobID          = "0190f5a0-0000-7000-8000-000000000001"
	trackRef       = "trk-1"
	idempotencyKey = jobID + ":" + trackRef + ":1" // <job_id>:<track_ref>:<attempt>, §1.2
	asrModel       = "large-v3"                    // whisperx-lab model, §4.1
)

// asrAudio is F-01's first microphone file (fixtures.md §3.1).
var asrAudio = []string{"meetings", "F-01", "raw", "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000",
	"audio", "microphone-w_fixada0001-TR_fixA1-1761732005200.webm"}

// Canaries in provider error bodies. They must never reach logs or output.
const (
	canaryDetail = "CANARY-ASR-ERROR-BODY-7f3a"
	canaryMsg    = "CANARY-ASR-ERROR-MSG-91c2"
)

var allOps = []string{"submit", "status", "result", "cleanup"}

type asrSegment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

type asrResult struct {
	Language *string      `json:"language"`
	Model    string       `json:"model"`
	Segments []asrSegment `json:"segments"`
}

type asrOp struct {
	Op     string          `json:"op"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *adapterError   `json:"error"`
}

type asrOutput struct {
	Operations []asrOp `json:"operations"`
}

// firstError returns the first error in operation order, or nil.
func (o asrOutput) firstError() *adapterError {
	for _, op := range o.Operations {
		if op.Error != nil {
			return op.Error
		}
	}
	return nil
}

// result returns the decoded AsrResult of the first result operation.
func (o asrOutput) result(t *testing.T) asrResult {
	t.Helper()
	for _, op := range o.Operations {
		if op.Op != "result" {
			continue
		}
		if op.Error != nil {
			t.Fatalf("result: error %+v, want an AsrResult", *op.Error)
		}
		var raw struct {
			Segments []map[string]json.RawMessage `json:"segments"`
		}
		if err := json.Unmarshal(op.Result, &raw); err != nil {
			t.Fatalf("result: %v in %s", err, op.Result)
		}
		for i, s := range raw.Segments {
			if _, ok := s["speaker"]; ok {
				t.Errorf("result segment %d has a speaker; provider labels are ignored (§1.3)", i)
			}
		}
		var r asrResult
		if err := json.Unmarshal(op.Result, &r); err != nil {
			t.Fatalf("result is not an AsrResult with integer ms times: %v\n%s", err, op.Result)
		}
		return r
	}
	t.Fatal("no result operation in output")
	return asrResult{}
}

// asrCase is one tool run against a fake whisperx server.
type asrCase struct {
	ops         []string
	asrLanguage string
	timeoutMS   int64
	edit        func(profiles) profiles // changes to the profiles fixture
	baseURL     string                  // overrides the fake's URL
	certFile    string                  // overrides the fake's certificate
}

// runASR runs `recap tool asr` against fake and returns the decoded output,
// stdout and stderr.
func runASR(t *testing.T, bin string, fake *fakeProvider, c asrCase) (asrOutput, []byte, []byte) {
	t.Helper()
	base, cert := c.baseURL, c.certFile
	if base == "" {
		base = fake.url()
	}
	if cert == "" {
		cert = fake.certFile(t)
	}
	p := loadProfiles(t).set(t, "base_url", asrProfile, base)
	if c.edit != nil {
		p = c.edit(p)
	}
	ops := c.ops
	if ops == nil {
		ops = allOps
	}
	audio := testdataPath(t, asrAudio...)
	b := readFile(t, audio)
	sum := sha256.Sum256(b)
	in := map[string]any{
		"profile": "whisperx-lab",
		"request": map[string]any{
			"job_id":          jobID,
			"track_ref":       trackRef,
			"idempotency_key": idempotencyKey,
			"audio": map[string]any{
				"path": audio, "media_type": "audio/webm", "size": len(b), "sha256": hex.EncodeToString(sum[:]),
			},
			"duration_ms": 9000,
		},
		"operations": ops,
	}
	if c.timeoutMS > 0 {
		in["attempt_timeout_ms"] = c.timeoutMS
	}
	env := toolEnv{providersFile: p.write(t), certFile: cert, asrLanguage: c.asrLanguage}
	var out asrOutput
	stdout, stderr := runJSON(t, bin, env, in, &out, "asr")
	if len(out.Operations) != len(ops) {
		t.Fatalf("output has %d operations, want %d: %s", len(out.Operations), len(ops), stdout)
	}
	for i, op := range out.Operations {
		if op.Op != ops[i] {
			t.Fatalf("operation %d is %q, want %q", i, op.Op, ops[i])
		}
	}
	return out, stdout, stderr
}

func okReply(t *testing.T, fixture string) reply {
	t.Helper()
	return reply{status: http.StatusOK, body: readFile(t, testdataPath(t, "providers", "whisperx", fixture))}
}

func expectedResult(t *testing.T) asrResult {
	t.Helper()
	var r asrResult
	if err := json.Unmarshal(readFile(t, testdataPath(t, "providers", "whisperx", "result.json")), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func expectResult(t *testing.T, got, want asrResult) {
	t.Helper()
	if (got.Language == nil) != (want.Language == nil) || (got.Language != nil && *got.Language != *want.Language) {
		t.Errorf("language = %v, want %v", deref(got.Language), deref(want.Language))
	}
	if got.Model != want.Model {
		t.Errorf("model = %q, want %q", got.Model, want.Model)
	}
	if !slices.Equal(got.Segments, want.Segments) {
		t.Errorf("segments = %+v\nwant       %+v", got.Segments, want.Segments)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// formPart is one part of a recorded multipart/form-data request.
type formPart struct {
	fileName, contentType string
	value                 []byte
}

// parseMultipart parses a recorded multipart/form-data request by field name.
func parseMultipart(t *testing.T, ex exchange) map[string][]formPart {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ex.header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, want multipart/form-data", ex.header.Get("Content-Type"))
	}
	r := multipart.NewReader(bytes.NewReader(ex.body), params["boundary"])
	parts := map[string][]formPart{}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("multipart body: %v", err)
		}
		v, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("multipart part %q: %v", p.FormName(), err)
		}
		parts[p.FormName()] = append(parts[p.FormName()], formPart{p.FileName(), p.Header.Get("Content-Type"), v})
	}
	return parts
}

func oneRequest(t *testing.T, f *fakeProvider) exchange {
	t.Helper()
	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("provider received %d requests, want exactly 1 (no client retries, ADR-008)", len(got))
	}
	return got[0]
}

func TestASR(t *testing.T) {
	t.Run("CT-PROV-01 whisperx request", func(t *testing.T) { testASRRequest(t, harness.Bin(t)) })
	t.Run("CT-PROV-02 whisperx list form (provisional)", func(t *testing.T) { testListForm(t, harness.Bin(t)) })
	t.Run("CT-PROV-03 whisperx object form (provisional)", func(t *testing.T) { testObjectForm(t, harness.Bin(t)) })
	t.Run("CT-PROV-04 ASR error classes", func(t *testing.T) { testASRErrors(t, harness.Bin(t)) })
	t.Run("CT-PROV-05 segment without times (provisional)", func(t *testing.T) { testMissingTimes(t, harness.Bin(t)) })
	t.Run("CT-PROV-06 cleanup and no network in status and result", func(t *testing.T) { testHandle(t, harness.Bin(t)) })
}

// §2.2: method, path, multipart fields, headers, file name; language only
// from RECAP_ASR_LANGUAGE; never diarization or prompt fields.
func testASRRequest(t *testing.T, bin string) {
	key := fixtureKey(t, "asr-fixture.key")
	audio := readFile(t, testdataPath(t, asrAudio...))

	check := func(t *testing.T, c asrCase, wantAuth bool, wantLang string) {
		t.Helper()
		f := startFake(t, okReply(t, "list-form.spec-derived.json"))
		_, stdout, stderr := runASR(t, bin, f, c)
		ex := oneRequest(t, f)
		if ex.method != http.MethodPost || ex.path != "/v1/audio/transcriptions" {
			t.Errorf("request = %s %s, want POST /v1/audio/transcriptions", ex.method, ex.path)
		}
		auth := ex.header.Get("Authorization")
		switch {
		case wantAuth && auth != "Bearer "+key:
			t.Errorf("Authorization = %q, want Bearer <asr-fixture.key>", auth)
		case !wantAuth && auth != "":
			t.Errorf("Authorization = %q, want none without credentials_file", auth)
		}
		if got := ex.header.Get("X-Request-ID"); got != idempotencyKey {
			t.Errorf("X-Request-ID = %q, want %q", got, idempotencyKey)
		}
		parts := parseMultipart(t, ex)
		files := parts["file"]
		if len(files) != 1 {
			t.Fatalf("got %d file parts, want 1", len(files))
		}
		if files[0].fileName != trackRef+".webm" {
			t.Errorf("file name = %q, want %q (never the BBB file name)", files[0].fileName, trackRef+".webm")
		}
		if files[0].contentType != "audio/webm" {
			t.Errorf("file Content-Type = %q, want audio/webm", files[0].contentType)
		}
		if !bytes.Equal(files[0].value, audio) {
			t.Errorf("file part (%d bytes) differs from the audio file (%d bytes)", len(files[0].value), len(audio))
		}
		fields := map[string]string{
			"model": asrModel, "response_format": "verbose_json", "align": "true",
			"suppress_numerals": "false", "diarize": "false",
		}
		if wantLang != "" {
			fields["language"] = wantLang
		} else if _, ok := parts["language"]; ok {
			t.Errorf("language field sent although RECAP_ASR_LANGUAGE is empty (auto-detect)")
		}
		for name, want := range fields {
			v := parts[name]
			if len(v) != 1 || string(v[0].value) != want {
				t.Errorf("field %s = %v, want exactly one %q", name, values(v), want)
			}
		}
		for _, name := range []string{"min_speakers", "max_speakers", "prompt", "hotwords", "audio_url", "callback_url", "async"} {
			if _, ok := parts[name]; ok {
				t.Errorf("field %s sent; never sent (§2.2)", name)
			}
		}
		expectNotLeaked(t, stdout, stderr, key)
	}

	t.Run("default-no-language", func(t *testing.T) { check(t, asrCase{}, true, "") })
	t.Run("RECAP_ASR_LANGUAGE-de", func(t *testing.T) { check(t, asrCase{asrLanguage: "de"}, true, "de") })
	t.Run("no-credentials_file-no-Authorization", func(t *testing.T) {
		check(t, asrCase{edit: func(p profiles) profiles { return p.without(t, "credentials_file", asrProfile) }}, false, "")
	})
}

func values(parts []formPart) []string {
	var v []string
	for _, p := range parts {
		v = append(v, string(p.value))
	}
	return v
}

// §1.3, §2.3: list-form reply → AsrResult (rounding with ties away from zero,
// trim, empty dropped, end < start clamped, speaker ignored, profile model).
func testListForm(t *testing.T, bin string) {
	t.Run("fixture", func(t *testing.T) {
		f := startFake(t, okReply(t, "list-form.spec-derived.json"))
		out, _, stderr := runASR(t, bin, f, asrCase{})
		if e := out.firstError(); e != nil {
			t.Fatalf("error %+v, want success", *e)
		}
		want := expectedResult(t)
		expectResult(t, out.result(t), want)
		// Log scan: no segment text at INFO or above.
		var texts []string
		for _, s := range want.Segments {
			texts = append(texts, s.Text)
		}
		expectNoContentAtInfo(t, stderr, texts...)
	})
	// language: kept if it matches ^[a-z]{2,3}$, else null.
	for _, c := range []struct {
		name string
		lang any // nil: field absent
		want *string
	}{
		{"language-deu-kept", "deu", ptr("deu")},
		{"language-English-null", "English", nil},
		{"language-EN-null", "EN", nil},
		{"language-absent-null", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(readFile(t, testdataPath(t, "providers", "whisperx", "list-form.spec-derived.json")), &doc); err != nil {
				t.Fatal(err)
			}
			if c.lang == nil {
				delete(doc, "language")
			} else {
				doc["language"] = c.lang
			}
			body, _ := json.Marshal(doc)
			f := startFake(t, reply{status: http.StatusOK, body: body})
			out, _, _ := runASR(t, bin, f, asrCase{})
			want := expectedResult(t)
			want.Language = c.want
			expectResult(t, out.result(t), want)
		})
	}
}

func ptr(s string) *string { return &s }

// §2.3: the object form {segments, word_segments} maps to the same AsrResult.
func testObjectForm(t *testing.T, bin string) {
	f := startFake(t, okReply(t, "object-form.spec-derived.json"))
	out, _, _ := runASR(t, bin, f, asrCase{})
	if e := out.firstError(); e != nil {
		t.Fatalf("error %+v, want success", *e)
	}
	expectResult(t, out.result(t), expectedResult(t))
}

// §1.4: status codes, connect refusal, TLS failure, timeout, non-JSON → error
// class; Retry-After parsed; one request only; error bodies never in output or logs.
func testASRErrors(t *testing.T, bin string) {
	key := fixtureKey(t, "asr-fixture.key")
	stringBody := []byte(`{"detail": "` + canaryDetail + `"}`)
	listBody := []byte(`{"detail": [{"loc": ["body", "file"], "msg": "` + canaryMsg + `"}]}`)
	classes := map[int]string{
		408: "retryable", 429: "retryable", 500: "retryable", 502: "retryable", 503: "retryable", 504: "retryable",
		400: "permanent_input", 413: "permanent_input", 415: "permanent_input", 422: "permanent_input",
		401: "permanent_config", 403: "permanent_config", 404: "permanent_config",
	}
	for _, status := range []int{400, 401, 403, 404, 408, 413, 415, 422, 429, 500, 502, 503, 504} {
		for name, body := range map[string][]byte{"detail-string": stringBody, "detail-list": listBody} {
			t.Run(http.StatusText(status)+"/"+name, func(t *testing.T) {
				f := startFake(t, reply{status: status, body: body})
				out, stdout, stderr := runASR(t, bin, f, asrCase{})
				oneRequest(t, f)
				e := out.firstError()
				if e == nil || e.Class != classes[status] {
					t.Fatalf("HTTP %d: error %+v, want class %s", status, e, classes[status])
				}
				expectNotLeaked(t, stdout, stderr, canaryDetail, canaryMsg, key)
			})
		}
	}

	t.Run("Retry-After", func(t *testing.T) {
		for _, c := range []struct {
			status int
			header string
			want   int64
		}{{429, "120", 120000}, {503, "30", 30000}, {503, "7200", 1800000}} { // capped at 30 min by the adapter
			f := startFake(t, reply{status: c.status, header: map[string]string{"Retry-After": c.header}, body: stringBody})
			out, _, _ := runASR(t, bin, f, asrCase{})
			e := out.firstError()
			if e == nil || e.Class != "retryable" || e.RetryAfterMS == nil || *e.RetryAfterMS != c.want {
				t.Errorf("HTTP %d Retry-After %s: error %+v, want retryable with retry_after_ms %d", c.status, c.header, e, c.want)
			}
		}
	})

	t.Run("file-above-max_file_bytes", func(t *testing.T) {
		// The F-01 audio file is 93 076 bytes; the profile allows 1024.
		f := startFake(t, okReply(t, "list-form.spec-derived.json"))
		out, _, _ := runASR(t, bin, f, asrCase{edit: func(p profiles) profiles {
			return p.set(t, "max_file_bytes", 1, "1024")
		}})
		if e := out.firstError(); e == nil || e.Class != "permanent_input" {
			t.Fatalf("file above max_file_bytes: error %+v, want permanent_input", e)
		}
		if n := len(f.requests()); n != 0 {
			t.Errorf("provider received %d requests, want none for a file above max_file_bytes", n)
		}
	})

	t.Run("connect-refused", func(t *testing.T) {
		f := startFake(t, okReply(t, "list-form.spec-derived.json")) // only for its certificate
		out, _, _ := runASR(t, bin, f, asrCase{baseURL: closedURL(t)})
		if e := out.firstError(); e == nil || e.Class != "retryable" {
			t.Fatalf("connection refused: error %+v, want retryable", e)
		}
	})

	t.Run("tls-untrusted", func(t *testing.T) {
		f := startFake(t, okReply(t, "list-form.spec-derived.json"))
		out, _, _ := runASR(t, bin, f, asrCase{certFile: otherCertFile(t)})
		if e := out.firstError(); e == nil || e.Class != "retryable" {
			t.Fatalf("untrusted certificate: error %+v, want retryable", e)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		// The fake answers successfully after 20 s; the attempt times out after 1 s.
		f := startFake(t, reply{status: http.StatusOK, body: okReply(t, "list-form.spec-derived.json").body, hold: 20 * time.Second})
		start := time.Now()
		out, _, _ := runASR(t, bin, f, asrCase{timeoutMS: 1000})
		if e := out.firstError(); e == nil || e.Class != "retryable" {
			t.Fatalf("timeout: error %+v, want retryable", e)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Errorf("tool took %v; attempt_timeout_ms 1000 not applied", d)
		}
		oneRequest(t, f)
	})

	for name, body := range map[string]string{
		"non-JSON":      "<html>" + canaryDetail + "</html>",
		"JSON-not-obj":  `["` + canaryDetail + `"]`,
		"segments-text": `{"text": "x", "language": "en", "segments": "` + canaryDetail + `"}`,
	} {
		t.Run("invalid-200/"+name, func(t *testing.T) {
			f := startFake(t, reply{status: http.StatusOK, body: []byte(body)})
			out, stdout, stderr := runASR(t, bin, f, asrCase{})
			if e := out.firstError(); e == nil || e.Class != "invalid_response" {
				t.Fatalf("200 with %s: error %+v, want invalid_response", name, e)
			}
			oneRequest(t, f)
			expectNotLeaked(t, stdout, stderr, canaryDetail)
		})
	}
}

// §2.3: a segment without start or end, or with a non-number time, is
// invalid_response (no default 0).
func testMissingTimes(t *testing.T, bin string) {
	for _, c := range []struct {
		name  string
		key   string
		value any // nil: delete the key
	}{
		{"start-missing", "start", nil},
		{"end-missing", "end", nil},
		{"start-string", "start", "1.0"},
		{"end-null", "end", json.RawMessage("null")},
	} {
		t.Run(c.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(readFile(t, testdataPath(t, "providers", "whisperx", "list-form.spec-derived.json")), &doc); err != nil {
				t.Fatal(err)
			}
			seg := doc["segments"].([]any)[1].(map[string]any)
			if c.value == nil {
				delete(seg, c.key)
			} else {
				seg[c.key] = c.value
			}
			body, _ := json.Marshal(doc)
			f := startFake(t, reply{status: http.StatusOK, body: body})
			out, _, _ := runASR(t, bin, f, asrCase{})
			if e := out.firstError(); e == nil || e.Class != "invalid_response" {
				t.Fatalf("segment %s: error %+v, want invalid_response", c.name, e)
			}
		})
	}
}

// §1, §2.4: cleanup succeeds twice in any state and, for whisperx-family, is a
// no-op; status and result make no network call (synchronous dialect).
func testHandle(t *testing.T, bin string) {
	for _, c := range []struct {
		name   string
		rep    func(t *testing.T) reply
		ops    []string
		status string // expected status of the status operation, if any
		failed bool   // submit failed: status must be failed or report an error (§1, §1.4)
	}{
		{"after-result", func(t *testing.T) reply { return okReply(t, "list-form.spec-derived.json") },
			[]string{"submit", "status", "status", "result", "result", "cleanup", "cleanup"}, "succeeded", false},
		{"after-submit", func(t *testing.T) reply { return okReply(t, "list-form.spec-derived.json") },
			[]string{"submit", "cleanup", "cleanup"}, "", false},
		{"after-failed-submit", func(*testing.T) reply { return reply{status: 503, body: []byte(`{"detail": "x"}`)} },
			[]string{"submit", "status", "result", "cleanup", "cleanup"}, "", true},
		{"after-invalid-response", func(*testing.T) reply { return reply{status: 200, body: []byte(`not json`)} },
			[]string{"submit", "status", "result", "cleanup", "cleanup"}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := startFake(t, c.rep(t))
			out, _, _ := runASR(t, bin, f, asrCase{ops: c.ops})
			oneRequest(t, f)
			for _, op := range out.Operations {
				switch op.Op {
				case "cleanup":
					if op.Error != nil {
						t.Errorf("cleanup: error %+v, want ok", *op.Error)
					}
				case "status":
					if c.status != "" && op.Status != c.status {
						t.Errorf("status = %q, want %q", op.Status, c.status)
					}
					if op.Error == nil && !slices.Contains([]string{"queued", "running", "succeeded", "failed"}, op.Status) {
						t.Errorf("status = %q, not a §1 status", op.Status)
					}
					if c.failed && op.Error == nil && op.Status != "failed" {
						t.Errorf("status after a failed submit = %q, want failed (or an error)", op.Status)
					}
				}
			}
			if c.name == "after-result" {
				if e := out.firstError(); e != nil {
					t.Fatalf("error %+v, want success", *e)
				}
				if r := out.result(t); len(r.Segments) == 0 {
					t.Errorf("result has no segments")
				}
			}
		})
	}
}
