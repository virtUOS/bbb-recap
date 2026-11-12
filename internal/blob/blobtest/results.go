package blobtest

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// resultsJob and otherJob are the job IDs of the results suite.
const (
	resultsJob = "019a2f8c-3b40-7000-8000-000000000021"
	otherJob   = "019a2f8c-3b40-7000-8000-000000000022"
)

// RunResults is the conformance suite for blob.Results (architecture.md §8,
// sinks.md §4a): the summary lives at results/<job_id>/summary.md, is replaced
// by a second write, is outside the job prefix (DeletePrefix and ListJobs do
// not touch it) and goes with DeleteResults; IDs are checked. newStore returns
// an empty store that implements both interfaces; it is called once per
// subtest.
func RunResults(t *testing.T, newStore func(t *testing.T) blob.Store) {
	results := func(t *testing.T) (blob.Store, blob.Results) {
		t.Helper()
		s := newStore(t)
		r, ok := s.(blob.Results)
		if !ok {
			t.Fatalf("%T does not implement blob.Results", s)
		}
		return s, r
	}
	t.Run("SummaryRoundTripAndReplace", func(t *testing.T) {
		_, r := results(t)
		if _, err := readSummary(t, r, resultsJob); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("missing summary: %v, want ErrNotFound", err)
		}
		for _, md := range []string{"# First\n", "# Second, longer\n", ""} {
			if err := r.PutSummary(context.Background(), resultsJob, []byte(md)); err != nil {
				t.Fatal(err)
			}
			if got, err := readSummary(t, r, resultsJob); err != nil || got != md {
				t.Fatalf("summary %q, %v; want %q", got, err, md)
			}
		}
	})
	t.Run("OutsideJobPrefix", func(t *testing.T) {
		s, r := results(t)
		ctx := context.Background()
		if err := r.PutSummary(ctx, resultsJob, []byte("# Summary\n")); err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePrefix(ctx, resultsJob); err != nil {
			t.Fatal(err)
		}
		if _, err := readSummary(t, r, resultsJob); err != nil {
			t.Fatalf("DeletePrefix removed the summary: %v", err)
		}
		if jobs, err := s.ListJobs(ctx); err != nil || len(jobs) != 0 {
			t.Fatalf("ListJobs lists results: %v, %v", jobs, err)
		}
	})
	t.Run("ListAndDelete", func(t *testing.T) {
		_, r := results(t)
		ctx := context.Background()
		if list, err := r.ListResults(ctx); err != nil || len(list) != 0 {
			t.Fatalf("empty store lists %v, %v", list, err)
		}
		for _, id := range []string{otherJob, resultsJob} {
			if err := r.PutSummary(ctx, id, []byte("# Summary\n")); err != nil {
				t.Fatal(err)
			}
		}
		list, err := r.ListResults(ctx)
		if err != nil || len(list) != 2 || list[0].ID != resultsJob || list[1].ID != otherJob ||
			slices.ContainsFunc(list, func(j blob.Job) bool { return j.Modified.IsZero() }) {
			t.Fatalf("ListResults = %v, %v; want both jobs sorted, with times", list, err)
		}
		for range 2 { // idempotent
			if err := r.DeleteResults(ctx, resultsJob); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := readSummary(t, r, resultsJob); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("after DeleteResults: %v", err)
		}
		if _, err := readSummary(t, r, otherJob); err != nil {
			t.Fatalf("DeleteResults removed another job's summary: %v", err)
		}
		if list, _ := r.ListResults(ctx); len(list) != 1 || list[0].ID != otherJob {
			t.Fatalf("ListResults after delete = %v", list)
		}
	})
	t.Run("InvalidKey", func(t *testing.T) {
		_, r := results(t)
		ctx := context.Background()
		for _, id := range []string{"../x", "019A2F8C-3B40-7000-8000-000000000021", ""} {
			if err := r.PutSummary(ctx, id, []byte("x")); !errors.Is(err, blob.ErrInvalidKey) {
				t.Errorf("PutSummary(%q): %v", id, err)
			}
			if _, err := r.GetSummary(ctx, id); !errors.Is(err, blob.ErrInvalidKey) {
				t.Errorf("GetSummary(%q): %v", id, err)
			}
			if err := r.DeleteResults(ctx, id); !errors.Is(err, blob.ErrInvalidKey) {
				t.Errorf("DeleteResults(%q): %v", id, err)
			}
		}
	})
}

func readSummary(t *testing.T, r blob.Results, jobID string) (string, error) {
	t.Helper()
	rc, err := r.GetSummary(context.Background(), jobID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	return string(b), err
}
