package deliver

import (
	"errors"
	"io"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/sink"
)

const frontendProfile = "frontend-main"

// newFrontendEnv is newEnv with the frontend profile (sinks.md §4a) on the
// env's filesystem store.
func newFrontendEnv(t *testing.T) (*env, blob.Results) {
	t.Helper()
	e := newEnv(t, 0, 0)
	results := e.deps.Blobs.(blob.Results)
	e.deps.Sinks[frontendProfile] = Sink{Sender: sink.Frontend{Results: results}, Idempotent: true}
	return e, results
}

func (e *env) seedFrontend(t *testing.T) string {
	t.Helper()
	jobID := e.seed(t, summaryMD)
	e.exec(t, `UPDATE jobs SET sink_profile = $2 WHERE job_id = $1`, jobID, frontendProfile)
	return jobID
}

func stored(t *testing.T, results blob.Results, jobID string) (string, bool) {
	t.Helper()
	rc, err := results.GetSummary(ctx, jobID)
	if errors.Is(err, blob.ErrNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// deliver to the frontend sink: summary.md under results/<job_id>/, the
// deliveries row sent without document ID written in the completion only,
// nothing sent to Docs; a second run after an interrupted completion
// overwrites (no uncertain case).
func TestFrontendDeliver(t *testing.T) {
	e, results := newFrontendEnv(t)
	jobID := e.seedFrontend(t)

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.OK || string(r.Output) != `{"delivery_state":"sent"}` {
		t.Fatalf("result %+v output %s", r, r.Output)
	}
	if got, ok := stored(t, results, jobID); !ok || got != summaryMD {
		t.Fatalf("stored %q (%v), want the summary", got, ok)
	}
	if row := e.row(t, jobID); row != "" {
		t.Fatalf("row %q before the completion, want none (no sending row)", row)
	}
	// The completion was lost: the next attempt runs again and succeeds.
	e.complete(t, run, queue.Result{})
	run = e.claim(t, jobID)
	r = Handler(e.deps)(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("second run: %+v", r)
	}
	e.complete(t, run, r)
	if row := e.row(t, jobID); row != "sent" {
		t.Fatalf("row %q, want sent without document ID", row)
	}
	if e.docs.requests() != 0 {
		t.Fatalf("%d Docs requests, want 0", e.docs.requests())
	}
}

// A reaped run or a stopping job stores nothing; a §2.4 violation is
// rejected_by_sink with nothing stored.
func TestFrontendGuards(t *testing.T) {
	e, results := newFrontendEnv(t)

	jobID := e.seedFrontend(t)
	run := e.claim(t, jobID)
	e.exec(t, `UPDATE steps SET state = 'pending', owner = NULL, lease_expires_at = NULL
		WHERE job_id = $1 AND name = 'deliver'`, jobID)
	if r := Handler(e.deps)(ctx, run); r.Outcome != queue.Transient || r.ErrorCode != "not_claimed" {
		t.Errorf("not claimed: %+v", r)
	}
	if _, ok := stored(t, results, jobID); ok {
		t.Error("a run that is no longer claimed stored the summary")
	}

	jobID = e.seedFrontend(t)
	run = e.claim(t, jobID)
	e.exec(t, `UPDATE jobs SET purge_requested_at = now() WHERE job_id = $1`, jobID)
	if r := Handler(e.deps)(ctx, run); r.Outcome == queue.OK {
		t.Errorf("purging job: %+v", r)
	}
	if _, ok := stored(t, results, jobID); ok {
		t.Error("a purging job stored the summary")
	}

	jobID = e.seedFrontend(t)
	e.deps.Sinks[frontendProfile] = Sink{Sender: sink.Frontend{Results: results}, Idempotent: true, MaxDocumentBytes: 8}
	run = e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Permanent || r.Reason != ReasonRejectedBySink || r.ErrorCode != "document_too_large" {
		t.Errorf("oversized: %+v", r)
	}
	if _, ok := stored(t, results, jobID); ok {
		t.Error("an oversized summary was stored")
	}
}
