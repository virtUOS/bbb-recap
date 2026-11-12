package sink

import (
	"context"
	"errors"
	"time"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// Frontend is the front-end sink (sinks.md §4a, ADR-014): Send keeps the
// summary in the blob store under results/<job_id>/summary.md for the front
// end that registered the meeting. Nothing leaves the service; the owner is
// not used. A second Send replaces the object, so sending again is safe.
type Frontend struct {
	Results blob.Results
}

// Send stores req.SummaryMD and returns an empty document ID. A failure of
// the blob store is NotSent: nothing reached a reader, the step may retry.
func (f Frontend) Send(ctx context.Context, req Request) (string, error) {
	if err := f.Results.PutSummary(ctx, req.JobID, []byte(req.SummaryMD)); err != nil {
		return "", &Error{Class: NotSent, Code: "store_failed"}
	}
	return "", nil
}

// Keep runs one delivery to a sink whose Send is idempotent, the frontend
// sink (§4a). It differs from Deliver in what an earlier attempt means:
// there is no uncertain case, so a record in sending is sent again, and no
// sending record is committed before. A record in sent or
// confirmed_by_admin is returned unchanged without sending. The §2.4 checks
// run first; a violation is Rejected with nothing stored. Success is a sent
// record without a document ID.
func Keep(ctx context.Context, s Sender, req Request, prior *Record, maxBytes int64) (*Record, error) {
	if prior != nil && prior.State != StateSending {
		return prior, nil
	}
	if err := Check(req, maxBytes); err != nil {
		return nil, err
	}
	if _, err := s.Send(ctx, req); err != nil {
		var serr *Error
		if !errors.As(err, &serr) {
			serr = &Error{Class: NotSent, Code: "send_failed"}
		}
		return nil, serr
	}
	at := time.Now().UTC().Format(time.RFC3339)
	return &Record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: StateSent, SentAt: &at}, nil
}
