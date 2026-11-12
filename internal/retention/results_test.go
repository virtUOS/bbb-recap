package retention

import (
	"errors"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

const resultTTL = 7 * 24 * time.Hour

// resultLoop is newLoop with RECAP_RESULT_TTL 7d and the results store.
func resultLoop(t *testing.T) *Loop {
	t.Helper()
	l := newLoop(t)
	l.ResultTTL, l.Results = resultTTL, l.Blobs.(blob.Results)
	return l
}

func summaryKept(t *testing.T, l *Loop, jobID string) bool {
	t.Helper()
	rc, err := l.Results.GetSummary(ctx, jobID)
	if errors.Is(err, blob.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	return true
}

// Result records (S9) go after result_expires_at; the summaries (S10) go
// RECAP_RESULT_TTL after delivery, or at once when their job is gone or
// ended without the outcome delivered. A job still on its way keeps its
// summary (privacy.md §5.3, architecture.md §8).
func TestResults(t *testing.T) {
	l := resultLoop(t)

	result := func(record string, expiresIn time.Duration) {
		exec(t, l.Pool, `
			INSERT INTO registrations (record_id, frontend_id, meeting_id, status, result_expires_at)
			VALUES ($1, 'pilos-fixture', 'fixture-meeting', 'registered', now() + $2 * interval '1 microsecond')`,
			record, expiresIn.Microseconds())
	}
	result("result-expired", -margin)
	result("result-valid", margin)

	// jobWith adds a job in state with delivered_at deliveredAgo ago (0:
	// not delivered) and a summary.
	jobWith := func(state, outcome string, deliveredAgo time.Duration) string {
		id := queue.NewJobID()
		job(t, l, id, pkg(t, l, id, "committed", 0), state, 0)
		exec(t, l.Pool, `UPDATE jobs SET outcome = nullif($2, ''),
			delivered_at = CASE WHEN $3::bigint > 0 THEN now() - $3 * interval '1 microsecond' END WHERE job_id = $1`,
			id, outcome, deliveredAgo.Microseconds())
		if err := l.Results.PutSummary(ctx, id, []byte("# Summary\n")); err != nil {
			t.Fatal(err)
		}
		return id
	}
	young := jobWith("cleaned", "delivered", resultTTL-margin)
	old := jobWith("cleaned", "delivered", resultTTL+margin)
	oldNotCleaned := jobWith("delivered", "", resultTTL+margin)
	purged := jobWith("cleaned", "purged", time.Hour)
	running := jobWith("running", "", 0)
	gone := queue.NewJobID() // erased, or a restore
	if err := l.Results.PutSummary(ctx, gone, []byte("# Summary\n")); err != nil {
		t.Fatal(err)
	}

	c := l.Sweep(ctx)
	if c.Results != 1 || c.Summaries != 4 {
		t.Errorf("counts = %+v, want 1 result record and 4 summaries", c)
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM registrations WHERE record_id = 'result-expired'`); n != 0 {
		t.Error("expired result record kept")
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM registrations WHERE record_id = 'result-valid'`); n != 1 {
		t.Error("valid result record deleted")
	}
	for id, want := range map[string]bool{young: true, running: true, old: false, oldNotCleaned: false,
		purged: false, gone: false} {
		if got := summaryKept(t, l, id); got != want {
			t.Errorf("summary of %s kept = %v, want %v", id, got, want)
		}
	}
	// A second pass finds nothing.
	if c := l.Sweep(ctx); c.Results != 0 || c.Summaries != 0 {
		t.Errorf("second pass = %+v", c)
	}
}

// Without a results store (nil) the sweeper does nothing.
func TestResultsWithoutStore(t *testing.T) {
	l := newLoop(t)
	l.ResultTTL = resultTTL
	if c := l.Sweep(ctx); c.Summaries != 0 {
		t.Errorf("summaries = %d", c.Summaries)
	}
}
