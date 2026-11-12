package sink

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// countingSender counts Send calls and fails when err is set.
type countingSender struct {
	calls int
	err   error
}

func (c *countingSender) Send(context.Context, Request) (string, error) {
	c.calls++
	return "ignored", c.err
}

func TestKeep(t *testing.T) {
	ctx := context.Background()
	req := request(read(t, filepath.Join(testdata, "sink", "summary.md")))

	t.Run("sent-without-document-id", func(t *testing.T) {
		s := &countingSender{}
		rec, err := Keep(ctx, s, req, nil, 0)
		if err != nil || s.calls != 1 || rec.State != StateSent || rec.DocumentID != nil || rec.SentAt == nil ||
			!strings.HasSuffix(*rec.SentAt, "Z") {
			t.Fatalf("Keep = %+v, %v (calls %d)", rec, err, s.calls)
		}
	})
	t.Run("sending-is-sent-again", func(t *testing.T) {
		s := &countingSender{}
		prior := &Record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: StateSending}
		if rec, err := Keep(ctx, s, req, prior, 0); err != nil || s.calls != 1 || rec.State != StateSent {
			t.Fatalf("Keep = %+v, %v (calls %d)", rec, err, s.calls)
		}
	})
	t.Run("sent-unchanged", func(t *testing.T) {
		s := &countingSender{}
		prior := &Record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: StateConfirmedByAdmin}
		if rec, err := Keep(ctx, s, req, prior, 0); err != nil || s.calls != 0 || rec != prior {
			t.Fatalf("Keep = %+v, %v (calls %d)", rec, err, s.calls)
		}
	})
	t.Run("rejected-before-send", func(t *testing.T) {
		s := &countingSender{}
		_, err := Keep(ctx, s, req, nil, 10)
		var serr *Error
		if !errors.As(err, &serr) || serr.Class != Rejected || serr.Code != "document_too_large" || s.calls != 0 {
			t.Fatalf("Keep: %v (calls %d)", err, s.calls)
		}
	})
	t.Run("store-failure-not-sent", func(t *testing.T) {
		s := &countingSender{err: errors.New("disk full")}
		_, err := Keep(ctx, s, req, nil, 0)
		var serr *Error
		if !errors.As(err, &serr) || serr.Class != NotSent {
			t.Fatalf("Keep: %v", err)
		}
	})
}

func TestFrontendSend(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := request("# Summary\n")
	if id, err := (Frontend{Results: store}).Send(ctx, req); err != nil || id != "" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	rc, err := store.GetSummary(ctx, req.JobID)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	req.JobID = "not-a-uuid"
	_, err = (Frontend{Results: store}).Send(ctx, req)
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != NotSent || serr.Code != "store_failed" {
		t.Fatalf("Send with a bad job ID: %v", err)
	}
}
