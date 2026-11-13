package normalize_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/jcs"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

// The handler stores the normalized meeting under its own hash; a repeat
// gives the same output and the same blob key.
func TestNormalizeHandler(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
	h := normalize.Handler(normalize.Deps{Pool: pool, Blobs: store, Log: slog.New(slog.DiscardHandler)})
	run := queue.Run{JobID: jobID, Step: "normalize", Attempt: 1}

	r := h(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("outcome %s %s %s", r.Outcome, r.ErrorClass, r.Reason)
	}
	again := h(ctx, run)
	if string(again.Output) != string(r.Output) {
		t.Fatalf("repeat output %s, first %s", again.Output, r.Output)
	}
	var out meeting.Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	rc, err := store.Get(ctx, jobID, out.NormalizedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc) // Get verifies the hash
	if err != nil {
		t.Fatal(err)
	}
	if canon, err := jcs.Canonicalize(data); err != nil || string(canon) != string(data) {
		t.Fatalf("blob is not canonical JSON: %v", err)
	}
	var m meeting.Meeting
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 4 || len(m.RecordingPeriods) != 2 || m.Version != meeting.Version {
		t.Fatalf("normalized: %d tracks, %d periods", len(m.Tracks), len(m.RecordingPeriods))
	}
}
