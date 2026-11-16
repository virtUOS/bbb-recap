package providers

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// LLM is the LLM adapter of providers.md §3.1: one operation, Complete. Its
// errors are *Error with a class of §3.4. JSON repair calls (§3.4) happen
// inside Complete; the adapter never retries otherwise (ADR-008).
type LLM interface {
	Complete(ctx context.Context, req LlmRequest) (*LlmResponse, error)
}

// Message roles (providers.md §3.1): system and user; assistant only for
// JSON repair.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Output types of an LlmRequest.
const (
	OutputText = "text"
	OutputJSON = "json"
)

// Output is what the call must return: text, or JSON that validates against
// Schema (draft 2020-12, owned by the format, templates.md §4).
type Output struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// LlmRequest is one call (providers.md §3.1).
type LlmRequest struct {
	Messages        []Message `json:"messages"`
	Output          Output    `json:"output"`
	MaxOutputTokens int       `json:"max_output_tokens"` // ≤ the profile's; 0: the profile's
	CallRef         string    `json:"call_ref"`          // <job_id>:<step>:<call-name>:<chunk>, logs only
}

// Usage is the token usage the provider reports.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// LlmResponse is the reply of a successful call (providers.md §3.1). Text is
// the raw content; JSON is the parsed, validated value (nil for text output).
// Model is the model the provider reports, not the profile's.
type LlmResponse struct {
	Text         string          `json:"text"`
	JSON         json.RawMessage `json:"json"`
	FinishReason string          `json:"finish_reason"`
	Usage        Usage           `json:"usage"`
	Model        string          `json:"model"`
}

// LLM error classes besides Retryable, PermanentConfig and InvalidResponse
// (providers.md §3.4).
const (
	InputTooLarge   ErrorClass = "input_too_large"
	OutputTruncated ErrorClass = "output_truncated"
)

// CompileSchema compiles a JSON Schema (draft 2020-12 unless the schema says
// otherwise). Formats are asserted, as in internal/schemas.
func CompileSchema(schema []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	const name = "output.schema.json"
	if err := c.AddResource(name, doc); err != nil {
		return nil, err
	}
	return c.Compile(name)
}
