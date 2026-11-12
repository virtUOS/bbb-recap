package providers

import (
	"context"
	"io"
	"time"
)

// ASR is the job-shaped ASR adapter of providers.md §1: Submit, Status,
// Result and Cleanup on one handle. A synchronous dialect does the whole
// request in Submit. Errors of Submit, Status and Result are *Error. The
// adapter never retries; the job queue does (ADR-008).
type ASR interface {
	Capabilities() Capabilities
	// Submit sends one track. It always returns a handle, also with an
	// error, so Status, Result and Cleanup can run on it.
	Submit(ctx context.Context, req AsrRequest) (AsrHandle, error)
	Status(ctx context.Context, h AsrHandle) (AsrStatus, error)
	Result(ctx context.Context, h AsrHandle) (*AsrResult, error)
	// Cleanup is idempotent and may be a no-op.
	Cleanup(ctx context.Context, h AsrHandle) error
}

// AsrHandle is an adapter's handle on one request. Only the adapter that
// returned it looks inside.
type AsrHandle any

// AsrStatus is the state of a handle (providers.md §1).
type AsrStatus string

// Handle states.
const (
	StatusQueued    AsrStatus = "queued"
	StatusRunning   AsrStatus = "running"
	StatusSucceeded AsrStatus = "succeeded"
	StatusFailed    AsrStatus = "failed"
)

// Capabilities are static per dialect (providers.md §1).
type Capabilities struct {
	Async           bool
	WordTimestamps  bool
	WordConfidence  bool
	Diarization     bool   // always requested off (ADR-001)
	MaxFileBytes    *int64 // nil: no known limit
	MediaTypes      []string
	RemoteRetention string // none, ttl, until_cleanup
}

// Audio is the track audio of a request. Open streams it; the adapter never
// writes it to local disk (providers.md §1.2).
type Audio struct {
	Open      func(ctx context.Context) (io.ReadCloser, error)
	Size      int64
	SHA256    string
	MediaType string
}

// AsrRequest is one call for one microphone file (providers.md §1.2).
type AsrRequest struct {
	JobID          string
	TrackRef       string // trk-<n>; also the upload file name
	IdempotencyKey string // <job_id>:<track_ref>:<attempt>, sent as X-Request-ID
	Audio          Audio
	DurationMS     int64  // sets the attempt timeout
	LanguageHint   string // RECAP_ASR_LANGUAGE; empty: the provider detects it
	// AttemptTimeout replaces the timeout of §1.5 when > 0 (recap tool asr only).
	AttemptTimeout time.Duration
}

// AsrResult is the normalized result of one track (providers.md §1.3).
// Times are integer ms from the start of the file.
type AsrResult struct {
	Language *string   `json:"language"`
	Model    string    `json:"model"`
	Segments []Segment `json:"segments"`
}

// Segment is one transcribed segment. Provider speaker labels are dropped.
type Segment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// ErrorClass is an adapter error class (providers.md §1.4, §3.4).
type ErrorClass string

// ASR error classes.
const (
	Retryable       ErrorClass = "retryable"
	PermanentInput  ErrorClass = "permanent_input"
	PermanentConfig ErrorClass = "permanent_config"
	InvalidResponse ErrorClass = "invalid_response"
)

// MaxRetryAfter caps a provider's Retry-After (architecture.md §5.3).
const MaxRetryAfter = 30 * time.Minute

// Error is an adapter error: a class, an allow-listed code and, for
// retryable errors, how long to wait at least. It never carries content
// from the provider's reply.
type Error struct {
	Class      ErrorClass
	Code       string
	RetryAfter time.Duration // 0: none
}

func (e *Error) Error() string { return string(e.Class) + ": " + e.Code }
