// Package sink is the provider-agnostic sink interface of
// docs/reference/sinks.md §1: the delivery request and record, the error
// classes, the checks before sending (§2.4) and the protocol that never
// sends twice on its own (§1.4). Vendor adapters (internal/adapters/docs)
// implement Sender with plain net/http.
//
// A request carries content and owner data. Nothing here logs; callers log
// IDs and codes only (privacy.md §6).
package sink

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Owner is the owner identity of the registration (identity-mapping.md §2).
type Owner struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// Request is the delivery request (§1.2). It lives in memory only and is
// never logged or stored in a step output.
type Request struct {
	JobID         string `json:"job_id"`
	SinkProfile   string `json:"sink_profile"`
	Title         string `json:"title"`
	SummaryMD     string `json:"summary_md"`
	SummarySHA256 string `json:"summary_sha256"`
	Owner         Owner  `json:"owner"`
}

// Record states (§1.3).
const (
	StateSending          = "sending"
	StateSent             = "sent"
	StateConfirmedByAdmin = "confirmed_by_admin"
)

// Record is the delivery record (§1.3): the deliveries row. It holds no
// content, title, email or sub.
type Record struct {
	JobID       string  `json:"job_id"`
	SinkProfile string  `json:"sink_profile"`
	State       string  `json:"state"`
	DocumentID  *string `json:"document_id"`
	SentAt      *string `json:"sent_at"` // RFC 3339 UTC
}

// Class is a sink error class (§1.6).
type Class string

// Error classes (§1.6).
const (
	NotSent             Class = "not_sent"
	Uncertain           Class = "uncertain"
	Config              Class = "config"
	RecipientUnresolved Class = "recipient_unresolved"
	Rejected            Class = "rejected"
)

// Error is a classified sink failure. Code is an allow-listed code, never
// a message from the sink.
type Error struct {
	Class      Class
	Code       string
	RetryAfter time.Duration // Retry-After of a 429, for NotSent
}

func (e *Error) Error() string { return fmt.Sprintf("sink: %s (%s)", e.Class, e.Code) }

// Sender sends one request to a sink: at most one HTTP request per call,
// no retries. It returns the sink's document ID, or an *Error.
type Sender interface {
	Send(ctx context.Context, req Request) (documentID string, err error)
}

// ErrRecordExists is what a begin function returns when a delivery record
// exists already: another run got there first, so this one must not send.
var ErrRecordExists = errors.New("sink: delivery record exists")

// Deliver runs one delivery (§1.4, §1.5). prior is the job's delivery
// record before the call (nil: none). begin commits the record in state
// sending before the request (nil: nothing to commit, as in recap tool
// sink); if it fails, nothing is sent and its error is returned as is,
// except ErrRecordExists, which counts as a re-entry.
//
// It returns the record the caller commits after the call and nil, or a
// record (Uncertain only) and an *Error, or nil and another error:
//   - a record in sent or confirmed_by_admin: nothing is sent, the record
//     is returned unchanged;
//   - a record in sending (re-entry): nothing is sent, Uncertain;
//   - a §2.4 violation: Rejected before any request and before begin;
//   - 201: a sent record with the document ID;
//   - Uncertain: the record stays in sending;
//   - any other class: no record (the caller deletes the sending row).
func Deliver(ctx context.Context, s Sender, req Request, prior *Record, maxBytes int64,
	begin func(context.Context) error) (*Record, error) {
	sending := &Record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: StateSending}
	if prior != nil {
		if prior.State == StateSending {
			return sending, &Error{Class: Uncertain, Code: "reentry_sending"}
		}
		return prior, nil
	}
	if err := Check(req, maxBytes); err != nil {
		return nil, err
	}
	if begin != nil {
		if err := begin(ctx); errors.Is(err, ErrRecordExists) {
			return sending, &Error{Class: Uncertain, Code: "reentry_sending"}
		} else if err != nil {
			return nil, err
		}
	}
	id, err := s.Send(ctx, req)
	if err != nil {
		var serr *Error
		if !errors.As(err, &serr) {
			serr = &Error{Class: Uncertain, Code: "send_failed"}
		}
		if serr.Class == Uncertain {
			return sending, serr
		}
		return nil, serr
	}
	at := time.Now().UTC().Format(time.RFC3339)
	return &Record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: StateSent, DocumentID: &id, SentAt: &at}, nil
}
