package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// Message is one chat message. Its content is the string Content, or, for
// the user message of a vision call, the content parts Parts (§3.1, §3.6);
// with Parts set, Content is not used. In JSON, content is a string or a
// list of parts.
type Message struct {
	Role    string
	Content string
	Parts   []ContentPart
}

// Content part types (§3.6).
const (
	PartText     = "text"
	PartImageURL = "image_url"
)

// ImageDataURLPrefix starts the only image URLs a call may carry: a base64
// PNG in a data URL (§3.6). The provider fetches nothing.
const ImageDataURLPrefix = "data:image/png;base64,"

// ContentPart is one part of a user message: {"type": "text", "text"} or
// {"type": "image_url", "image_url": {"url"}} (§3.6).
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the image of an image_url part.
type ImageURL struct {
	URL string `json:"url"`
}

type messageJSON struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// MarshalJSON writes content as a string, or as the list of parts.
func (m Message) MarshalJSON() ([]byte, error) {
	var content any = m.Content
	if m.Parts != nil {
		content = m.Parts
	}
	b, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	return json.Marshal(messageJSON{Role: m.Role, Content: b})
}

// UnmarshalJSON reads content as a string or as a list of parts.
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*m = Message{Role: raw.Role}
	if t := bytes.TrimSpace(raw.Content); len(t) > 0 && t[0] == '[' {
		if err := json.Unmarshal(t, &m.Parts); err != nil {
			return err
		}
		if m.Parts == nil {
			m.Parts = []ContentPart{}
		}
		return nil
	}
	if len(raw.Content) == 0 {
		return nil
	}
	return json.Unmarshal(raw.Content, &m.Content)
}

// HasImage reports whether a message carries an image part.
func (m Message) HasImage() bool {
	for _, p := range m.Parts {
		if p.Type == PartImageURL {
			return true
		}
	}
	return false
}

// Text returns the text of a message: Content, or its text parts joined.
func (m Message) Text() string {
	if m.Parts == nil {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ErrBadPart is returned by CheckParts.
var ErrBadPart = errors.New("content part")

// CheckParts checks the content parts of the messages (§3.6): parts only
// in user messages, each a non-empty text part or an image_url part with a
// data:image/png;base64 URL. Any other URL is refused before a request is
// sent.
func CheckParts(messages []Message) error {
	for i, m := range messages {
		if m.Parts == nil {
			continue
		}
		if m.Role != RoleUser || len(m.Parts) == 0 {
			return fmt.Errorf("%w: messages[%d]: content parts only in a non-empty user message", ErrBadPart, i)
		}
		for j, p := range m.Parts {
			switch {
			case p.Type == PartText && p.ImageURL == nil:
			case p.Type == PartImageURL && p.Text == "" && p.ImageURL != nil &&
				strings.HasPrefix(p.ImageURL.URL, ImageDataURLPrefix):
			default:
				return fmt.Errorf("%w: messages[%d].content[%d]: want a text part or an image_url part with a %s URL",
					ErrBadPart, i, j, ImageDataURLPrefix)
			}
		}
	}
	return nil
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
