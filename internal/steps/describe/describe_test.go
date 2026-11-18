package describe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

const formatsDir = "../../../formats"

// fakeLLM answers each call with the next reply of its script; a reply is
// a JSON content or an error class.
type fakeLLM struct {
	mu     sync.Mutex
	script []string
	reqs   []providers.LlmRequest
}

func (f *fakeLLM) Complete(_ context.Context, req providers.LlmRequest) (*providers.LlmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	next := `{"text": "Fixture slide", "visual": ""}`
	if len(f.script) > 0 {
		next, f.script = f.script[0], f.script[1:]
	}
	if class, ok := strings.CutPrefix(next, "error:"); ok {
		return nil, &providers.Error{Class: providers.ErrorClass(class), Code: "fixture"}
	}
	return &providers.LlmResponse{Text: next, JSON: json.RawMessage(next), FinishReason: "stop"}, nil
}

func images(n int) ([]Image, map[string]int) {
	var out []Image
	pos := map[string]int{}
	for i := range n {
		ref := fmt.Sprintf("sha256:%064x", i+1)
		out = append(out, Image{Ref: ref, Load: func(context.Context) ([]byte, error) {
			return []byte(ref), nil
		}})
		pos[ref] = i + 1
	}
	return out, pos
}

func describer(t *testing.T, llm providers.LLM, inflight int) *Describer {
	t.Helper()
	f, err := format.Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	return &Describer{LLM: llm, Profile: &providers.Profile{Name: "vision-test", MaxInflight: inflight,
		MaxOutputTokens: 2048}, Format: f, Log: slog.New(slog.DiscardHandler)}
}

const r = "error:retryable"

// On the last attempt, with max_inflight 1, the step stops after 3
// retryable results in a row; a result in between starts the count again
// (templates.md §3.2 "Bounded delay"). Every image without a result falls
// back as retryable.
func TestRetryableStopInARow(t *testing.T) {
	for _, c := range []struct {
		name   string
		script []string
		calls  int
	}{
		{"three in a row", []string{r, r, r}, 3},
		{"success resets", []string{r, r, `{"text": "Fixture", "visual": ""}`, r, r, r}, 6},
		{"fallback resets", []string{r, "error:invalid_response", r, r, `{"text": "", "visual": ""}`, r, r, r}, 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			llm := &fakeLLM{script: c.script}
			imgs, pos := images(10)
			res, err := describer(t, llm, 1).Run(context.Background(), "job", imgs, pos, true)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(llm.reqs) != c.calls {
				t.Errorf("%d calls, want %d", len(llm.reqs), c.calls)
			}
			if len(res) != len(imgs) {
				t.Errorf("%d results, want one per image", len(res))
			}
			for i, img := range imgs[c.calls:] {
				if res[img.Ref].Fallback != FallbackRetryable {
					t.Errorf("image %d not sent: %+v, want fallback retryable", c.calls+i+1, res[img.Ref])
				}
			}
		})
	}
}

// Before the last attempt the first retryable result stops the step; the
// results so far are kept.
func TestRetryableBeforeLastAttempt(t *testing.T) {
	llm := &fakeLLM{script: []string{`{"text": "Fixture one", "visual": ""}`, r}}
	imgs, pos := images(5)
	res, err := describer(t, llm, 1).Run(context.Background(), "job", imgs, pos, false)
	var stop *Stop
	if !errors.As(err, &stop) || stop.Permanent || stop.Err.Class != providers.Retryable {
		t.Fatalf("err = %v, want a transient stop", err)
	}
	if len(llm.reqs) != 2 || len(res) != 1 || res[imgs[0].Ref].Description != "Fixture one" {
		t.Errorf("calls %d, results %+v; want 2 calls and the first description", len(llm.reqs), res)
	}
}

// permanent_config stops the step as permanent.
func TestPermanentConfig(t *testing.T) {
	llm := &fakeLLM{script: []string{"error:permanent_config"}}
	imgs, pos := images(3)
	_, err := describer(t, llm, 1).Run(context.Background(), "job", imgs, pos, true)
	var stop *Stop
	if !errors.As(err, &stop) || !stop.Permanent {
		t.Fatalf("err = %v, want a permanent stop", err)
	}
	if len(llm.reqs) != 1 {
		t.Errorf("%d calls, want 1", len(llm.reqs))
	}
}

// Outcomes per image and the request shape (templates.md §3.2,
// providers.md §3.6).
func TestOutcomesAndRequest(t *testing.T) {
	llm := &fakeLLM{script: []string{
		`{"text": "Fixture Title\r\n  Q1  40", "visual": "A chart."}`,
		`{"text": " ", "visual": ""}`,
		"error:output_truncated",
		"error:input_too_large",
		"error:invalid_response",
		`{"text": "", "visual": "A picture."}`,
	}}
	imgs, pos := images(6)
	d := describer(t, llm, 1)
	var fallbacks []string
	d.OnFallback = func(reason string) { fallbacks = append(fallbacks, reason) }
	res, err := d.Run(context.Background(), "job-1", imgs, pos, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{Description: "Fixture Title\nQ1 40\nA chart."}, {Fallback: FallbackEmpty},
		{Fallback: FallbackOutputTruncated}, {Fallback: FallbackInputTooLarge}, {Fallback: FallbackInvalidResponse},
		{Description: "A picture."}}
	for i, img := range imgs {
		if res[img.Ref] != want[i] {
			t.Errorf("image %d: %+v, want %+v", i+1, res[img.Ref], want[i])
		}
	}
	if len(fallbacks) != 4 {
		t.Errorf("fallbacks counted %v, want 4", fallbacks)
	}
	req := llm.reqs[0]
	if req.MaxOutputTokens != 700 || req.CallRef != "job-1:describe:slide:1" || req.Output.Type != providers.OutputJSON {
		t.Errorf("request %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != providers.RoleSystem || req.Messages[1].Role != providers.RoleUser {
		t.Fatalf("messages %+v", req.Messages)
	}
	parts := req.Messages[1].Parts
	if len(parts) != 2 || parts[0].Text != UserText || parts[1].ImageURL == nil ||
		parts[1].ImageURL.URL != providers.ImageDataURLPrefix+base64.StdEncoding.EncodeToString([]byte(imgs[0].Ref)) {
		t.Errorf("user parts %+v", parts)
	}
}

// With max_inflight 2 the calls run two at a time and stop after 3
// retryable results; calls in flight finish.
func TestRetryableStopInflight(t *testing.T) {
	llm := &fakeLLM{script: []string{r, r, r, r, r, r, r, r}}
	imgs, pos := images(8)
	res, err := describer(t, llm, 2).Run(context.Background(), "job", imgs, pos, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(llm.reqs); n < 3 || n > 4 {
		t.Errorf("%d calls, want 3 or 4", n)
	}
	if len(res) != 8 {
		t.Errorf("%d results, want 8", len(res))
	}
}
