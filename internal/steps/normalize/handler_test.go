package normalize_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
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

// A job whose meeting type switches slide images on gets them as job blobs
// (templates.md §3.2); another type gets none (ADR-017, D10).
func TestNormalizeHandlerSlideImages(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		meetingType string
		images      int
	}{{"lecture", 2}, {"meeting-summary", 0}} {
		pool := dbtest.New(t)
		store, err := blob.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		h := normalize.Handler(normalize.Deps{Pool: pool, Blobs: store, Log: slog.New(slog.DiscardHandler),
			SlideImageTypes: map[string]bool{"lecture": true}})
		jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
		if _, err := pool.Exec(ctx, `UPDATE jobs SET meeting_type = $2 WHERE job_id = $1`, jobID,
			c.meetingType); err != nil {
			t.Fatal(err)
		}
		r := h(ctx, queue.Run{JobID: jobID, Step: "normalize", Attempt: 1})
		if r.Outcome != queue.OK {
			t.Fatalf("%s: outcome %+v", c.meetingType, r)
		}
		var out meeting.Output
		if err := json.Unmarshal(r.Output, &out); err != nil {
			t.Fatal(err)
		}
		rc, err := store.Get(ctx, jobID, out.NormalizedSHA256)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		m, err := meeting.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		images := 0
		for _, s := range m.Slides {
			key, ok := strings.CutPrefix(s.Image, meeting.ImagePrefix)
			if !ok {
				continue
			}
			images++
			rc, err := store.Get(ctx, jobID, key) // verifies the hash
			if err != nil {
				t.Fatalf("%s: image blob %s: %v", c.meetingType, key, err)
			}
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			if len(b) < 8 || string(b[1:4]) != "PNG" {
				t.Errorf("%s: image blob is not a PNG", c.meetingType)
			}
		}
		if images != c.images {
			t.Errorf("%s: %d slides with an image, want %d", c.meetingType, images, c.images)
		}
	}
}
