package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

const (
	schema     = `{"type":"object","additionalProperties":false,"required":["overview"],"properties":{"overview":{"type":"string","minLength":1},"items":{"type":"array","items":{"type":"string"}}}}`
	valid      = `{"overview":"ok"}`
	canary     = "CANARY-CONTENT-7f3a"
	systemText = "System " + canary
	userText   = "User " + canary
)

// canned is one canned answer of the fake.
type canned struct {
	status  int
	content string
	finish  string
	body    string // raw body instead of a completion
}

// fake is a chat completions server that answers with canned replies in
// order (the last one repeats) and records the request bodies.
type fake struct {
	mu      sync.Mutex
	replies []canned
	bodies  [][]byte
	auth    []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	rep := f.replies[min(len(f.bodies), len(f.replies))-1]
	f.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if rep.status == 0 {
		rep.status = 200
	}
	w.WriteHeader(rep.status)
	if rep.body != "" || rep.status != 200 {
		_, _ = io.WriteString(w, rep.body)
		return
	}
	if rep.finish == "" {
		rep.finish = "stop"
	}
	out, _ := json.Marshal(map[string]any{
		"model": "reported-model",
		"choices": []any{map[string]any{"index": 0, "finish_reason": rep.finish,
			"message": map[string]any{"role": "assistant", "content": rep.content}}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	})
	_, _ = w.Write(out)
}

func (f *fake) requests(t *testing.T) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, b := range f.bodies {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func setup(t *testing.T, mode string, replies ...canned) (*Adapter, *fake, *bytes.Buffer) {
	t.Helper()
	f := &fake{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := &providers.Profile{Name: "vllm-test", Kind: providers.KindLLM, Dialect: Dialect, BaseURL: srv.URL + "/v1",
		Model: "example-model", ContextTokens: 1000, MaxOutputTokens: 100, StructuredOutput: mode,
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}, "temperature": 0.7}}
	var logs bytes.Buffer
	return New(p, logging.New(&logs, slog.LevelDebug)), f, &logs
}

func request(user string, jsonOut bool) providers.LlmRequest {
	req := providers.LlmRequest{
		Messages: []providers.Message{{Role: "system", Content: systemText}, {Role: "user", Content: user}},
		Output:   providers.Output{Type: providers.OutputText},
		CallRef:  "job:summarize:summarize:0",
	}
	if jsonOut {
		req.Output = providers.Output{Type: providers.OutputJSON, Schema: json.RawMessage(schema)}
	}
	return req
}

func class(err error) providers.ErrorClass {
	var e *providers.Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

func TestRequestBodyPerMode(t *testing.T) {
	for _, mode := range []string{ModeJSONSchema, ModeJSONObject, ModePrompt} {
		t.Run(mode, func(t *testing.T) {
			a, f, _ := setup(t, mode, canned{content: valid})
			resp, err := a.Complete(context.Background(), request(userText, true))
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.JSON) != valid || resp.Model != "reported-model" || resp.Usage.TotalTokens != 12 {
				t.Errorf("response %+v", resp)
			}
			b := f.requests(t)[0]
			// The fixed fields win over extra_body.
			if b["model"] != "example-model" || b["max_tokens"] != float64(100) || b["temperature"] != float64(0) ||
				b["stream"] != false || b["chat_template_kwargs"] == nil {
				t.Errorf("body %v", b)
			}
			sys := b["messages"].([]any)[0].(map[string]any)["content"].(string)
			rf, has := b["response_format"].(map[string]any)
			switch mode {
			case ModeJSONSchema:
				if rf["type"] != "json_schema" || strings.Contains(sys, `"additionalProperties"`) {
					t.Errorf("response_format %v, system %q", rf, sys)
				}
			case ModeJSONObject:
				if rf["type"] != "json_object" || !strings.Contains(sys, `"additionalProperties":false`) {
					t.Errorf("response_format %v, system %q", rf, sys)
				}
			case ModePrompt:
				if has || !strings.Contains(sys, `"additionalProperties":false`) {
					t.Errorf("response_format %v, system %q", rf, sys)
				}
			}
		})
	}
}

func TestMaxTokensCappedAtProfile(t *testing.T) {
	a, f, _ := setup(t, ModeJSONObject, canned{content: "text"})
	req := request(userText, false)
	req.MaxOutputTokens = 5000
	if _, err := a.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := f.requests(t)[0]
	if b["max_tokens"] != float64(100) {
		t.Errorf("max_tokens %v, want the profile's 100", b["max_tokens"])
	}
	if _, ok := b["response_format"]; ok {
		t.Errorf("response_format for text output")
	}
}

func TestRepair(t *testing.T) {
	cases := map[string]struct {
		replies []canned
		calls   int
		class   providers.ErrorClass
		path    string
	}{
		"fenced":            {[]canned{{content: "```json\n" + valid + "\n```"}}, 1, "", ""},
		"bare fence":        {[]canned{{content: "```\n" + valid + "\n```"}}, 1, "", ""},
		"not JSON":          {[]canned{{content: `{"overview": "` + canary}, {content: valid}}, 2, "", ""},
		"trailing data":     {[]canned{{content: valid + ` {"x":1}`}, {content: valid}}, 2, "", ""},
		"violation":         {[]canned{{content: `{"overview":"` + canary + `","items":"x"}`}, {content: valid}}, 2, "", "/items"},
		"nested violation":  {[]canned{{content: `{"overview":"x","items":[1]}`}, {content: valid}}, 2, "", "/items/0"},
		"echo $schema":      {[]canned{{content: `{"$schema":"x","overview":"x"}`}, {content: valid}}, 2, "", ""},
		"echo type+props":   {[]canned{{content: `{"type":"object","properties":{},"overview":"x"}`}, {content: valid}}, 2, "", ""},
		"always invalid":    {[]canned{{content: `{"overview":"` + canary + `","items":"x"}`}}, 3, providers.InvalidResponse, "/items"},
		"truncated":         {[]canned{{content: valid, finish: "length"}}, 1, providers.OutputTruncated, ""},
		"content filter":    {[]canned{{content: valid, finish: "content_filter"}}, 1, providers.InvalidResponse, ""},
		"no choices":        {[]canned{{body: `{"model":"m","choices":[]}`}}, 1, providers.InvalidResponse, ""},
		"body not JSON":     {[]canned{{body: `<html>`}}, 1, providers.InvalidResponse, ""},
		"null content":      {[]canned{{body: `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`}}, 1, providers.InvalidResponse, ""},
		"truncated no echo": {[]canned{{content: `{"$schema":"x"}`, finish: "length"}}, 1, providers.OutputTruncated, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a, f, logs := setup(t, ModeJSONObject, c.replies...)
			resp, err := a.Complete(context.Background(), request(userText, true))
			reqs := f.requests(t)
			if len(reqs) != c.calls {
				t.Fatalf("%d calls, want %d", len(reqs), c.calls)
			}
			if class(err) != c.class {
				t.Fatalf("error %v, want class %q", err, c.class)
			}
			if c.class == "" && string(resp.JSON) != valid {
				t.Errorf("json %s", resp.JSON)
			}
			for _, r := range reqs[1:] {
				msgs := r["messages"].([]any)
				if len(msgs) != 4 {
					t.Fatalf("repair call has %d messages, want 4", len(msgs))
				}
				last := msgs[3].(map[string]any)
				if msgs[2].(map[string]any)["role"] != "assistant" || last["role"] != "user" {
					t.Errorf("repair messages %v", msgs[2:])
				}
				if s := last["content"].(string); strings.Contains(s, canary) || !strings.Contains(s, c.path) {
					t.Errorf("repair message %q (want path %q, no content)", s, c.path)
				}
			}
			if strings.Contains(logs.String(), canary) {
				t.Errorf("log contains content: %s", logs.String())
			}
		})
	}
}

func TestStatusClasses(t *testing.T) {
	for status, want := range map[int]providers.ErrorClass{
		400: providers.PermanentConfig, 401: providers.PermanentConfig, 403: providers.PermanentConfig,
		404: providers.PermanentConfig, 409: providers.PermanentConfig, 302: providers.PermanentConfig,
		408: providers.Retryable, 429: providers.Retryable, 500: providers.Retryable, 501: providers.Retryable,
		502: providers.Retryable, 503: providers.Retryable, 504: providers.Retryable,
	} {
		a, f, logs := setup(t, ModeJSONObject, canned{status: status, body: canary})
		_, err := a.Complete(context.Background(), request(userText, true))
		if class(err) != want {
			t.Errorf("%d: error %v, want %s", status, err, want)
		}
		if n := len(f.requests(t)); n != 1 {
			t.Errorf("%d: %d calls, want 1", status, n)
		}
		if strings.Contains(logs.String(), canary) || strings.Contains(err.Error(), canary) {
			t.Errorf("%d: error body leaked", status)
		}
	}
}

func TestInputTooLarge(t *testing.T) {
	// 2 500 chars ≈ 834 tokens > 80 % of 1 000 context tokens.
	a, _, _ := setup(t, ModeJSONObject, canned{status: 400})
	_, err := a.Complete(context.Background(), request(strings.Repeat("x", 2500), true))
	if class(err) != providers.InputTooLarge {
		t.Errorf("error %v, want input_too_large", err)
	}
}

func TestRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	a, _, _ := setup(t, ModeJSONObject)
	a.profile.BaseURL = srv.URL + "/v1"
	_, err := a.Complete(context.Background(), request(userText, true))
	var e *providers.Error
	if !errors.As(err, &e) || e.Class != providers.Retryable || e.RetryAfter != 7*time.Second {
		t.Errorf("error %+v, want retryable with Retry-After 7 s", err)
	}
}

func TestConnectRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	a, _, _ := setup(t, ModeJSONObject)
	a.profile.BaseURL = srv.URL + "/v1"
	if _, err := a.Complete(context.Background(), request(userText, true)); class(err) != providers.Retryable {
		t.Errorf("error %v, want retryable", err)
	}
}

func TestBadSchemaIsConfig(t *testing.T) {
	a, f, _ := setup(t, ModeJSONObject, canned{content: valid})
	req := request(userText, true)
	req.Output.Schema = json.RawMessage(`{"type": 5}`)
	if _, err := a.Complete(context.Background(), req); class(err) != providers.PermanentConfig {
		t.Errorf("error %v, want permanent_config", err)
	}
	if len(f.requests(t)) != 0 {
		t.Errorf("request sent with an invalid schema")
	}
}

// A key the model invented reaches the repair message as part of the
// pointer, but never the log: there the pointer stops at the defined parent.
func TestInventedKeyNotLogged(t *testing.T) {
	const invented = "INVENTEDKEY9c2e"
	a, f, logs := setup(t, ModeJSONObject,
		canned{content: `{"overview":"x","items":["a"],"` + invented + `":{"deep":1}}`}, canned{content: valid})
	req := request(userText, true)
	req.Output.Schema = json.RawMessage(`{"type":"object","required":["overview"],"additionalProperties":{"type":"string"},` +
		`"properties":{"overview":{"type":"string"},"items":{"type":"array","items":{"type":"string"}}}}`)
	if _, err := a.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests(t)
	repair := reqs[1]["messages"].([]any)[3].(map[string]any)["content"].(string)
	if !strings.Contains(repair, "/"+invented) {
		t.Errorf("repair message %q lacks the full pointer", repair)
	}
	if strings.Contains(logs.String(), invented) || !strings.Contains(logs.String(), `\"\" breaks the schema keyword`) {
		t.Errorf("log: %s", logs.String())
	}
}

func TestDefinedPointer(t *testing.T) {
	sch, err := providers.CompileSchema([]byte(`{"type":"object","properties":{"items":{"type":"array",` +
		`"items":{"type":"object","properties":{"at":{"type":"string"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"/items/0/at": "/items/0/at", "/items/3/made-up": "/items/3", "/made-up/0": "", "/items/x": "/items",
	} {
		if got := pointer(defined(sch, strings.Split(in, "/")[1:])); got != want {
			t.Errorf("defined(%s) = %q, want %q", in, got, want)
		}
	}
}
