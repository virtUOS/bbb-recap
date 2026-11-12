package assemble_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/transcribe"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

// seedF01 seeds F-01, runs normalize and stores its output on the step, as
// the engine would. It returns the job ID and the normalized meeting.
func seedF01(t *testing.T, pool *pgxpool.Pool, store blob.Store) (string, *meeting.Meeting) {
	t.Helper()
	ctx := context.Background()
	jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
	r := normalize.Handler(normalize.Deps{Pool: pool, Blobs: store, Log: slog.New(slog.DiscardHandler)})(
		ctx, queue.Run{JobID: jobID, Step: "normalize", Attempt: 1})
	if r.Outcome != queue.OK {
		t.Fatalf("normalize: %+v", r)
	}
	if _, err := pool.Exec(ctx, `UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'normalize'`,
		jobID, r.Output); err != nil {
		t.Fatal(err)
	}
	var out meeting.Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	var m meeting.Meeting
	if err := json.Unmarshal(readBlob(t, store, jobID, out.NormalizedSHA256), &m); err != nil {
		t.Fatal(err)
	}
	return jobID, &m
}

func readBlob(t *testing.T, store blob.Store, jobID, key string) []byte {
	t.Helper()
	rc, err := store.Get(context.Background(), jobID, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeItems writes the step_items rows transcribe would leave: the case's
// ASR results as blobs (succeeded) or gaps.
func writeItems(t *testing.T, pool *pgxpool.Pool, store blob.Store, jobID string, m *meeting.Meeting,
	asr []assemble.TrackASR) {
	t.Helper()
	ctx := context.Background()
	byPath := map[string]assemble.TrackASR{}
	for _, a := range asr {
		byPath[a.Path] = a
	}
	for i, tk := range m.Tracks {
		a, ok := byPath[tk.Path]
		if !ok {
			continue
		}
		state, out := "gap", transcribe.ItemOutput{ErrorClass: "permanent_input", ErrorCode: "http_422"}
		var reason any = a.Gap
		if a.Result != nil {
			data, err := json.Marshal(a.Result)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			key := hex.EncodeToString(sum[:])
			if err := store.Put(ctx, jobID, key, int64(len(data)), bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			state, reason = "succeeded", nil
			out = transcribe.ItemOutput{ResultSHA256: key, Profile: a.Profile, Dialect: a.Dialect, Model: a.Result.Model}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO step_items (job_id, item, state, attempt, gap_reason, output)
			VALUES ($1, $2, $3, 1, $4, $5)`, jobID, transcribe.TrackRef(i), state, reason, out); err != nil {
			t.Fatal(err)
		}
	}
}

// The handler on F-01 stores the golden transcript (with this job's ID and
// created_at) under its own hash; a repeat gives the same key. Its log has
// no content.
func TestHandlerF01(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID, m := seedF01(t, pool, store)
	writeItems(t, pool, store, jobID, m, loadF01Case(t).Assemble.ASR)

	var logs bytes.Buffer
	h := assemble.Handler(assemble.Deps{Pool: pool, Blobs: store, Log: logging.New(&logs, slog.LevelDebug)})
	run := queue.Run{JobID: jobID, Step: assemble.Step, Attempt: 1}
	r := h(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("outcome %s %s %s", r.Outcome, r.ErrorClass, r.ErrorCode)
	}
	if again := h(ctx, run); string(again.Output) != string(r.Output) {
		t.Fatalf("repeat output %s, first %s", again.Output, r.Output)
	}
	var out assemble.Output
	if err := json.Unmarshal(r.Output, &out); err != nil || len(out.TranscriptSHA256) != 64 {
		t.Fatalf("output %s: %v", r.Output, err)
	}

	got := decode(t, readBlob(t, store, jobID, out.TranscriptSHA256))
	var created time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM jobs WHERE job_id = $1`, jobID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if got.JobID != jobID || got.CreatedAt != created.UTC().Format(time.RFC3339Nano) {
		t.Errorf("job_id %s created_at %s, want %s %s", got.JobID, got.CreatedAt, jobID, created.UTC())
	}
	want := golden(t, "F-01")
	want.JobID, want.CreatedAt = got.JobID, got.CreatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transcript differs from the golden\n got %+v\nwant %+v", got, want)
	}

	low := strings.ToLower(logs.String())
	for _, bad := range []string{"testperson", "widget", "blue", "w_fix", "fixture meeting"} {
		if strings.Contains(low, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
}

// A transcribe gap row becomes a failed track with a gap; a track without a
// row is a transient error.
func TestHandlerGapsAndIncomplete(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := assemble.Handler(assemble.Deps{Pool: pool, Blobs: store, Log: slog.New(slog.DiscardHandler)})

	jobID, m := seedF01(t, pool, store)
	asr := loadF01Case(t).Assemble.ASR
	writeItems(t, pool, store, jobID, m, asr[:3]) // no row for the fourth track
	r := h(ctx, queue.Run{JobID: jobID, Step: assemble.Step, Attempt: 1})
	if r.Outcome != queue.Transient || r.ErrorCode != "transcribe_incomplete" {
		t.Fatalf("missing row: %+v", r)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO step_items (job_id, item, state, attempt, gap_reason, output)
		VALUES ($1, 'trk-4', 'gap', 5, 'asr_failed', '{"error_class":"retryable","error_code":"timeout"}')`,
		jobID); err != nil {
		t.Fatal(err)
	}
	r = h(ctx, queue.Run{JobID: jobID, Step: assemble.Step, Attempt: 2})
	if r.Outcome != queue.OK {
		t.Fatalf("outcome %+v", r)
	}
	var out assemble.Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	tr := decode(t, readBlob(t, store, jobID, out.TranscriptSHA256))
	if tr.Tracks[3].Status != assemble.StatusFailed || len(tr.Gaps) != 1 || tr.Gaps[0].TrackRef != "trk-4" ||
		tr.Gaps[0].Reason != "asr_failed" || tr.Gaps[0].StartMS != 85300 || tr.Gaps[0].EndMS != 145000 {
		t.Errorf("tracks %+v gaps %+v", tr.Tracks, tr.Gaps)
	}
}
