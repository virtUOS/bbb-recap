package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxLLMRequestBytes bounds a mock LLM request body.
const maxLLMRequestBytes = 64 << 20

// mountLLM serves the openai-chat endpoint (docs/reference/providers.md §3.2).
// A request whose output schema has the property chunk_summary gets
// testdata/render/llm/chunk-notes.v1/valid.base.json, any other request
// testdata/render/F-01/summary.json (the rule of contracts/render/README.md
// "--summarize"), wrapped in the chat completion of
// testdata/providers/llm/chat-completion.spec-derived.json. The schema is
// read from response_format (json_schema mode) or, in json_object and prompt
// modes, found in the system message. With -llm-key it requires
// Bearer <key>.
func mountLLM(mux *http.ServeMux, c config) error {
	var replies [2][]byte // summary, chunk notes
	for i, p := range [][]string{{"render", "F-01", "summary.json"},
		{"render", "llm", "chunk-notes.v1", "valid.base.json"}} {
		b, err := llmCompletion(c.Fixtures, p...)
		if err != nil {
			return err
		}
		replies[i] = b
	}
	key, err := readKey(c.LLMKeyFile)
	if err != nil {
		return err
	}
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status, chunk := llmStatus(r, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch {
		case status != http.StatusOK:
			_, _ = w.Write([]byte(`{"error":{"message":"` + http.StatusText(status) + `"}}`))
		case chunk:
			_, _ = w.Write(replies[1])
		default:
			_, _ = w.Write(replies[0])
		}
		c.Log.Info("mock llm request", "status", status, "chunk_notes", chunk,
			"latency_ms", time.Since(start).Milliseconds())
	})
	return nil
}

// llmCompletion returns the chat completion template with the content of the
// fixture file at path as the message content.
func llmCompletion(fixtures string, path ...string) ([]byte, error) {
	content, err := os.ReadFile(filepath.Join(append([]string{fixtures}, path...)...))
	if err != nil {
		return nil, err
	}
	tmpl, err := os.ReadFile(filepath.Join(fixtures, "providers", "llm", "chat-completion.spec-derived.json"))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(tmpl, &doc); err != nil {
		return nil, err
	}
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		return nil, errNoChoices
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return nil, errNoChoices
	}
	msg["content"] = string(content)
	return json.Marshal(doc)
}

var errNoChoices = errors.New("chat completion template has no choices[0].message")

// llmStatus checks the credential and reads the request. chunk is true when
// its output schema is chunk-notes (property chunk_summary). Nothing is kept.
func llmStatus(r *http.Request, key string) (status int, chunk bool) {
	if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
		return http.StatusUnauthorized, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLLMRequestBytes+1))
	if err != nil || len(body) > maxLLMRequestBytes {
		return http.StatusBadRequest, false
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat *struct {
			JSONSchema *struct {
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return http.StatusBadRequest, false
	}
	if rf := req.ResponseFormat; rf != nil && rf.JSONSchema != nil {
		_, chunk = rf.JSONSchema.Schema.Properties["chunk_summary"]
		return http.StatusOK, chunk
	}
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, `"chunk_summary"`) {
			return http.StatusOK, true
		}
	}
	return http.StatusOK, false
}
