// Package whisperx is the ASR dialect whisperx-family of
// docs/reference/providers.md §2 (whisperx-api-server): one synchronous
// multipart POST to {base_url}/v1/audio/transcriptions whose body is the
// result. Plain net/http, no SDK (ADR-008). The HTTP client never retries;
// the job queue does.
package whisperx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

// Dialect is the dialect name in provider profiles.
const Dialect = "whisperx-family"

const (
	maxResponseBytes   = 64 << 20 // assumption: far above any verbose_json of one track
	transcriptionsPath = "/v1/audio/transcriptions"
)

var languageRE = regexp.MustCompile(`^[a-z]{2,3}$`)

// fileExtensions maps each media type the adapter sends (§2.4
// media_types) to the extension of the upload file name. Only WebM has
// been tried against the server (§7).
var fileExtensions = map[string]string{
	"audio/webm": ".webm",
}

// Adapter talks to one whisperx-family profile.
type Adapter struct {
	profile *providers.Profile
	client  *http.Client
	log     *slog.Logger
}

// New returns the adapter for an ASR profile with dialect whisperx-family.
func New(p *providers.Profile, log *slog.Logger) *Adapter {
	return &Adapter{profile: p, client: providers.NewHTTPClient(), log: log}
}

// Capabilities of the dialect (§2.4). The profile's max_file_bytes overrides
// the (unknown) server limit.
func (a *Adapter) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		Diarization:     true, // supported by the server, never requested
		MaxFileBytes:    a.profile.MaxFileBytes,
		MediaTypes:      slices.Sorted(maps.Keys(fileExtensions)),
		RemoteRetention: "ttl",
	}
}

// handle is the state of one synchronous request: the reply body on
// success, else the error of Submit.
type handle struct {
	body []byte
	err  *providers.Error
}

// Submit sends the track and waits for the reply (§2.2).
func (a *Adapter) Submit(ctx context.Context, req providers.AsrRequest) (providers.AsrHandle, error) {
	h := &handle{}
	start := time.Now()
	status := 0
	h.body, status, h.err = a.send(ctx, req)
	attrs := []any{"job_id", req.JobID, "track_ref", req.TrackRef, "profile", a.profile.Name,
		"status", status, "latency_ms", time.Since(start).Milliseconds(), "bytes", req.Audio.Size}
	if h.err != nil {
		attrs = append(attrs, "class", string(h.err.Class), "code", h.err.Code)
		a.log.Info("asr request failed", attrs...)
		return h, h.err
	}
	a.log.Info("asr request done", append(attrs, "response_bytes", len(h.body))...)
	return h, nil
}

// Status makes no network call: the request is over when Submit returns.
func (a *Adapter) Status(_ context.Context, ah providers.AsrHandle) (providers.AsrStatus, error) {
	h, ok := ah.(*handle)
	if !ok || h.err != nil {
		return providers.StatusFailed, nil
	}
	return providers.StatusSucceeded, nil
}

// Result parses the reply kept by Submit (§2.3). No network call.
func (a *Adapter) Result(_ context.Context, ah providers.AsrHandle) (*providers.AsrResult, error) {
	h, ok := ah.(*handle)
	if !ok {
		return nil, &providers.Error{Class: providers.InvalidResponse, Code: "no_handle"}
	}
	if h.err != nil {
		return nil, h.err
	}
	r, err := parse(h.body, a.profile.Model)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Cleanup is a no-op: the server has no delete endpoint (§2.4).
func (a *Adapter) Cleanup(context.Context, providers.AsrHandle) error { return nil }

// send does the request and returns the reply body of a 2xx answer.
func (a *Adapter) send(ctx context.Context, req providers.AsrRequest) ([]byte, int, *providers.Error) {
	if limit := a.profile.MaxFileBytes; limit != nil && req.Audio.Size > *limit {
		return nil, 0, &providers.Error{Class: providers.PermanentInput, Code: "file_too_large"}
	}
	if _, ok := fileExtensions[req.Audio.MediaType]; !ok {
		return nil, 0, &providers.Error{Class: providers.PermanentInput, Code: "unsupported_media_type"}
	}
	timeout := req.AttemptTimeout
	if timeout <= 0 {
		timeout = providers.AsrAttemptTimeout(req.DurationMS)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	audio, err := req.Audio.Open(ctx)
	if err != nil {
		return nil, 0, &providers.Error{Class: providers.Retryable, Code: "audio_unavailable"}
	}
	body, contentType := a.multipart(req, audio)
	defer func() { _ = body.Close() }()

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.profile.BaseURL, "/")+transcriptionsPath, body)
	if err != nil {
		return nil, 0, &providers.Error{Class: providers.PermanentConfig, Code: "bad_request_url"}
	}
	hr.Header.Set("Content-Type", contentType)
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("X-Request-ID", req.IdempotencyKey)
	if tok := a.profile.Token(); tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := a.client.Do(hr)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, &providers.Error{Class: providers.Retryable, Code: "timeout", RetryAfter: providers.AsrTimeoutRetry}
		}
		return nil, 0, &providers.Error{Class: providers.Retryable, Code: providers.TransportCode(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The error body only selects a code; it is never read into memory,
		// logged or stored (§1.4, privacy.md §6).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, providers.AsrStatusError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, resp.StatusCode, &providers.Error{Class: providers.Retryable, Code: "timeout", RetryAfter: providers.AsrTimeoutRetry}
	case err != nil:
		return nil, resp.StatusCode, &providers.Error{Class: providers.Retryable, Code: "connection_lost"}
	case len(b) > maxResponseBytes:
		return nil, resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "response_too_large"}
	}
	return b, resp.StatusCode, nil
}

// multipart streams the form of §2.2 through a pipe: fields first, then
// the audio, copied from its reader without touching local disk.
func (a *Adapter) multipart(req providers.AsrRequest, audio io.ReadCloser) (io.ReadCloser, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = audio.Close() }()
		pw.CloseWithError(writeForm(mw, req, a.profile.Model, audio))
	}()
	return pr, mw.FormDataContentType()
}

func writeForm(mw *multipart.Writer, req providers.AsrRequest, model string, audio io.Reader) error {
	fields := [][2]string{
		{"model", model},
		{"response_format", "verbose_json"},
		{"align", "true"},
		{"suppress_numerals", "false"},
		{"diarize", "false"}, // always off (ADR-001)
	}
	if req.LanguageHint != "" {
		fields = append(fields, [2]string{"language", req.LanguageHint})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s%s"`, req.TrackRef,
		fileExtensions[req.Audio.MediaType]))
	hdr.Set("Content-Type", req.Audio.MediaType)
	w, err := mw.CreatePart(hdr)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, audio)
	if err != nil {
		return err
	}
	if n != req.Audio.Size {
		return fmt.Errorf("audio has %d bytes, want %d", n, req.Audio.Size)
	}
	return mw.Close()
}

// parse maps a verbose_json reply to an AsrResult (§2.3). model is the
// profile's: the reply reports none that §2.3 names.
func parse(body []byte, model string) (*providers.AsrResult, error) {
	invalid := func(code string) error {
		return &providers.Error{Class: providers.InvalidResponse, Code: code}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, invalid("not_json_object")
	}
	raw, ok := top["segments"]
	if !ok {
		return nil, invalid("no_segments")
	}
	// segments is a list, or an object {segments, word_segments}.
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil, invalid("bad_segments")
		}
		raw = bytes.TrimSpace(obj["segments"])
	}
	var segs []map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &segs) != nil {
		return nil, invalid("bad_segments")
	}

	r := &providers.AsrResult{Model: model, Segments: []providers.Segment{}}
	var lang string
	if json.Unmarshal(top["language"], &lang) == nil && languageRE.MatchString(lang) {
		r.Language = &lang
	}
	for _, s := range segs {
		start, ok1 := millis(s["start"])
		end, ok2 := millis(s["end"])
		if !ok1 || !ok2 {
			return nil, invalid("bad_segment_time")
		}
		var text string
		if json.Unmarshal(s["text"], &text) != nil {
			return nil, invalid("bad_segment_text")
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		r.Segments = append(r.Segments, providers.Segment{StartMS: start, EndMS: max(end, start), Text: text})
	}
	return r, nil
}

// millis converts a JSON number of seconds to integer ms, rounding half
// away from zero (§1.3). Anything but a non-negative number is invalid.
func millis(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false // missing, null, string, …
	}
	var s float64
	if json.Unmarshal(raw, &s) != nil || s < 0 {
		return 0, false
	}
	ms := math.Round(s * 1000)
	if ms > math.MaxInt64/2 {
		return 0, false
	}
	return int64(ms), true
}
