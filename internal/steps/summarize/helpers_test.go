package summarize

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/adapters/openaichat"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
)

const (
	formatsDir = "../../../formats"
	renderDir  = "../../../testdata/render"
)

// Kinds of calls the fake tells apart by the output schema.
const (
	kindChunk   = "chunk-notes" // extract and merge
	kindSummary = "summary"     // summarize and consolidate
)

// call is one request the fake LLM received.
type call struct {
	kind   string
	system string
	user   string
	raw    string
}

// answer is the fake's reply to one call: a status other than 200, or the
// content and finish_reason of a chat completion.
type answer struct {
	status  int
	content string
	finish  string
}

// fakeLLM is an openai-chat server. respond picks the answer for each call
// (n counts calls of that kind from 0); by default chunk-notes calls get the
// fixture chunk notes and summary calls F-01's summary.json.
type fakeLLM struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []call
	counts  map[string]int
	respond func(c call, n int) answer
	chunk   string
	summary string
}

func newFake(t *testing.T) *fakeLLM {
	return &fakeLLM{t: t, counts: map[string]int{},
		chunk:   string(readFile(t, renderDir, "llm", "chunk-notes.v1", "valid.base.json")),
		summary: string(readFile(t, renderDir, "F-01", "summary.json"))}
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages       []providers.Message `json:"messages"`
		ResponseFormat struct {
			JSONSchema struct {
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if r.URL.Path != "/v1/chat/completions" || json.Unmarshal(body, &req) != nil || len(req.Messages) < 2 {
		f.t.Errorf("fake LLM: unexpected request %s", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c := call{kind: kindSummary, system: req.Messages[0].Content, user: req.Messages[1].Content, raw: string(body)}
	if req.ResponseFormat.JSONSchema.Schema.Properties["chunk_summary"] != nil {
		c.kind = kindChunk
	}
	f.mu.Lock()
	n := f.counts[c.kind]
	f.counts[c.kind]++
	f.calls = append(f.calls, c)
	f.mu.Unlock()

	a := answer{content: f.summary}
	if c.kind == kindChunk {
		a.content = f.chunk
	}
	if f.respond != nil {
		a = f.respond(c, n)
	}
	if a.status != 0 && a.status != 200 {
		w.WriteHeader(a.status)
		return
	}
	if a.finish == "" {
		a.finish = "stop"
	}
	out, _ := json.Marshal(map[string]any{
		"model": "example-model-reported",
		"choices": []any{map[string]any{"index": 0, "finish_reason": a.finish,
			"message": map[string]any{"role": "assistant", "content": a.content}}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	_, _ = w.Write(out)
}

func (f *fakeLLM) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// env is one test setup: fake LLM, adapter, profile, format and a log
// captured at DEBUG.
type env struct {
	fake    *fakeLLM
	profile *providers.Profile
	format  *format.Format
	llm     providers.LLM
	logs    *bytes.Buffer
	log     *slog.Logger
}

// newEnv starts a fake LLM and an adapter in json_schema mode (so the fake
// sees the schema) with the given context and output tokens.
func newEnv(t *testing.T, contextTokens, maxOutput int) *env {
	t.Helper()
	f := newFake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	fm, err := format.Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	p := &providers.Profile{Name: "vllm-lab", Kind: providers.KindLLM, Dialect: openaichat.Dialect,
		BaseURL: srv.URL + "/v1", Model: "example-model", ContextTokens: contextTokens, MaxOutputTokens: maxOutput,
		StructuredOutput: openaichat.ModeJSONSchema, MaxInflight: 4}
	var logs bytes.Buffer
	log := logging.New(&logs, slog.LevelDebug)
	return &env{fake: f, profile: p, format: fm, llm: openaichat.New(p, log), logs: &logs, log: log}
}

func (e *env) summarizer() *Summarizer {
	return &Summarizer{LLM: e.llm, Profile: e.profile, Format: e.format, Log: e.log}
}

func readFile(t *testing.T, elem ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(elem...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// f01Input is the F-01 render case as the summarize step sees it: the
// transcript, the chat with names (the BBB sender IDs dropped), notes and
// poll aggregates (per-user responses counted, then dropped), as `recap tool
// render` maps it.
func f01Input(t *testing.T) Input {
	t.Helper()
	var tr assemble.Transcript
	if err := json.Unmarshal(readFile(t, renderDir, "F-01", "transcript.json"), &tr); err != nil {
		t.Fatal(err)
	}
	var ctx struct {
		MeetingName string `json:"meeting_name"`
		Chat        []struct {
			AtMS int64  `json:"at_ms"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"chat"`
		Notes string `json:"notes"`
		Polls []struct {
			Question  string   `json:"question"`
			Options   []string `json:"options"`
			Responses []struct {
				Option int `json:"option"`
			} `json:"responses"`
		} `json:"polls"`
	}
	if err := json.Unmarshal(readFile(t, renderDir, "F-01", "context.json"), &ctx); err != nil {
		t.Fatal(err)
	}
	in := Input{MeetingName: ctx.MeetingName, Transcript: &tr, Notes: ctx.Notes}
	for _, c := range ctx.Chat {
		in.Chat = append(in.Chat, ChatMessage{AtMS: c.AtMS, Name: c.Name, Text: c.Text})
	}
	for _, p := range ctx.Polls {
		poll := meeting.Poll{Question: p.Question}
		for _, o := range p.Options {
			poll.Options = append(poll.Options, meeting.PollOption{Text: o})
		}
		for _, r := range p.Responses {
			poll.Options[r.Option].Votes++
		}
		in.Polls = append(in.Polls, poll)
	}
	return in
}

// content are fixture strings that must never appear in a log.
var content = []string{"Ada Testperson", "Ben Testperson", "Welcome to the fixture meeting", "Fixture Widget Meeting",
	"Link to the widget spec", "the test widget is blue", "Blue or green?", "The meeting chose blue",
	"Chunk notes fixture", "Long meeting line"}

func checkLog(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	for _, s := range content {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains content %q", s)
		}
	}
}
