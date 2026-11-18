package describe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

func put(t *testing.T, store blob.Store, jobID string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if err := store.Put(context.Background(), jobID, key, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return key
}

// seed makes a job whose normalized meeting has three slides: two show
// image A, one image B; the meeting type is set.
func seed(t *testing.T, pool *pgxpool.Pool, store blob.Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
	a := put(t, store, jobID, []byte("fixture image A"))
	b := put(t, store, jobID, []byte("fixture image B"))
	m := &meeting.Meeting{Version: meeting.Version, Slides: []meeting.Slide{
		{TMS: 1000, Deck: "d-1", Page: 1, Text: "Fixture one", Image: meeting.ImagePrefix + a},
		{TMS: 2000, Deck: "d-1", Page: 2, Text: "", Image: meeting.ImagePrefix + b},
		{TMS: 3000, Deck: "d-1", Page: 3, Text: "Fixture three", Image: meeting.ImagePrefix + a},
	}}
	data, err := meeting.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	nout, _ := json.Marshal(meeting.Output{NormalizedSHA256: put(t, store, jobID, data)})
	if _, err := pool.Exec(ctx, `UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'normalize'`,
		jobID, nout); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET meeting_type = 'lecture' WHERE job_id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID, a, b
}

func deps(t *testing.T, pool *pgxpool.Pool, store blob.Store, llm *fakeLLM, log *bytes.Buffer) Deps {
	d := describer(t, llm, 1)
	return Deps{Pool: pool, Blobs: store, Log: slog.New(slog.NewJSONHandler(log, nil)), LLM: llm, Profile: d.Profile,
		FormatsDir: formatsDir, MeetingTypes: map[string]string{"lecture": "meeting-summary"},
		SlideImageTypes: map[string]bool{"lecture": true}}
}

// commit runs a result's Commit in a transaction, as the engine does.
func commit(t *testing.T, pool *pgxpool.Pool, r queue.Result) {
	t.Helper()
	if r.Commit == nil {
		return
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// One call per distinct image; a retried step skips the images that have
// a row; the descriptions file holds the descriptions only; the log has no
// description text.
func TestHandler(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID, a, b := seed(t, pool, store)
	var logs bytes.Buffer
	llm := &fakeLLM{script: []string{`{"text": "Fixture described A", "visual": ""}`, r}}
	h := Handler(deps(t, pool, store, llm, &logs))

	// Attempt 1: A is described, B fails retryable: the step is transient
	// and keeps A's row.
	res := h(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: 1})
	if res.Outcome != queue.Transient || len(llm.reqs) != 2 {
		t.Fatalf("attempt 1: %+v, %d calls; want transient after 2 calls", res, len(llm.reqs))
	}
	commit(t, pool, res)

	// Attempt 5 (the last): only B is sent; it fails retryable again and
	// falls back.
	llm.script = []string{r}
	res = h(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: queue.MaxAttempts(Step)})
	if res.Outcome != queue.OK || len(llm.reqs) != 3 {
		t.Fatalf("last attempt: %+v, %d calls in all; want ok after one more call", res, len(llm.reqs))
	}
	commit(t, pool, res)
	var out Output
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Images != 2 || out.Described != 1 || out.Fallbacks != 1 || len(out.DescriptionsSHA256) != 64 {
		t.Errorf("output %+v", out)
	}
	rc, err := store.Get(ctx, jobID, out.DescriptionsSHA256)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	var file Descriptions
	if err := json.Unmarshal(raw, &file); err != nil || file.Version != 1 || len(file.Descriptions) != 1 ||
		file.Descriptions[meeting.ImagePrefix+a] != "Fixture described A" {
		t.Errorf("descriptions file %s: %v", raw, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM step_items WHERE job_id = $1 AND item IN ($2, $3)
		AND state = 'succeeded'`, jobID, "img-"+a, "img-"+b).Scan(&n); err != nil || n != 2 {
		t.Errorf("step_items rows %d, %v; want 2", n, err)
	}
	if strings.Contains(logs.String(), "Fixture described") {
		t.Errorf("log contains description text")
	}

	// A repeat sends nothing and gives the same output.
	again := h(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: 1})
	if again.Outcome != queue.OK || len(llm.reqs) != 3 || string(again.Output) != string(res.Output) {
		t.Errorf("repeat: %+v, %d calls", again, len(llm.reqs))
	}
}

// A type without slide images is a no-op: ok, no call, no file.
func TestHandlerImagesOff(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, _ := seed(t, pool, store)
	llm := &fakeLLM{}
	var logs bytes.Buffer
	d := deps(t, pool, store, llm, &logs)
	d.SlideImageTypes = map[string]bool{}
	res := Handler(d)(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: 1})
	if res.Outcome != queue.OK || len(llm.reqs) != 0 || string(res.Output) != `{"images":0,"described":0,"fallbacks":0}` {
		t.Errorf("result %+v (%s), %d calls", res, res.Output, len(llm.reqs))
	}
}
