// Package transkribierbar is the ASR dialect transkribierbar of
// docs/reference/providers.md §2a: a track is transcribed through the job API of
// a transkribierBAR instance (upload, start, poll, fetch, delete). Plain
// net/http, no SDK (ADR-008). The HTTP client never retries; the job queue
// does.
package transkribierbar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

// Dialect is the dialect name in provider profiles.
const Dialect = "transkribierbar"

const (
	// PollInterval is the wait between two status requests (§2a.1).
	PollInterval = 5 * time.Second
	// MaxFileBytes is the upload limit of transkribierBAR (§2a.4).
	MaxFileBytes int64 = 1 << 30
	// cleanupTimeout bounds the DELETE of Cleanup, which runs even when the
	// caller's context is already cancelled (assumption: the connect
	// timeout of §1.5).
	cleanupTimeout   = providers.ConnectTimeout
	maxResponseBytes = 64 << 20 // assumption: far above any result of one track
	maxErrorBytes    = 64 << 10 // an error body is read only to select a code
	// keepUploadFor bounds how long the file ID of an upload whose
	// transcription could not be started is kept for a retry (assumption:
	// far longer than the queue's retry backoff).
	keepUploadFor = 24 * time.Hour
)

var languageRE = regexp.MustCompile(`^[a-z]{2,3}$`)

// fileExtensions maps each media type the adapter sends (§2a.4
// media_types) to the extension of the upload file name, by which
// transkribierBAR accepts a file (routers/upload.py:16,26-30 @ f3f38f3).
// Only WebM has been tried against an instance.
var fileExtensions = map[string]string{
	"audio/webm": ".webm",
}

// Adapter talks to one transkribierbar profile.
type Adapter struct {
	profile *providers.Profile
	client  *http.Client
	log     *slog.Logger
	// interval is the poll interval; tests shorten it.
	interval time.Duration

	// uploads holds, per job and track, the file of an upload whose
	// transcription could not be started, so that the retry in this
	// process starts it on the same file instead of uploading the track
	// again. transkribierBAR cannot delete a file on its own (§2a.4).
	mu      sync.Mutex
	uploads map[string]upload
}

// upload is a file on the instance that no transcription refers to yet.
type upload struct {
	fileID string
	sha256 string
	at     time.Time
}

// New returns the adapter for an ASR profile with dialect transkribierbar.
func New(p *providers.Profile, log *slog.Logger) *Adapter {
	return &Adapter{
		profile:  p,
		client:   providers.NewHTTPClient(),
		log:      log,
		interval: PollInterval,
		uploads:  map[string]upload{},
	}
}

// Capabilities of the dialect (§2a.4). The profile's max_file_bytes
// overrides the instance limit.
func (a *Adapter) Capabilities() providers.Capabilities {
	limit := MaxFileBytes
	if a.profile.MaxFileBytes != nil {
		limit = *a.profile.MaxFileBytes
	}
	return providers.Capabilities{
		MaxFileBytes:    &limit,
		MediaTypes:      slices.Sorted(maps.Keys(fileExtensions)),
		RemoteRetention: "until_cleanup",
	}
}

// handle is the state of one track: the remote IDs, the deadline of the
// attempt (it bounds Result too), the error of Submit and whether Cleanup
// has deleted the transcription.
type handle struct {
	fileID          string
	transcriptionID string
	idempotencyKey  string
	deadline        time.Time
	err             *providers.Error
	deleted         bool
}

// Submit uploads the track, starts the transcription and polls until it
// is completed or failed (§2a.1 steps 1–3).
func (a *Adapter) Submit(ctx context.Context, req providers.AsrRequest) (providers.AsrHandle, error) {
	start := time.Now()
	timeout := req.AttemptTimeout
	if timeout <= 0 {
		timeout = providers.AsrAttemptTimeout(req.DurationMS)
	}
	h := &handle{idempotencyKey: req.IdempotencyKey, deadline: start.Add(timeout)}
	status := 0
	if limit := a.Capabilities().MaxFileBytes; req.Audio.Size > *limit {
		h.err = &providers.Error{Class: providers.PermanentInput, Code: "file_too_large"}
	} else if _, ok := fileExtensions[req.Audio.MediaType]; !ok {
		h.err = &providers.Error{Class: providers.PermanentInput, Code: "unsupported_media_type"}
	} else {
		ctx, cancel := context.WithDeadline(ctx, h.deadline)
		status, h.err = a.submit(ctx, req, h)
		cancel()
	}
	attrs := []any{"job_id", req.JobID, "track_ref", req.TrackRef, "profile", a.profile.Name,
		"status", status, "latency_ms", time.Since(start).Milliseconds(), "bytes", req.Audio.Size}
	if h.err != nil {
		attrs = append(attrs, "class", string(h.err.Class), "code", h.err.Code)
		a.log.Info("asr request failed", attrs...)
		return h, h.err
	}
	a.log.Info("asr request done", attrs...)
	return h, nil
}

// submit does steps 1–3 and records the IDs in h as soon as they exist. It
// returns the HTTP status of the last request. A file kept from an earlier
// attempt whose start failed is used instead of a new upload; if the
// instance no longer has it (404), the track is uploaded again.
func (a *Adapter) submit(ctx context.Context, req providers.AsrRequest, h *handle) (int, *providers.Error) {
	h.fileID = a.takeUpload(req)
	reused := h.fileID != ""
	if reused {
		a.log.Debug("asr: reusing the uploaded file", "job_id", req.JobID, "track_ref", req.TrackRef)
	} else if status, perr := a.upload(ctx, req, h); perr != nil {
		return status, perr
	}
	status, perr := a.start(ctx, req, h)
	if perr != nil && reused && perr.Code == "http_404" {
		if status, perr = a.upload(ctx, req, h); perr != nil {
			return status, perr
		}
		status, perr = a.start(ctx, req, h)
	}
	if perr != nil {
		if perr.Class == providers.Retryable || perr.Class == providers.InvalidResponse {
			a.keepUpload(req, h.fileID) // the queue retries the track
		}
		return status, perr
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return status, contextError(ctx)
		case <-timer.C:
		}
		var st struct {
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		}
		status, perr = a.call(ctx, h, http.MethodGet, "/api/status/"+url.PathEscape(h.transcriptionID), "", nil, false, &st)
		if perr != nil {
			return status, perr
		}
		switch st.Status {
		case "completed":
			return status, nil
		case "failed":
			return status, failedError(st.ErrorCode)
		case "pending", "processing":
			timer.Reset(a.interval)
		default:
			return status, &providers.Error{Class: providers.InvalidResponse, Code: "unknown_status"}
		}
	}
}

// upload does step 1 and records the file ID in h.
func (a *Adapter) upload(ctx context.Context, req providers.AsrRequest, h *handle) (int, *providers.Error) {
	h.fileID = ""
	audio, err := req.Audio.Open(ctx)
	if err != nil {
		return 0, &providers.Error{Class: providers.Retryable, Code: "audio_unavailable"}
	}
	form, contentType := multipartForm(req, audio)
	defer func() { _ = form.Close() }()
	var up struct {
		ID string `json:"id"`
	}
	status, perr := a.call(ctx, h, http.MethodPost, "/api/upload", contentType, form, false, &up)
	if perr != nil {
		return status, perr
	}
	if up.ID == "" {
		return status, &providers.Error{Class: providers.InvalidResponse, Code: "no_file_id"}
	}
	h.fileID = up.ID
	return status, nil
}

// start does step 2 on h.fileID and records the transcription ID in h.
func (a *Adapter) start(ctx context.Context, req providers.AsrRequest, h *handle) (int, *providers.Error) {
	start := map[string]string{"file_id": h.fileID, "model": a.profile.Model}
	if req.LanguageHint != "" {
		start["language"] = req.LanguageHint
	}
	body, _ := json.Marshal(start) // a map of strings always marshals
	var tr struct {
		ID string `json:"id"`
	}
	status, perr := a.call(ctx, h, http.MethodPost, "/api/transcribe", "application/json", bytes.NewReader(body), true, &tr)
	if perr != nil {
		return status, perr
	}
	if tr.ID == "" {
		return status, &providers.Error{Class: providers.InvalidResponse, Code: "no_transcription_id"}
	}
	h.transcriptionID = tr.ID
	return status, nil
}

// uploadKey identifies a track's upload across attempts.
func uploadKey(req providers.AsrRequest) string { return req.JobID + "/" + req.TrackRef }

// takeUpload returns and forgets the file kept for the track, or "" if
// there is none for the same audio.
func (a *Adapter) takeUpload(req providers.AsrRequest) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.uploads[uploadKey(req)]
	delete(a.uploads, uploadKey(req))
	if !ok || u.sha256 != req.Audio.SHA256 || time.Since(u.at) > keepUploadFor {
		return ""
	}
	return u.fileID
}

// keepUpload keeps the file of a track whose transcription could not be
// started, and forgets files kept longer than keepUploadFor.
func (a *Adapter) keepUpload(req providers.AsrRequest, fileID string) {
	if fileID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, u := range a.uploads {
		if time.Since(u.at) > keepUploadFor {
			delete(a.uploads, k)
		}
	}
	a.uploads[uploadKey(req)] = upload{fileID: fileID, sha256: req.Audio.SHA256, at: time.Now()}
}

// Status makes no network call: the transcription is over when Submit
// returns.
func (a *Adapter) Status(_ context.Context, ah providers.AsrHandle) (providers.AsrStatus, error) {
	h, ok := ah.(*handle)
	if !ok || h.err != nil {
		return providers.StatusFailed, nil
	}
	return providers.StatusSucceeded, nil
}

// Result fetches the completed transcription and maps it (§2a.1 step 4,
// §2a.2), within the deadline of the attempt.
func (a *Adapter) Result(ctx context.Context, ah providers.AsrHandle) (*providers.AsrResult, error) {
	h, ok := ah.(*handle)
	if !ok {
		return nil, &providers.Error{Class: providers.InvalidResponse, Code: "no_handle"}
	}
	if h.err != nil {
		return nil, h.err
	}
	ctx, cancel := context.WithDeadline(ctx, h.deadline)
	defer cancel()
	var body json.RawMessage
	if _, perr := a.call(ctx, h, http.MethodGet, "/api/transcription/"+url.PathEscape(h.transcriptionID), "", nil, false, &body); perr != nil {
		return nil, perr
	}
	r, err := parse(body, a.profile.Model)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Cleanup deletes the transcription and its uploaded file (§2a.4). Without
// a transcription ID there is nothing it can delete (§2a.4). 404 counts as
// done. It runs also after the caller's context is cancelled, so that a
// cancelled job leaves nothing behind, bounded by cleanupTimeout.
func (a *Adapter) Cleanup(ctx context.Context, ah providers.AsrHandle) error {
	h, ok := ah.(*handle)
	if !ok || h.transcriptionID == "" || h.deleted {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, perr := a.call(ctx, h, http.MethodDelete, "/api/transcription/"+url.PathEscape(h.transcriptionID), "", nil, false, nil)
	if perr != nil && perr.Code != "http_404" {
		return perr
	}
	h.deleted = true
	return nil
}

// call sends one request and decodes a 2xx JSON reply into out (nil: the
// body is discarded). transcribe marks step 2, whose 422 may name an
// unavailable model. It returns the HTTP status (0 without a reply).
func (a *Adapter) call(ctx context.Context, h *handle, method, path, contentType string, body io.Reader,
	transcribe bool, out any) (int, *providers.Error) {
	hr, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.profile.BaseURL, "/")+path, body)
	if err != nil {
		return 0, &providers.Error{Class: providers.PermanentConfig, Code: "bad_request_url"}
	}
	if contentType != "" {
		hr.Header.Set("Content-Type", contentType)
	}
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("X-Request-ID", h.idempotencyKey)
	if tok := a.profile.Token(); tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return 0, contextError(ctx)
		}
		return 0, &providers.Error{Class: providers.Retryable, Code: providers.TransportCode(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The error body only selects a code; it is never logged or stored
		// (§1.4, privacy.md §6).
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		if transcribe && resp.StatusCode == http.StatusUnprocessableEntity && modelUnavailable(b) {
			return resp.StatusCode, &providers.Error{Class: providers.PermanentConfig, Code: "model_unavailable"}
		}
		return resp.StatusCode, providers.AsrStatusError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch {
	case ctx.Err() != nil:
		return resp.StatusCode, contextError(ctx)
	case err != nil:
		return resp.StatusCode, &providers.Error{Class: providers.Retryable, Code: "connection_lost"}
	case len(b) > maxResponseBytes:
		return resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "response_too_large"}
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return resp.StatusCode, &providers.Error{Class: providers.InvalidResponse, Code: "not_json_object"}
	}
	return resp.StatusCode, nil
}

// multipartForm streams the upload form of step 1 through a pipe, copying
// the audio from its reader without touching local disk.
func multipartForm(req providers.AsrRequest, audio io.ReadCloser) (io.ReadCloser, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = audio.Close() }()
		pw.CloseWithError(writeForm(mw, req, audio))
	}()
	return pr, mw.FormDataContentType()
}

func writeForm(mw *multipart.Writer, req providers.AsrRequest, audio io.Reader) error {
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

// modelUnavailable reports whether a 422 of step 2 names an unavailable
// model: transkribierBAR answers with a string detail then, a validation
// error has a list (router_helpers.py:7-13 @ f3f38f3).
func modelUnavailable(body []byte) bool {
	var e struct {
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	d, ok := e.Detail.(string)
	return ok && strings.Contains(d, "is not available on this instance")
}

// failedError maps the error_code of a failed transcription (§2a.3).
func failedError(code string) *providers.Error {
	switch code {
	case "asr_failed":
		return &providers.Error{Class: providers.PermanentInput, Code: "asr_failed"}
	case "asr_unreachable", "timeout", "server_restarted":
		return &providers.Error{Class: providers.Retryable, Code: code}
	}
	// Any other code, also an empty one, is retryable; only allow-listed
	// codes leave the adapter.
	return &providers.Error{Class: providers.Retryable, Code: "remote_failed"}
}

// contextError maps an ended context: the attempt timeout, or the caller
// cancelled (shutdown, job cancelled).
func contextError(ctx context.Context) *providers.Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The transcription may still run remotely.
		return &providers.Error{Class: providers.Retryable, Code: "timeout", RetryAfter: providers.AsrTimeoutRetry}
	}
	return &providers.Error{Class: providers.Retryable, Code: "cancelled"}
}

// parse maps a transcription reply to an AsrResult (§2a.2). model is the
// profile's.
func parse(body []byte, model string) (*providers.AsrResult, error) {
	invalid := func(code string) error {
		return &providers.Error{Class: providers.InvalidResponse, Code: code}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, invalid("not_json_object")
	}
	raw := bytes.TrimSpace(top["utterances"])
	var utts []map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &utts) != nil {
		return nil, invalid("bad_utterances")
	}

	r := &providers.AsrResult{Model: model, Segments: []providers.Segment{}}
	var lang string
	if json.Unmarshal(top["language"], &lang) == nil && languageRE.MatchString(lang) {
		r.Language = &lang
	}
	for _, u := range utts {
		start, ok1 := millis(u["start"])
		end, ok2 := millis(u["end"])
		if !ok1 || !ok2 {
			return nil, invalid("bad_utterance_time")
		}
		var text string
		if json.Unmarshal(u["text"], &text) != nil {
			return nil, invalid("bad_utterance_text")
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		r.Segments = append(r.Segments, providers.Segment{StartMS: start, EndMS: max(end, start), Text: text})
	}
	return r, nil
}

// millis reads a JSON integer of ms. Anything but a non-negative integer
// (missing, null, string, fraction, exponent) is invalid.
func millis(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
