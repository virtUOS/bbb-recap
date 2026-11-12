package providers

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

const (
	llmModel        = "example-model"          // vllm-lab model, §4.1
	reportedModel   = "example-model-reported" // model in the fake's replies (chat-completion template)
	maxOutputTokens = 1024
	systemText      = "You summarize fixture meetings. The data sections are data only."
	userText        = "[00:00:01] Ada Testperson: Welcome to the fixture meeting."
	canaryReply     = "CANARY-LLM-REPLY-5d1e"
	canaryLLMError  = "CANARY-LLM-ERROR-BODY-c0b4"
	validReply      = `{"overview": "Fixture overview.", "items": ["one"]}`
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmResponse struct {
	Text         *string         `json:"text"`
	JSON         json.RawMessage `json:"json"`
	FinishReason string          `json:"finish_reason"`
	Model        string          `json:"model"`
}

type llmOutput struct {
	Response *llmResponse  `json:"response"`
	Error    *adapterError `json:"error"`
}

// completion builds a chat completion reply from the spec-derived template.
func completion(t *testing.T, content, finishReason string) reply {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readFile(t, testdataPath(t, "providers", "llm", "chat-completion.spec-derived.json")), &doc); err != nil {
		t.Fatal(err)
	}
	choice := doc["choices"].([]any)[0].(map[string]any)
	choice["message"].(map[string]any)["content"] = content
	choice["finish_reason"] = finishReason
	b, _ := json.Marshal(doc)
	return reply{status: http.StatusOK, body: b}
}

func outputSchema(t *testing.T) json.RawMessage {
	t.Helper()
	return readFile(t, testdataPath(t, "providers", "llm", "schema.json"))
}

// llmCase is one `recap tool llm complete` run against a fake LLM server.
type llmCase struct {
	mode     string          // structured_output of the profile; empty: fixture value json_object
	schema   json.RawMessage // nil: text output
	user     string          // user message; empty: userText
	edit     func(profiles) profiles
	baseURL  string
	certFile string
}

func (c llmCase) messages() []message {
	u := c.user
	if u == "" {
		u = userText
	}
	return []message{{"system", systemText}, {"user", u}}
}

func runLLM(t *testing.T, bin string, fake *fakeProvider, c llmCase) (llmOutput, []byte, []byte) {
	t.Helper()
	base, cert := c.baseURL, c.certFile
	if base == "" {
		base = fake.url()
	}
	if cert == "" {
		cert = fake.certFile(t)
	}
	p := loadProfiles(t).set(t, "base_url", llmProfile, base+"/v1")
	if c.mode != "" {
		p = p.set(t, "structured_output", 1, c.mode)
	}
	if c.edit != nil {
		p = c.edit(p)
	}
	output := map[string]any{"type": "text"}
	if c.schema != nil {
		output = map[string]any{"type": "json", "schema": c.schema}
	}
	in := map[string]any{
		"profile": "vllm-lab",
		"request": map[string]any{
			"messages":          c.messages(),
			"output":            output,
			"max_output_tokens": maxOutputTokens,
			"call_ref":          jobID + ":summarize:summarize:0",
		},
	}
	var out llmOutput
	stdout, stderr := runJSON(t, bin, toolEnv{providersFile: p.write(t), certFile: cert}, in, &out, "llm", "complete")
	if (out.Response == nil) == (out.Error == nil) {
		t.Fatalf("want exactly one of response and error: %s", stdout)
	}
	return out, stdout, stderr
}

// requestBody decodes a recorded chat completion request.
func requestBody(t *testing.T, ex exchange) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(ex.body, &b); err != nil {
		t.Fatalf("request body is not a JSON object: %v", err)
	}
	return b
}

func sentMessages(t *testing.T, ex exchange) []message {
	t.Helper()
	var b struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(ex.body, &b); err != nil {
		t.Fatalf("request messages: %v", err)
	}
	return b.Messages
}

func expectCalls(t *testing.T, f *fakeProvider, n int) []exchange {
	t.Helper()
	got := f.requests()
	if len(got) != n {
		t.Fatalf("provider received %d calls, want %d", len(got), n)
	}
	return got
}

func expectClass(t *testing.T, out llmOutput, class string) {
	t.Helper()
	if out.Error == nil || out.Error.Class != class {
		t.Fatalf("error %+v (response %+v), want class %s", out.Error, out.Response, class)
	}
}

func expectJSON(t *testing.T, out llmOutput, want string) {
	t.Helper()
	if out.Response == nil {
		t.Fatalf("error %+v, want a response", *out.Error)
	}
	if !jsonEqual(t, out.Response.JSON, []byte(want)) {
		t.Fatalf("json = %s, want %s", out.Response.JSON, want)
	}
}

func TestLLM(t *testing.T) {
	t.Run("CT-PROV-07 LLM request body", func(t *testing.T) { testLLMRequest(t, harness.Bin(t)) })
	t.Run("CT-PROV-08 structured output and repair", func(t *testing.T) { testStructured(t, harness.Bin(t)) })
	t.Run("CT-PROV-09 output truncated", func(t *testing.T) { testTruncated(t, harness.Bin(t)) })
	t.Run("CT-PROV-10 LLM error classes", func(t *testing.T) { testLLMErrors(t, harness.Bin(t)) })
	t.Run("CT-PROV-11 chunker", func(t *testing.T) { testChunker(t, harness.Bin(t)) })
}

// §3.2, §3.3: path, Authorization, model, messages, max_tokens, temperature 0,
// stream false, response_format per mode, extra_body merged.
func testLLMRequest(t *testing.T, bin string) {
	key := fixtureKey(t, "llm-fixture.key")
	schema := outputSchema(t)
	// "absent": no structured_output in the profile; the default is json_object (§3.3).
	for _, name := range []string{"json_schema", "json_object", "prompt", "absent"} {
		t.Run(name, func(t *testing.T) {
			mode, c := name, llmCase{mode: name, schema: schema}
			if name == "absent" {
				mode = "json_object"
				c = llmCase{schema: schema, edit: func(p profiles) profiles { return p.without(t, "structured_output", 1) }}
			}
			f := startFake(t, completion(t, validReply, "stop"))
			out, stdout, stderr := runLLM(t, bin, f, c)
			ex := expectCalls(t, f, 1)[0]
			if ex.method != http.MethodPost || ex.path != "/v1/chat/completions" {
				t.Errorf("request = %s %s, want POST /v1/chat/completions", ex.method, ex.path)
			}
			if got := ex.header.Get("Authorization"); got != "Bearer "+key {
				t.Errorf("Authorization = %q, want Bearer <llm-fixture.key>", got)
			}
			b := requestBody(t, ex)
			if b["model"] != llmModel {
				t.Errorf("model = %v, want %s", b["model"], llmModel)
			}
			if b["max_tokens"] != float64(maxOutputTokens) {
				t.Errorf("max_tokens = %v, want %d", b["max_tokens"], maxOutputTokens)
			}
			if v, ok := b["temperature"]; !ok || v != float64(0) {
				t.Errorf("temperature = %v, want 0", v)
			}
			if v, ok := b["stream"]; !ok || v != false {
				t.Errorf("stream = %v, want false", v)
			}
			want := map[string]any{"enable_thinking": false}
			if got, _ := b["chat_template_kwargs"].(map[string]any); !jsonEqualValues(got, want) {
				t.Errorf("chat_template_kwargs = %v, want %v (extra_body merged)", b["chat_template_kwargs"], want)
			}
			msgs := sentMessages(t, ex)
			if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
				t.Fatalf("messages = %+v, want system then user", msgs)
			}
			if !strings.Contains(msgs[0].Content, systemText) {
				t.Errorf("system message lost the request's system text")
			}
			if msgs[1].Content != userText {
				t.Errorf("user message = %q, want %q", msgs[1].Content, userText)
			}
			rf, has := b["response_format"]
			switch mode {
			case "json_schema":
				m, _ := rf.(map[string]any)
				js, _ := m["json_schema"].(map[string]any)
				name, _ := js["name"].(string)
				gotSchema, _ := json.Marshal(js["schema"])
				if m["type"] != "json_schema" || name == "" || js["strict"] != true || !jsonEqual(t, gotSchema, schema) {
					t.Errorf("response_format = %v, want {type: json_schema, json_schema: {name, schema, strict: true}}", rf)
				}
			case "json_object":
				m, _ := rf.(map[string]any)
				if len(m) != 1 || m["type"] != "json_object" {
					t.Errorf("response_format = %v, want {type: json_object}", rf)
				}
			case "prompt":
				if has {
					t.Errorf("response_format = %v, want none in prompt mode", rf)
				}
			}
			// The adapter puts the schema into the system message in json_object
			// and prompt modes (§3.3).
			if mode != "json_schema" && !containsJSON(msgs[0].Content, schema) {
				t.Errorf("system message does not contain the output schema in %s mode", mode)
			}
			expectJSON(t, out, validReply)
			// LlmResponse.model is the model the provider reports (§3.1; §4.1
			// "reported model as provenance"), not the profile's.
			if out.Response.Model != reportedModel || out.Response.FinishReason != "stop" {
				t.Errorf("response model %q finish_reason %q, want %q stop", out.Response.Model, out.Response.FinishReason, reportedModel)
			}
			expectNotLeaked(t, stdout, stderr, key)
		})
	}
	t.Run("no-extra_body", func(t *testing.T) {
		f := startFake(t, completion(t, validReply, "stop"))
		runLLM(t, bin, f, llmCase{schema: schema, edit: func(p profiles) profiles { return p.without(t, "extra_body", 1) }})
		if _, ok := requestBody(t, expectCalls(t, f, 1)[0])["chat_template_kwargs"]; ok {
			t.Errorf("chat_template_kwargs sent without extra_body")
		}
	})
	t.Run("text-output", func(t *testing.T) {
		f := startFake(t, completion(t, "Plain text reply.", "stop"))
		out, _, _ := runLLM(t, bin, f, llmCase{mode: "json_schema"})
		if _, ok := requestBody(t, expectCalls(t, f, 1)[0])["response_format"]; ok {
			t.Errorf("response_format sent for text output")
		}
		if out.Response == nil || out.Response.Text == nil || *out.Response.Text != "Plain text reply." {
			t.Errorf("response = %+v, want text %q", out.Response, "Plain text reply.")
		}
	})
}

// containsJSON reports whether text contains a JSON value equal to want, in
// any formatting.
func containsJSON(text string, want json.RawMessage) bool {
	for i := range text {
		if text[i] != '{' {
			continue
		}
		var v json.RawMessage
		if json.NewDecoder(strings.NewReader(text[i:])).Decode(&v) == nil && jsonEqualRaw(v, want) {
			return true
		}
	}
	return false
}

func jsonEqualRaw(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func jsonEqualValues(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return a != nil && string(x) == string(y)
}

// §3.3, §3.4: valid, fenced, repair after invalid JSON / schema violation /
// schema echo, invalid_response after 2 repairs.
func testStructured(t *testing.T, bin string) {
	schema := outputSchema(t)
	notJSON := `{"overview": "` + canaryReply + `", "items": [`
	violation := `{"overview": "` + canaryReply + `", "items": "not-an-array"}`

	t.Run("valid", func(t *testing.T) {
		f := startFake(t, completion(t, validReply, "stop"))
		out, _, _ := runLLM(t, bin, f, llmCase{schema: schema})
		expectCalls(t, f, 1)
		expectJSON(t, out, validReply)
	})
	t.Run("fenced", func(t *testing.T) {
		f := startFake(t, completion(t, "```json\n"+validReply+"\n```", "stop"))
		out, _, _ := runLLM(t, bin, f, llmCase{schema: schema})
		expectCalls(t, f, 1)
		expectJSON(t, out, validReply)
	})

	t.Run("log-scan", func(t *testing.T) {
		// No prompt or reply text at INFO or above, over a run with one repair.
		f := startFake(t, completion(t, violation, "stop"), completion(t, validReply, "stop"))
		_, _, stderr := runLLM(t, bin, f, llmCase{schema: schema})
		expectCalls(t, f, 2)
		expectNoContentAtInfo(t, stderr, systemText, userText, canaryReply, "Fixture overview.")
	})

	// repairOnce: first reply bad, second valid → exactly one repair call.
	repairOnce := func(t *testing.T, bad string, c llmCase, wantPath string) {
		t.Helper()
		f := startFake(t, completion(t, bad, "stop"), completion(t, validReply, "stop"))
		out, stdout, stderr := runLLM(t, bin, f, c)
		calls := expectCalls(t, f, 2)
		expectJSON(t, out, validReply)
		checkRepair(t, c.messages(), calls[1], bad, wantPath)
		expectNotLeaked(t, stdout, stderr, canaryReply)
	}
	t.Run("invalid-JSON-then-valid", func(t *testing.T) { repairOnce(t, notJSON, llmCase{schema: schema}, "") })
	t.Run("schema-violation-then-valid", func(t *testing.T) { repairOnce(t, violation, llmCase{schema: schema}, "/items") })

	// Schema echo is rejected even when the echo satisfies the schema, so these
	// cases use the permissive schema {"type": "object"}.
	permissive := json.RawMessage(`{"type": "object"}`)
	// Each echo rule on its own: only $schema, then only type + properties.
	t.Run("schema-echo-$schema", func(t *testing.T) {
		repairOnce(t, `{"$schema": "https://json-schema.org/draft/2020-12/schema", "overview": "x"}`, llmCase{schema: permissive}, "")
	})
	t.Run("schema-echo-type-properties", func(t *testing.T) {
		repairOnce(t, `{"type": "object", "properties": {"overview": {"type": "string"}}}`, llmCase{schema: permissive}, "")
	})

	t.Run("always-invalid", func(t *testing.T) {
		f := startFake(t, completion(t, violation, "stop"))
		out, stdout, stderr := runLLM(t, bin, f, llmCase{schema: schema})
		calls := expectCalls(t, f, 3) // first call + 2 repairs
		expectClass(t, out, "invalid_response")
		for _, c := range calls[1:] {
			checkRepair(t, llmCase{}.messages(), c, violation, "/items")
		}
		expectNotLeaked(t, stdout, stderr, canaryReply)
	})
}

// checkRepair checks a repair call: the original messages unchanged, then the
// previous reply as assistant, then a user message that names the error path
// but carries no content of the reply (§3.4).
func checkRepair(t *testing.T, original []message, ex exchange, prev, wantPath string) {
	t.Helper()
	msgs := sentMessages(t, ex)
	if len(msgs) < len(original)+2 {
		t.Fatalf("repair call has %d messages, want at least %d", len(msgs), len(original)+2)
	}
	if msgs[1] != original[1] || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, systemText) {
		t.Errorf("repair call changed the original messages: %+v", msgs[:2])
	}
	a, u := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if a.Role != "assistant" || a.Content != prev {
		t.Errorf("second-to-last message = %s %q, want assistant with the previous reply", a.Role, a.Content)
	}
	if u.Role != "user" {
		t.Errorf("last message role = %s, want user", u.Role)
	}
	if strings.Contains(u.Content, canaryReply) {
		t.Errorf("repair message contains reply content")
	}
	if wantPath != "" && !strings.Contains(u.Content, wantPath) {
		t.Errorf("repair message %q does not name the error path %s", u.Content, wantPath)
	}
}

// §3.4: finish_reason length → output_truncated, never parsed or repaired.
func testTruncated(t *testing.T, bin string) {
	schema := outputSchema(t)
	for name, content := range map[string]string{
		"cut-JSON":   `{"overview": "Fixture over`,
		"valid-JSON": validReply,
	} {
		t.Run(name, func(t *testing.T) {
			f := startFake(t, completion(t, content, "length"), completion(t, validReply, "stop"))
			out, _, _ := runLLM(t, bin, f, llmCase{schema: schema})
			expectCalls(t, f, 1)
			expectClass(t, out, "output_truncated")
		})
	}
}

// §3.4: status codes and transport errors → class; one call only; error
// bodies never in output or logs.
func testLLMErrors(t *testing.T, bin string) {
	key := fixtureKey(t, "llm-fixture.key")
	schema := outputSchema(t)
	body := []byte(`{"error": {"message": "` + canaryLLMError + `"}}`)
	classes := map[int]string{
		408: "retryable", 429: "retryable", 500: "retryable", 502: "retryable", 503: "retryable", 504: "retryable",
		401: "permanent_config", 403: "permanent_config", 404: "permanent_config", 400: "permanent_config",
	}
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := startFake(t, reply{status: status, body: body})
			out, stdout, stderr := runLLM(t, bin, f, llmCase{schema: schema})
			expectCalls(t, f, 1)
			expectClass(t, out, classes[status])
			expectNotLeaked(t, stdout, stderr, canaryLLMError, key)
		})
	}
	t.Run("400-large-input", func(t *testing.T) {
		// 90 000 chars ≈ 30 000 tokens (3.0 chars per token) > 80 % of context_tokens 32768.
		big := strings.Repeat("Fixture words fill the long transcript. ", 2250)
		f := startFake(t, reply{status: 400, body: body})
		out, stdout, stderr := runLLM(t, bin, f, llmCase{schema: schema, user: big})
		expectCalls(t, f, 1)
		expectClass(t, out, "input_too_large")
		expectNotLeaked(t, stdout, stderr, canaryLLMError)
	})
	t.Run("connect-refused", func(t *testing.T) {
		f := startFake(t, completion(t, validReply, "stop")) // only for its certificate
		out, _, _ := runLLM(t, bin, f, llmCase{schema: schema, baseURL: closedURL(t)})
		expectClass(t, out, "retryable")
	})
	t.Run("no-choices", func(t *testing.T) {
		f := startFake(t, reply{status: 200, body: []byte(`{"model": "example-model"}`)})
		out, _, _ := runLLM(t, bin, f, llmCase{schema: schema})
		expectClass(t, out, "invalid_response")
	})
	t.Run("finish_reason-content_filter", func(t *testing.T) {
		f := startFake(t, completion(t, validReply, "content_filter"))
		out, _, _ := runLLM(t, bin, f, llmCase{schema: schema})
		expectClass(t, out, "invalid_response")
	})
}

// --- chunker (§3.5) -------------------------------------------------------

type chunkOutput struct {
	BudgetTokens int `json:"budget_tokens"`
	Chunks       []struct {
		Lines []string `json:"lines"`
	} `json:"chunks"`
}

func (o chunkOutput) lines() [][]string {
	var out [][]string
	for _, c := range o.Chunks {
		out = append(out, c.Lines)
	}
	return out
}

func runChunk(t *testing.T, bin string, lines []string, budget int, resplit []int) ([][]string, []byte) {
	t.Helper()
	in := map[string]any{"lines": lines, "budget_tokens": budget}
	if resplit != nil {
		in["resplit"] = resplit
	}
	providersFile := loadProfiles(t).write(t)
	var out chunkOutput
	stdout, _ := runJSON(t, bin, toolEnv{providersFile: providersFile}, in, &out, "llm", "chunk")
	return out.lines(), stdout
}

// runChunkProfile lets the tool compute the budget from the profile and the
// prompt size (§3.5 step 1) and returns the budget it reports and the chunks.
func runChunkProfile(t *testing.T, bin string, p profiles, lines []string, promptChars int) (int, [][]string) {
	t.Helper()
	in := map[string]any{"lines": lines, "profile": "vllm-lab", "prompt_chars": promptChars}
	var out chunkOutput
	runJSON(t, bin, toolEnv{providersFile: p.write(t)}, in, &out, "llm", "chunk")
	return out.BudgetTokens, out.lines()
}

// tokens is the §3.5 estimate for a chunk: ceil(chars / 3.0) of its lines
// joined with newlines.
func tokens(lines []string) int {
	return int(math.Ceil(float64(len(strings.Join(lines, "\n"))) / 3.0))
}

func words(lines []string) []string { return strings.Fields(strings.Join(lines, " ")) }

// checkChunks checks budget, no empty chunk, and that the chunks hold the
// input in order without loss or overlap (cuts only at whitespace).
func checkChunks(t *testing.T, chunks [][]string, input []string, budget int) {
	t.Helper()
	var flat []string
	for i, c := range chunks {
		if len(c) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
		if n := tokens(c); n > budget {
			t.Errorf("chunk %d has %d tokens, budget %d", i, n, budget)
		}
		flat = append(flat, c...)
	}
	if !slices.Equal(words(flat), words(input)) {
		t.Fatalf("chunks do not hold the input in order (lost, repeated or cut inside a word)")
	}
	// Every input line that fits the budget stays whole and in order.
	j := 0
	for _, l := range input {
		if tokens([]string{l}) > budget {
			continue
		}
		for j < len(flat) && flat[j] != l {
			j++
		}
		if j == len(flat) {
			t.Fatalf("line %q is not a whole element of a chunk (cut only between segments)", l)
		}
		j++
	}
}

func testChunker(t *testing.T, bin string) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("[00:%02d:%02d] Speaker %d: Line %02d is about the fixture widget.", i/60, i%60, i%2+1, i))
	}
	// Sentences of 50 chars: two joined = 101 chars, three = 152.
	var sentences []string
	for i := 1; i <= 8; i++ {
		sentences = append(sentences, fmt.Sprintf("Long segment sentence number %02d goes on for a bit.", i))
	}
	for _, s := range sentences {
		if len(s) != 50 {
			t.Fatalf("test helper: sentence length %d, want 50", len(s))
		}
	}
	longSentences := strings.Join(sentences, " ")
	// Words of 8 chars without sentence ends: 16 words = 143 chars, 17 = 152.
	var ws []string
	for i := 1; i <= 40; i++ {
		ws = append(ws, fmt.Sprintf("widget%02d", i))
	}
	longWords := strings.Join(ws, " ")

	const budget = 50 // 150 chars

	t.Run("between-segments-within-budget", func(t *testing.T) {
		chunks, _ := runChunk(t, bin, lines, budget, nil)
		checkChunks(t, chunks, lines, budget)
		if len(chunks) < 2 {
			t.Errorf("got %d chunks, want several for %d lines over a %d-token budget", len(chunks), len(lines), budget)
		}
	})
	t.Run("budget-from-profile", func(t *testing.T) {
		// 10000 − 2000 (max_output_tokens) − ceil(3000 / 3.0) (prompt) − 10 % of 10000 = 6000.
		p := loadProfiles(t).set(t, "context_tokens", 1, "10000").set(t, "max_output_tokens", 1, "2000")
		var many []string
		for i := 0; i < 16; i++ { // about 9 400 tokens
			many = append(many, lines...)
		}
		budget, chunks := runChunkProfile(t, bin, p, many, 3000)
		if budget != 6000 {
			t.Fatalf("budget_tokens = %d, want 6000", budget)
		}
		checkChunks(t, chunks, many, budget)
		if tokens(many) <= budget || len(chunks) < 2 {
			t.Errorf("got %d chunks for %d tokens of input, want several over a %d-token budget", len(chunks), tokens(many), budget)
		}
	})
	t.Run("input-fits-one-chunk", func(t *testing.T) {
		// §3.5 step 1: a single call when the input fits the budget.
		chunks, _ := runChunk(t, bin, lines, 1000, nil)
		if len(chunks) != 1 || !slices.Equal(chunks[0], lines) {
			t.Fatalf("got %d chunks, want exactly one holding all %d lines unchanged", len(chunks), len(lines))
		}
	})
	t.Run("deterministic", func(t *testing.T) {
		_, a := runChunk(t, bin, lines, budget, nil)
		_, b := runChunk(t, bin, lines, budget, nil)
		if string(a) != string(b) {
			t.Errorf("two runs differ:\n%s\n%s", a, b)
		}
	})
	t.Run("long-segment-cut-at-sentence-end", func(t *testing.T) {
		input := append([]string{longSentences}, lines[:4]...)
		chunks, _ := runChunk(t, bin, input, budget, nil)
		checkChunks(t, chunks, input, budget)
		want := sentences[0] + " " + sentences[1] // last sentence end within 150 chars
		if got := strings.TrimSpace(chunks[0][0]); got != want {
			t.Errorf("first piece = %q, want %q (cut at the last sentence end)", got, want)
		}
	})
	t.Run("long-segment-cut-at-space", func(t *testing.T) {
		input := append([]string{longWords}, lines[:4]...)
		chunks, _ := runChunk(t, bin, input, budget, nil)
		checkChunks(t, chunks, input, budget)
		want := strings.Join(ws[:16], " ") // last space within 150 chars
		if got := strings.TrimSpace(chunks[0][0]); got != want {
			t.Errorf("first piece = %q, want %q (cut at the last space)", got, want)
		}
	})
	t.Run("one-resplit-between-segments", func(t *testing.T) {
		const big = 70 // 210 chars: three lines per chunk
		first, _ := runChunk(t, bin, lines, big, nil)
		if len(first) < 2 || len(first[1]) < 2 {
			t.Fatalf("want chunk 1 with several lines, got %v", first)
		}
		second, _ := runChunk(t, bin, lines, big, []int{1})
		checkChunks(t, second, lines, big)
		if len(second) != len(first)+1 {
			t.Fatalf("got %d chunks after re-split, want %d", len(second), len(first)+1)
		}
		checkHalves(t, first, second, 1)
		for i, h := range second[1:3] {
			for _, l := range h {
				if !slices.Contains(first[1], l) {
					t.Errorf("half %d line %q is not a whole line of the chunk", i, l)
				}
			}
		}
	})
	t.Run("one-resplit-long-segment", func(t *testing.T) {
		input := []string{longSentences}
		first, _ := runChunk(t, bin, input, budget, nil)
		second, _ := runChunk(t, bin, input, budget, []int{0})
		checkChunks(t, second, input, budget)
		if len(second) != len(first)+1 {
			t.Fatalf("got %d chunks after re-split, want %d", len(second), len(first)+1)
		}
		checkHalves(t, first, second, 0)
	})
}

// checkHalves checks that chunk i of first became two non-empty, smaller
// chunks i and i+1 of second, holding the same words, and nothing else changed.
func checkHalves(t *testing.T, first, second [][]string, i int) {
	t.Helper()
	for k := range first {
		switch {
		case k < i && !slices.Equal(first[k], second[k]), k > i && !slices.Equal(first[k], second[k+1]):
			t.Errorf("chunk %d changed by the re-split of chunk %d", k, i)
		}
	}
	a, b := second[i], second[i+1]
	if !slices.Equal(words(append(slices.Clone(a), b...)), words(first[i])) {
		t.Errorf("halves %v + %v do not hold chunk %v", a, b, first[i])
	}
	whole := len(strings.Join(first[i], "\n"))
	if len(strings.Join(a, "\n")) >= whole || len(strings.Join(b, "\n")) >= whole || len(a) == 0 || len(b) == 0 {
		t.Errorf("halves %q and %q are not both non-empty and smaller than the chunk", a, b)
	}
}
