// Package openaichat is the LLM dialect openai-chat of docs/reference/providers.md
// §3: OpenAI-compatible chat completions as served by vLLM. One
// POST {base_url}/chat/completions per call, plain net/http, no SDK
// (ADR-008). The HTTP client never retries; the job queue does. JSON repair
// calls (§3.4) are the only extra requests.
//
// Nothing here logs or returns prompt text, reply text or provider error
// bodies (privacy.md §6).
package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/virtUOS/bbb-recap/internal/llmplan"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

// Dialect is the dialect name in provider profiles.
const Dialect = "openai-chat"

const (
	completionsPath  = "/chat/completions"
	maxResponseBytes = 16 << 20 // assumption: far above any reply within max_tokens
	// inputTooLargeShare: a 400 on a call whose estimated input is above
	// this share of context_tokens is input_too_large (§3.4).
	inputTooLargeShare = 0.8
)

// Structured-output modes (§3.3).
const (
	ModeJSONSchema = "json_schema"
	ModeJSONObject = "json_object"
	ModePrompt     = "prompt"
)

// Outcomes is every value of the outcome label this adapter counts in
// recap_external_requests_total: ok or an error class.
var Outcomes = []string{metrics.OK, string(providers.Retryable), string(providers.PermanentConfig),
	string(providers.InputTooLarge), string(providers.OutputTruncated), string(providers.InvalidResponse)}

// Adapter talks to one openai-chat profile.
type Adapter struct {
	profile *providers.Profile
	client  *http.Client
	log     *slog.Logger
}

// New returns the adapter for an LLM profile with dialect openai-chat.
func New(p *providers.Profile, log *slog.Logger) *Adapter {
	metrics.InitExternal(p.Name, metrics.KindLLM, Outcomes...)
	return &Adapter{profile: p, client: providers.NewHTTPClient(), log: log}
}

// SchemaInstruction is the text the adapter appends to the system message
// in json_object and prompt modes, followed by the schema (§3.3).
const SchemaInstruction = "\n\nReply with one JSON value only, no other text. " +
	"It must validate against this JSON Schema:\n"

// Complete runs one call (§3.1), with up to providers.RepairAttempts JSON
// repair calls (§3.4).
func (a *Adapter) Complete(ctx context.Context, req providers.LlmRequest) (*providers.LlmResponse, error) {
	var schema *jsonschema.Schema
	jsonOut := req.Output.Type == providers.OutputJSON
	if jsonOut {
		var err error
		if schema, err = providers.CompileSchema(req.Output.Schema); err != nil {
			return nil, &providers.Error{Class: providers.PermanentConfig, Code: "bad_output_schema"}
		}
	}
	maxTokens := a.profile.MaxOutputTokens
	if req.MaxOutputTokens > 0 {
		maxTokens = min(req.MaxOutputTokens, maxTokens)
	}
	messages := a.messages(req)

	original := len(messages)
	for repair := 0; ; repair++ {
		resp, err := a.call(ctx, req, messages, maxTokens, repair)
		if err != nil {
			return nil, err
		}
		if !jsonOut {
			metrics.ExternalRequest(a.profile.Name, metrics.KindLLM, metrics.OK)
			return resp, nil
		}
		value, problem, logged := check(resp.Text, schema)
		if problem == "" {
			metrics.ExternalRequest(a.profile.Name, metrics.KindLLM, metrics.OK)
			resp.JSON = value
			return resp, nil
		}
		metrics.ExternalRequest(a.profile.Name, metrics.KindLLM, string(providers.InvalidResponse))
		a.log.Info("llm reply rejected", "call_ref", req.CallRef, "profile", a.profile.Name, "repair", repair,
			"problem", logged)
		if repair == providers.RepairAttempts {
			return nil, &providers.Error{Class: providers.InvalidResponse, Code: "repair_exhausted"}
		}
		// The original messages, then the previous reply and a fixed
		// request naming only the problem (§3.4).
		messages = append(messages[:original:original],
			providers.Message{Role: providers.RoleAssistant, Content: resp.Text},
			providers.Message{Role: providers.RoleUser, Content: repairMessage(problem)})
	}
}

// messages returns the request's messages; in json_object and prompt modes
// with the schema added to the system message (§3.3).
func (a *Adapter) messages(req providers.LlmRequest) []providers.Message {
	msgs := append([]providers.Message(nil), req.Messages...)
	if req.Output.Type != providers.OutputJSON || a.profile.StructuredOutput == ModeJSONSchema {
		return msgs
	}
	var compact bytes.Buffer
	if json.Compact(&compact, req.Output.Schema) != nil {
		compact.Reset()
		compact.Write(req.Output.Schema)
	}
	text := SchemaInstruction + compact.String()
	for i, m := range msgs {
		if m.Role == providers.RoleSystem {
			msgs[i].Content += text
			return msgs
		}
	}
	return append([]providers.Message{{Role: providers.RoleSystem, Content: strings.TrimLeft(text, "\n")}}, msgs...)
}

// body builds the request body (§3.2): the profile's extra_body, then the
// fixed fields on top.
func (a *Adapter) body(req providers.LlmRequest, messages []providers.Message, maxTokens int) ([]byte, error) {
	b := map[string]any{}
	for k, v := range a.profile.ExtraBody {
		b[k] = v
	}
	b["model"] = a.profile.Model
	b["messages"] = messages
	b["max_tokens"] = maxTokens
	b["temperature"] = 0
	b["stream"] = false
	delete(b, "response_format")
	if req.Output.Type == providers.OutputJSON {
		switch a.profile.StructuredOutput {
		case ModeJSONSchema:
			b["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": "output", "schema": req.Output.Schema, "strict": true}}
		case ModePrompt:
		default:
			b["response_format"] = map[string]any{"type": "json_object"}
		}
	}
	return json.Marshal(b)
}

// reply is the part of a chat completion the adapter reads.
type reply struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage providers.Usage `json:"usage"`
}

// call sends one request and returns the reply content. A failed request
// is counted in the metrics here; a reply is counted by Complete once it is
// checked.
func (a *Adapter) call(ctx context.Context, req providers.LlmRequest, messages []providers.Message,
	maxTokens, repair int) (*providers.LlmResponse, error) {
	start := time.Now()
	resp, status, perr := a.send(ctx, req, messages, maxTokens)
	attrs := []any{"call_ref", req.CallRef, "profile", a.profile.Name, "repair", repair, "status", status,
		"latency_ms", time.Since(start).Milliseconds()}
	if perr != nil {
		metrics.ExternalRequest(a.profile.Name, metrics.KindLLM, string(perr.Class))
		a.log.Info("llm request failed", append(attrs, "class", string(perr.Class), "code", perr.Code)...)
		return nil, perr
	}
	a.log.Info("llm request done", append(attrs, "model", resp.Model, "finish_reason", resp.FinishReason,
		"prompt_tokens", resp.Usage.PromptTokens, "completion_tokens", resp.Usage.CompletionTokens)...)
	return resp, nil
}

// send does one HTTP request and maps the reply (§3.2, §3.4).
func (a *Adapter) send(ctx context.Context, req providers.LlmRequest, messages []providers.Message,
	maxTokens int) (*providers.LlmResponse, int, *providers.Error) {
	body, err := a.body(req, messages, maxTokens)
	if err != nil {
		return nil, 0, &providers.Error{Class: providers.PermanentConfig, Code: "bad_request_body"}
	}
	ctx, cancel := context.WithTimeout(ctx, providers.LlmRequestTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.profile.BaseURL, "/")+completionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &providers.Error{Class: providers.PermanentConfig, Code: "bad_request_url"}
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if tok := a.profile.Token(); tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := a.client.Do(hr)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, &providers.Error{Class: providers.Retryable, Code: "timeout"}
		}
		return nil, 0, &providers.Error{Class: providers.Retryable, Code: providers.TransportCode(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The error body is never read into memory, logged or stored
		// (§1.4, privacy.md §6).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, a.statusError(resp, messages)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, resp.StatusCode, &providers.Error{Class: providers.Retryable, Code: "timeout"}
	case err != nil:
		return nil, resp.StatusCode, &providers.Error{Class: providers.Retryable, Code: "connection_lost"}
	case len(b) > maxResponseBytes:
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "response_too_large"}
	}
	var r reply
	if json.Unmarshal(b, &r) != nil {
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "not_json"}
	}
	if len(r.Choices) == 0 {
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "no_choices"}
	}
	c := r.Choices[0]
	switch {
	case c.FinishReason == "length":
		// Never parsed or repaired (§3.4).
		return nil, resp.StatusCode, &providers.Error{Class: providers.OutputTruncated, Code: "finish_length"}
	case c.FinishReason != "stop":
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "finish_other"}
	case c.Message.Content == nil:
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "no_content"}
	}
	return &providers.LlmResponse{Text: *c.Message.Content, FinishReason: c.FinishReason, Usage: r.Usage,
		Model: r.Model}, resp.StatusCode, nil
}

// statusError maps an HTTP status to an error class (§3.4). Statuses §3.4
// does not list follow §1.4: other 5xx are retryable, redirects and other
// 4xx permanent_config.
func (a *Adapter) statusError(resp *http.Response, messages []providers.Message) *providers.Error {
	code := "http_" + strconv.Itoa(resp.StatusCode)
	switch s := resp.StatusCode; {
	case s == 408 || s == 429 || s >= 500:
		return &providers.Error{Class: providers.Retryable, Code: code,
			RetryAfter: providers.RetryAfter(resp.Header.Get("Retry-After"))}
	case s == 400 && float64(EstimateTokens(messages)) > inputTooLargeShare*float64(a.profile.ContextTokens):
		return &providers.Error{Class: providers.InputTooLarge, Code: code}
	}
	return &providers.Error{Class: providers.PermanentConfig, Code: code}
}

// EstimateTokens is the §3.5 estimate for the messages of a call:
// ceil(chars / 3.0) of their contents.
func EstimateTokens(messages []providers.Message) int {
	n := 0
	for _, m := range messages {
		n += len([]rune(m.Content))
	}
	return llmplan.Tokens(n)
}

// check strips one surrounding code fence, parses the content and validates
// it (§3.3). It returns the JSON value, or a problem description that names
// only the JSON pointer and the rule, never content: problem for the repair
// message, logged for the log. In logged the pointer stops before the first
// key the schema does not define, so keys the model invented never reach a
// log.
func check(text string, schema *jsonschema.Schema) (value json.RawMessage, problem, logged string) {
	raw := []byte(stripFence(text))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	const notJSON = "the reply is not one valid JSON value"
	if dec.Decode(&v) != nil || dec.InputOffset() != int64(len(bytes.TrimRight(raw, " \t\r\n"))) {
		return nil, notJSON, notJSON
	}
	if echo(v) {
		const msg = "the reply repeats the JSON Schema instead of giving data that matches it"
		return nil, msg, msg
	}
	if err := schema.Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			const msg = "the reply does not match the JSON Schema"
			return nil, msg, msg
		}
		ve = leaf(ve)
		rule := strings.Join(ve.ErrorKind.KeywordPath(), "/")
		const format = "the value at JSON pointer %q breaks the schema keyword %q"
		return nil, fmt.Sprintf(format, pointer(ve.InstanceLocation), rule),
			fmt.Sprintf(format, pointer(defined(schema, ve.InstanceLocation)), rule)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, bytes.TrimSpace(raw)); err != nil {
		return nil, notJSON, notJSON
	}
	return compact.Bytes(), "", ""
}

// defined returns the leading tokens of a JSON pointer that the schema
// defines: object keys listed in properties and array indexes under items.
// It stops at the first other token.
func defined(s *jsonschema.Schema, tokens []string) []string {
	for i, tok := range tokens {
		if s = child(s, tok, 0); s == nil {
			return tokens[:i]
		}
	}
	return tokens
}

// child is the subschema of s for one pointer token, looking through $ref;
// nil when s does not define it.
func child(s *jsonschema.Schema, tok string, depth int) *jsonschema.Schema {
	if s == nil || depth > 32 {
		return nil
	}
	if next, ok := s.Properties[tok]; ok {
		return next
	}
	if _, err := strconv.Atoi(tok); err == nil && s.Items2020 != nil {
		return s.Items2020
	}
	return child(s.Ref, tok, depth+1)
}

// stripFence removes one surrounding Markdown code fence (``` or ```json).
func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") || len(t) < 6 {
		return s
	}
	t = strings.TrimSuffix(t[3:], "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 && !strings.ContainsAny(t[:i], "{[\"") {
		t = t[i+1:] // drop the info string, e.g. "json"
	}
	return t
}

// echo reports schema echo: a top-level object with $schema, or with both
// type and properties (§3.3).
func echo(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, hasSchema := m["$schema"]
	_, hasType := m["type"]
	_, hasProps := m["properties"]
	return hasSchema || (hasType && hasProps)
}

// leaf follows the first cause down to the most specific error.
func leaf(ve *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	return ve
}

// pointer builds an RFC 6901 JSON pointer from reference tokens.
func pointer(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteString("/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(t))
	}
	return b.String()
}

// repairMessage is the fixed user message of a repair call (§3.4).
func repairMessage(problem string) string {
	return "Your previous reply was rejected: " + problem +
		". Reply again with only the corrected JSON value that validates against the JSON Schema."
}
