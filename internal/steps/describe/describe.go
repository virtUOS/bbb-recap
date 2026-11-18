// Package describe implements the describe step (docs/reference/templates.md
// §3.2, providers.md §3.6, ADR-017): one vision call per distinct slide
// image of the normalized meeting, whose description later replaces the
// slide's text in the SLIDES section of summarize. It is source neutral
// (ADR-013): it reads only the normalized meeting.
//
// # Calls
//
// One call per distinct image, in order of first appearance in slides[], at
// most the vision profile's max_inflight at once. The request has the
// format's prompts/slide-describe.md as system message and two user content
// parts: the fixed text UserText and the image as a base64 PNG data URL.
// Nothing else of the meeting is sent. The reply is slide-description.v1
// JSON; text and visual are joined by LF, cleaned as slide text and cut to
// 1 000 code points.
//
// # Outcome per image
//
//	valid reply, description not empty        described
//	valid reply, empty description            fallback empty
//	invalid_response, output_truncated,       fallback (the class name)
//	input_too_large
//	retryable                                 before the last attempt: the step is
//	                                          transient; on the last attempt fallback
//	                                          retryable
//	permanent_config                          the step is permanent (config_error)
//
// A fallback leaves the slide with its text; the job neither fails nor holds
// for it. Bounded delay: before the last attempt the first retryable result
// stops new calls; on the last attempt 3 retryable results in a row (in the
// order the calls end) stop new calls, and every image without a result
// falls back (retryable). Calls in flight always finish.
//
// Nothing here logs content: IDs, counts, hashes, sizes and latencies only.
package describe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/slidetext"
)

// Step is the step name.
const Step = "describe"

// Fixed parts of a vision call (templates.md §3.2).
const (
	// Prompt is the format's system prompt, prompts/slide-describe.md.
	Prompt = "slide-describe"
	// Schema is the reply schema of the format.
	Schema = "slide-description.v1"
	// UserText is the text part of the user message.
	UserText = "Describe the slide in this image."
	// MaxOutputTokens caps max_output_tokens: min(700, the profile's).
	MaxOutputTokens = 700
	// RetryableStop is the number of retryable results in a row after which
	// no new call starts on the step's last attempt (provisional).
	RetryableStop = 3
)

// Fallback reasons: why an image has no description
// (recap_slide_image_fallbacks_total{step="describe"}).
const (
	FallbackEmpty           = "empty"
	FallbackInvalidResponse = "invalid_response"
	FallbackOutputTruncated = "output_truncated"
	FallbackInputTooLarge   = "input_too_large"
	FallbackRetryable       = "retryable"
)

// Image is one distinct slide image: its ref sha256:<hex> and a function
// that reads its PNG bytes.
type Image struct {
	Ref  string
	Load func(ctx context.Context) ([]byte, error)
}

// Result is the outcome of one image: a description, or the reason of the
// fallback.
type Result struct {
	Description string
	Fallback    string
}

// Describer runs the vision calls of one profile and one format.
type Describer struct {
	LLM     providers.LLM
	Profile *providers.Profile
	Format  *format.Format
	// Sem limits concurrent calls to the profile (max_inflight per worker
	// process, shared with summarize when both use one profile). Nil: a new
	// one of the profile's max_inflight.
	Sem chan struct{}
	Log *slog.Logger
	// OnFallback, if set, is called once per image that falls back.
	OnFallback func(reason string)
}

// Stop is why Run stopped before every image had a result: a transient
// step (retryable before the last attempt, or an image that could not be
// read) or a permanent one (permanent_config).
type Stop struct {
	Err       *providers.Error
	Permanent bool
}

func (s *Stop) Error() string { return "describe stopped: " + string(s.Err.Class) + " " + s.Err.Code }

// ErrFormat means the format lacks the describe prompt or schema.
var ErrFormat = errors.New("describe: the format has no slide-describe prompt or slide-description.v1 schema")

// run is the state of one Run.
type run struct {
	*Describer
	jobID   string
	last    bool
	system  string
	schema  json.RawMessage
	mu      sync.Mutex
	results map[string]Result
	inARow  int   // retryable results in a row (last attempt)
	stop    *Stop // set: no new call
}

// Run describes the images in order. pos gives each image its position in
// order of first appearance (1-based; for the call_ref). last is set on
// the step's last attempt. It returns the results of the images that got
// one in this run, and a *Stop when the step must not end ok. ErrFormat
// when the format is incomplete.
func (d *Describer) Run(ctx context.Context, jobID string, images []Image, pos map[string]int,
	last bool) (map[string]Result, error) {
	sys, ok1 := d.Format.Prompt(Prompt)
	schema, ok2 := d.Format.Schema(Schema)
	if !ok1 || !ok2 {
		return nil, ErrFormat
	}
	if d.Sem == nil {
		d.Sem = make(chan struct{}, max(d.Profile.MaxInflight, 1))
	}
	r := &run{Describer: d, jobID: jobID, last: last, system: sys, schema: schema, results: map[string]Result{}}
	var wg sync.WaitGroup
	for _, img := range images {
		acquired := false
		select {
		case d.Sem <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		r.mu.Lock()
		stopped := r.stop != nil || ctx.Err() != nil
		r.mu.Unlock()
		if stopped {
			if acquired {
				<-d.Sem
			}
			break
		}
		wg.Go(func() {
			defer func() { <-d.Sem }()
			r.describe(ctx, img, pos[img.Ref])
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return r.results, &Stop{Err: &providers.Error{Class: providers.Retryable, Code: "cancelled"}}
	}
	if r.stop != nil {
		if r.stop.Permanent || !last || r.stop.Err.Code == codeLoad {
			return r.results, r.stop
		}
	}
	// On the last attempt every image without a result falls back.
	for _, img := range images {
		if _, ok := r.results[img.Ref]; !ok {
			r.fallback(img.Ref, FallbackRetryable)
		}
	}
	return r.results, nil
}

// codeLoad is the error code of an image that could not be read.
const codeLoad = "image_read"

// describe makes the call for one image and records its outcome.
func (r *run) describe(ctx context.Context, img Image, n int) {
	png, err := img.Load(ctx)
	if err != nil {
		r.mu.Lock()
		if r.stop == nil {
			r.stop = &Stop{Err: &providers.Error{Class: providers.Retryable, Code: codeLoad}}
		}
		r.mu.Unlock()
		return
	}
	resp, err := r.LLM.Complete(ctx, providers.LlmRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: r.system},
			{Role: providers.RoleUser, Parts: []providers.ContentPart{
				{Type: providers.PartText, Text: UserText},
				{Type: providers.PartImageURL, ImageURL: &providers.ImageURL{
					URL: providers.ImageDataURLPrefix + base64.StdEncoding.EncodeToString(png)}},
			}},
		},
		Output:          providers.Output{Type: providers.OutputJSON, Schema: r.schema},
		MaxOutputTokens: min(MaxOutputTokens, r.Profile.MaxOutputTokens),
		CallRef:         r.jobID + ":" + Step + ":slide:" + strconv.Itoa(n),
	})
	if ctx.Err() != nil {
		return // interrupted: no result, the step runs again
	}
	if err == nil {
		desc, ok := description(resp.JSON)
		if !ok {
			r.fallback(img.Ref, FallbackInvalidResponse)
			return
		}
		r.mu.Lock()
		r.inARow = 0
		r.mu.Unlock()
		if desc == "" {
			r.fallback(img.Ref, FallbackEmpty)
			return
		}
		r.mu.Lock()
		r.results[img.Ref] = Result{Description: desc}
		r.mu.Unlock()
		return
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		pe = &providers.Error{Class: providers.Retryable, Code: "llm_internal"}
	}
	switch pe.Class {
	case providers.PermanentConfig:
		r.mu.Lock()
		if r.stop == nil || !r.stop.Permanent {
			r.stop = &Stop{Err: pe, Permanent: true}
		}
		r.mu.Unlock()
	case providers.Retryable:
		r.retryable(img.Ref, pe)
	case providers.InvalidResponse, providers.OutputTruncated, providers.InputTooLarge:
		r.fallback(img.Ref, string(pe.Class))
	default:
		r.retryable(img.Ref, pe)
	}
}

// retryable handles a retryable result: before the last attempt it stops
// the step; on the last attempt the image falls back, and RetryableStop of
// them in a row stop new calls.
func (r *run) retryable(ref string, pe *providers.Error) {
	r.mu.Lock()
	if !r.last {
		if r.stop == nil {
			r.stop = &Stop{Err: pe}
		}
		r.mu.Unlock()
		return
	}
	r.inARow++
	if r.inARow >= RetryableStop && r.stop == nil {
		r.stop = &Stop{Err: pe}
	}
	r.mu.Unlock()
	r.fallback(ref, FallbackRetryable)
}

// fallback records an image without a description. Any result other than
// retryable ends a run of retryable results.
func (r *run) fallback(ref, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason != FallbackRetryable {
		r.inARow = 0
	}
	r.results[ref] = Result{Fallback: reason}
	if r.OnFallback != nil {
		r.OnFallback(reason)
	}
}

// description joins text and visual of a slide-description.v1 reply by LF
// (an empty part is left out with its LF), cleans the result as slide text
// and cuts it to 1 000 code points (templates.md §3.2).
func description(reply json.RawMessage) (string, bool) {
	var d struct {
		Text   string `json:"text"`
		Visual string `json:"visual"`
	}
	if err := json.Unmarshal(reply, &d); err != nil {
		return "", false
	}
	joined := d.Text
	switch {
	case joined == "":
		joined = d.Visual
	case d.Visual != "":
		joined += "\n" + d.Visual
	}
	return slidetext.Cut(slidetext.Clean([]byte(joined))), true
}
