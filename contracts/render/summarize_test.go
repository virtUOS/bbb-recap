package render

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// providersYAML is the fixture providers file of providers.md §4.1 with three
// changes for the fake: base_url points at it, credentials_file at the
// fixture keys, and structured_output is json_schema, so every request names
// its output schema (§3.3) and the fake can tell extract calls (chunk-notes.v1,
// property chunk_summary) from summarize/consolidate calls (summary.v1,
// property overview).
const providersYAML = `schema_version: "1.0.0"
defaults: { asr: whisperx-lab, llm: vllm-lab }
profiles:
  - name: whisperx-lab
    kind: asr
    dialect: whisperx-family
    base_url: https://asr.recap.test
    model: large-v3
    credentials_file: %s
    max_inflight: 1
    declaration: { data_categories: [voice], operator: "operator's computing centre", location: on_prem,
                   retention: "No persistence; RESULT_STORE__ENABLED=false", deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: fixture-reviewer, reviewed_at: "2026-10-29" }
  - name: vllm-lab
    kind: llm
    dialect: openai-chat
    base_url: %s/v1
    model: example-model
    credentials_file: %s
    context_tokens: 32768
    max_output_tokens: 4096
    structured_output: json_schema
    extra_body: { chat_template_kwargs: { enable_thinking: false } }
    max_inflight: 4
    declaration: { data_categories: [transcript, chat, summary], operator: "operator's computing centre",
                   location: on_prem, retention: "No request logging (operator to confirm)", deletion: none,
                   used_for_training: "no", contract_ref: none, reviewed_by: fixture-reviewer, reviewed_at: "2026-10-29" }
`

// llmCall is one request the fake LLM received.
type llmCall struct {
	kind string // "extract" (chunk-notes.v1) or "summary" (summary.v1)
	text string // all message contents, joined
	raw  string // the request body
}

// fakeLLM answers chat completions (providers.md §3.1-3.2) with fixture
// replies: chunk-notes.v1 for extract calls, F-01's summary.json otherwise.
type fakeLLM struct {
	summary, chunk string
	mu             sync.Mutex
	calls          []llmCall
	errs           []string
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fail := func(format string, args ...any) {
		f.mu.Lock()
		f.errs = append(f.errs, fmt.Sprintf(format, args...))
		f.mu.Unlock()
		http.Error(w, "fake LLM: unexpected request", http.StatusBadRequest)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		fail("unexpected %s %s", r.Method, r.URL.Path)
		return
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail("request body is not JSON: %v", err)
		return
	}
	var text strings.Builder
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) != nil {
			s = string(m.Content)
		}
		text.WriteString(s + "\n")
	}
	props := req.ResponseFormat.JSONSchema.Schema.Properties
	var kind, reply string
	switch {
	case req.ResponseFormat.Type != "json_schema":
		fail("response_format.type = %q, want json_schema (profile structured_output, providers.md §3.3)", req.ResponseFormat.Type)
		return
	case props["chunk_summary"] != nil:
		kind, reply = "extract", f.chunk
	case props["overview"] != nil:
		kind, reply = "summary", f.summary
	default:
		fail("output schema is neither chunk-notes.v1 nor summary.v1")
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, llmCall{kind: kind, text: text.String(), raw: string(body)})
	f.mu.Unlock()
	resp, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-fixture", "object": "chat.completion", "created": 0, "model": "example-model",
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// summarize runs `recap tool render --summarize` against a TLS fake LLM and
// returns the verdict and the calls the fake saw. env overrides the defaults.
func summarize(t *testing.T, bin string, input any, env map[string]string) (rendered, []llmCall) {
	t.Helper()
	f := &fakeLLM{
		summary: string(golden(t, "F-01", "summary.json")),
		chunk:   string(golden(t, "llm", "chunk-notes.v1", "valid.base.json")),
	}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	secrets, err := filepath.Abs(filepath.Join(testdataDir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	providers := filepath.Join(dir, "providers.yaml")
	writeFile(t, providers, []byte(fmt.Sprintf(providersYAML,
		filepath.Join(secrets, "asr-fixture.key"), srv.URL, filepath.Join(secrets, "llm-fixture.key"))))
	e := map[string]string{"RECAP_PROVIDERS_FILE": providers, "SSL_CERT_FILE": ca}
	for k, v := range env {
		e[k] = v
	}
	res := renderTool(t, bin, run{args: []string{"--summarize"}, env: e, input: input})
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, msg := range f.errs {
		t.Errorf("fake LLM: %s", msg)
	}
	return res, f.calls
}

// readOutputs reads both outputs after outcome ok and validates the .vtt.
func readOutputs(t *testing.T, res *rendered) {
	t.Helper()
	if res.Outcome != "ok" {
		t.Fatalf("outcome = %q (reason %q), want ok", res.Outcome, res.Reason)
	}
	res.summaryMD = readFile(t, filepath.Join(res.outDir, "summary.md"))
	res.transcript = readFile(t, filepath.Join(res.outDir, "transcript.vtt"))
	for _, e := range validateVTT(res.transcript) {
		t.Errorf("rendered transcript.vtt breaks WebVTT syntax (templates.md §6): %s", e)
	}
}

// Fixture content used to find inputs in LLM requests.
const (
	chatText   = "Link to the widget spec will follow."
	notesText  = "Decision: the test widget is blue."
	pollText   = "Blue or green?"
	pauseChat  = "Message sent while the recording was paused."
	chunkNotes = "Chunk notes fixture: Ben proposed blue"
)

// f01Summarize is F-01 without a summary (the LLM makes it) plus one chat
// message in the pause (axis 70 000 ms, between the periods), which must
// not reach the LLM.
func f01Summarize(t *testing.T) renderCase {
	t.Helper()
	c := loadCase(t, "F-01").clone(t)
	c.summary = nil
	c.context["chat"] = append(c.context["chat"].([]any), object{
		"at_ms": 70000, "sender_id": "w_fixben0002", "name": "Ben Testperson", "text": pauseChat,
	})
	return c
}

// longMeeting is F-01 with one long recording period and 800 segments of
// about 500 characters (about 135 000 tokens at chars/3), far over the budget
// of the fixture profile (32 768 context tokens, providers.md §3.5). Each
// segment starts with a unique marker "Long meeting line NNNN:".
func longMeeting(t *testing.T) renderCase {
	t.Helper()
	c := f01Summarize(t)
	c.context["chat"] = c.context["chat"].([]any)[:2]
	tr := c.transcript
	tr["recording_periods"] = []any{object{"start_ms": 0, "end_ms": 3600000, "recorded_offset_ms": 0}}
	tracks := tr["tracks"].([]any)[:2]
	for _, x := range tracks {
		x.(object)["start_ms"], x.(object)["end_ms"] = 0, 3600000
	}
	tr["tracks"] = tracks
	filler := strings.Repeat("The group keeps testing the blue widget and notes each result. ", 7)
	var segs []any
	for i := 1; i <= 800; i++ {
		trk, spk := "trk-1", "spk-1"
		if i%2 == 0 {
			trk, spk = "trk-2", "spk-2"
		}
		segs = append(segs, object{
			"id": fmt.Sprintf("seg-%06d", i), "track_ref": trk, "speaker_ref": spk,
			"start_ms": i * 4000, "end_ms": i*4000 + 3500, "text": fmt.Sprintf("Long meeting line %04d: %s", i, filler),
		})
	}
	tr["segments"] = segs
	return c
}

// Call plan (templates.md §4, providers.md §3.5): one summarize call under
// the budget; extract per chunk then one consolidate over the budget, with
// chat, notes and polls only in consolidate; no call without speech.
func TestCallPlan(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-03", func(t *testing.T) {
		t.Run("under-budget", func(t *testing.T) {
			res, calls := summarize(t, bin, f01Summarize(t).input(), nil)
			readOutputs(t, &res)
			if len(calls) != 1 || calls[0].kind != "summary" {
				t.Fatalf("calls = %s, want one summary.v1 call", kinds(calls))
			}
			for _, s := range []string{
				// Transcript lines "[hh:mm:ss] Name: text" in recorded time (§3, §5.3).
				"[00:00:01] Ada Testperson: Welcome to the fixture meeting. Today we decide the colour of the test widget.",
				"[00:00:56] Ada Testperson: Agreed, blue it is. Ben, please update the widget specification by Friday.",
				"[00:00:07] Ben Testperson: " + chatText,
				notesText, pollText, "Fixture Widget Meeting",
			} {
				if !strings.Contains(calls[0].text, s) {
					t.Errorf("summarize call lacks %q", s)
				}
			}
			if strings.Contains(calls[0].raw, pauseChat) {
				t.Errorf("chat from a recording pause reached the LLM (templates.md §3)")
			}
			// The fake returned F-01's summary.json, so the document is the golden.
			wantBytes(t, "summary.md", res.summaryMD, golden(t, "F-01", "summary.md"))
		})
		t.Run("over-budget", func(t *testing.T) {
			res, calls := summarize(t, bin, longMeeting(t).input(), nil)
			readOutputs(t, &res)
			if len(calls) < 3 || calls[len(calls)-1].kind != "summary" {
				t.Fatalf("calls = %s, want two or more extract calls, then one consolidate (summary.v1) call", kinds(calls))
			}
			extracts, consolidate := calls[:len(calls)-1], calls[len(calls)-1]
			for i, c := range extracts {
				if c.kind != "extract" {
					t.Fatalf("calls = %s, want one summary.v1 call, the last", kinds(calls))
				}
				for _, s := range []string{chatText, notesText, pollText} {
					if strings.Contains(c.raw, s) {
						t.Errorf("extract call %d contains %q (chat, notes and polls go only to consolidate)", i+1, s)
					}
				}
			}
			for _, s := range []string{chatText, notesText, pollText, chunkNotes} {
				if !strings.Contains(consolidate.text, s) {
					t.Errorf("consolidate call lacks %q", s)
				}
			}
			for i := 1; i <= 800; i++ {
				marker := fmt.Sprintf("Long meeting line %04d:", i)
				n := 0
				for _, c := range extracts {
					n += strings.Count(c.text, marker)
				}
				if n != 1 {
					t.Errorf("%q appears in %d extract calls, want exactly 1 (no overlap, nothing lost)", marker, n)
				}
				if strings.Contains(consolidate.text, marker) {
					t.Errorf("consolidate call contains transcript line %q", marker)
				}
			}
		})
		t.Run("no-speech", func(t *testing.T) {
			c := f01Summarize(t)
			c.transcript["segments"] = []any{}
			for _, x := range c.transcript["tracks"].([]any) {
				x.(object)["status"] = "empty"
			}
			res, calls := summarize(t, bin, c.input(), nil)
			readOutputs(t, &res)
			if len(calls) != 0 {
				t.Errorf("calls = %s, want none when the transcript has no segments", kinds(calls))
			}
			if len(res.summaryMD) == 0 {
				t.Errorf("summary.md is empty, want the no-speech notice")
			}
		})
	})
}

func kinds(calls []llmCall) string {
	var k []string
	for _, c := range calls {
		k = append(k, c.kind)
	}
	return fmt.Sprintf("%d %v", len(calls), k)
}

// LLM requests carry no BBB user IDs, no email or owner identity and no
// per-user poll answers (templates.md §3, providers.md §5). A third voter,
// Cleo, appears only in the poll responses: her name or ID in a request would
// be a per-user answer.
func TestLLMPrivacy(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-05", func(t *testing.T) {
		forbidden := []string{
			"w_fix", "w_fixcleo0003", "Cleo Pollvoter", // BBB user IDs, per-user poll answer
			"@example.org", "ada.testperson", "fixture-sub-", // owner email and sub
		}
		for name, c := range map[string]renderCase{"under-budget": f01Summarize(t), "over-budget": longMeeting(t)} {
			t.Run(name, func(t *testing.T) {
				poll := c.context["polls"].([]any)[0].(object)
				poll["responses"] = append(poll["responses"].([]any), object{
					"user_id": "w_fixcleo0003", "name": "Cleo Pollvoter", "option": 1,
				})
				res, calls := summarize(t, bin, c.input(), nil)
				readOutputs(t, &res)
				if len(calls) == 0 {
					t.Fatal("no LLM call")
				}
				seen := false
				for i, call := range calls {
					for _, s := range forbidden {
						if strings.Contains(call.raw, s) {
							t.Errorf("call %d (%s) contains %q", i+1, call.kind, s)
						}
					}
					seen = seen || strings.Contains(call.text, pollText)
				}
				if !seen {
					t.Errorf("no call carries the poll %q (aggregates are a declared input)", pollText)
				}
			})
		}
	})
}

// Format selection (templates.md §1): meeting-summary selects the format;
// F-04's type no-such-type, or a type whose format is missing, is held
// unknown_type before any provider call, and nothing is rendered.
func TestFormatSelection(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-06", func(t *testing.T) {
		t.Run("meeting-summary", func(t *testing.T) {
			res := renderOK(t, bin, run{input: loadCase(t, "F-01").input()})
			if res.FormatID != "meeting-summary" || res.FormatVersion != "1.0.0" || !hex64.MatchString(res.FormatSHA256) {
				t.Errorf("format = {%q, %q, %q}, want {meeting-summary, 1.0.0, <64 hex>}", res.FormatID, res.FormatVersion, res.FormatSHA256)
			}
		})
		manifest := loadObject(t, testdataDir, "meetings", "F-04", "expected", "manifest.json")
		f04Type, _ := manifest["metadata"].(object)["recap-type"].(string)
		service := loadObject(t, testdataDir, "meetings", "F-04", "expected", "service.json")
		job := service["job"].(object)
		if f04Type == "" || job["state"] != "held" || job["reason"] != "unknown_type" {
			t.Fatalf("fixture: F-04 recap-type %q, job %v; want a type and held/unknown_type", f04Type, job)
		}
		missingFormat := filepath.Join(t.TempDir(), "meeting-types.yaml")
		writeFile(t, missingFormat, []byte(strings.ReplaceAll(meetingTypesYAML, "format: meeting-summary", "format: no-such-format")))
		for _, tc := range []struct {
			name, meetingType string
			env               map[string]string
		}{
			{"F-04-" + f04Type, f04Type, nil},
			{"format-missing", "meeting-summary", map[string]string{"RECAP_MEETING_TYPES_FILE": missingFormat}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := f01Summarize(t)
				c.context["meeting_type"] = tc.meetingType
				res, calls := summarize(t, bin, c.input(), tc.env)
				if res.Outcome != "hold" || res.Reason != "unknown_type" {
					t.Errorf("outcome = %q, reason %q; want hold, unknown_type", res.Outcome, res.Reason)
				}
				if len(calls) != 0 {
					t.Errorf("calls = %s, want none before the hold", kinds(calls))
				}
				if entries, err := os.ReadDir(res.outDir); err != nil || len(entries) != 0 {
					t.Errorf("output directory after a hold: %v entries (err %v), want empty", len(entries), err)
				}
			})
		}
	})
}
