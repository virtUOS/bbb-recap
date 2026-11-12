package summarize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

// seedJob seeds F-01, runs normalize, stores the F-01 render transcript as
// the assemble output and sets the meeting type, as the engine and the
// earlier steps would. segments=false empties the transcript.
func seedJob(t *testing.T, pool *pgxpool.Pool, store blob.Store, segments bool) string {
	t.Helper()
	ctx := context.Background()
	jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
	r := normalize.Handler(normalize.Deps{Pool: pool, Blobs: store, Log: slog.New(slog.DiscardHandler)})(
		ctx, queue.Run{JobID: jobID, Step: "normalize", Attempt: 1})
	if r.Outcome != queue.OK {
		t.Fatalf("normalize: %+v", r)
	}
	tr := readFile(t, renderDir, "F-01", "transcript.json")
	if !segments {
		var doc assemble.Transcript
		if err := json.Unmarshal(tr, &doc); err != nil {
			t.Fatal(err)
		}
		doc.Segments = []assemble.Segment{}
		var err error
		if tr, err = assemble.Marshal(&doc); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(tr)
	key := hex.EncodeToString(sum[:])
	if err := store.Put(ctx, jobID, key, int64(len(tr)), bytes.NewReader(tr)); err != nil {
		t.Fatal(err)
	}
	aout, _ := json.Marshal(assemble.Output{TranscriptSHA256: key})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'normalize'`, []any{jobID, r.Output}},
		{`UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'assemble'`, []any{jobID, aout}},
		{`UPDATE jobs SET meeting_type = 'meeting-summary' WHERE job_id = $1`, []any{jobID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return jobID
}

func handlerDeps(e *env, pool *pgxpool.Pool, store blob.Store) Deps {
	return Deps{Pool: pool, Blobs: store, Log: e.log, LLM: e.llm, Profile: e.profile, FormatsDir: formatsDir,
		MeetingTypes: map[string]string{"meeting-summary": "meeting-summary"}, DefaultLanguage: "en"}
}

// The handler on F-01 stores the summary under its own hash with
// provenance; a repeat gives the same key. Requests carry the package's
// inputs but no BBB user IDs, owner identity or per-user poll answers. The
// log (DEBUG) has no content.
func TestHandlerF01(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID := seedJob(t, pool, store, true)
	e := newEnv(t, 32768, 4096)
	h := Handler(handlerDeps(e, pool, store))
	run := queue.Run{JobID: jobID, Step: Step, Attempt: 1}
	r := h(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("result %+v", r)
	}
	if again := h(ctx, run); string(again.Output) != string(r.Output) {
		t.Errorf("repeat output %s, first %s", again.Output, r.Output)
	}
	var out Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.SummarySHA256) != 64 || out.NoSpeech || out.Profile != "vllm-lab" || out.Dialect != "openai-chat" ||
		out.Model != "example-model-reported" || out.Language != "en" || out.FormatID != "meeting-summary" ||
		out.FormatVersion != "1.0.0" || len(out.FormatSHA256) != 64 {
		t.Errorf("output %+v", out)
	}
	rc, err := store.Get(ctx, jobID, out.SummarySHA256)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil || s.Overview == "" {
		t.Errorf("summary blob %s: %v", b, err)
	}

	calls := e.fake.all()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want one per run", len(calls))
	}
	// Inputs from the package: transcript, chat, notes (notes.html as
	// text), poll aggregates, meeting name.
	for _, want := range []string{"Ada Testperson: Welcome", "Link to the widget spec will follow.", pollText,
		"Fixture Widget Meeting", "votes"} {
		if !strings.Contains(calls[0].user, want) {
			t.Errorf("request lacks %q", want)
		}
	}
	if !strings.Contains(calls[0].user, "<<<BEGIN SHARED NOTES>>>") || strings.Contains(calls[0].user, "<p>") {
		t.Errorf("shared notes missing or not plain text")
	}
	checkPrivacy(t, calls)
	checkLog(t, e.logs)
}

func TestHandlerNoSpeechAndHold(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID := seedJob(t, pool, store, false)
	e := newEnv(t, 32768, 4096)
	d := handlerDeps(e, pool, store)
	run := queue.Run{JobID: jobID, Step: Step, Attempt: 1}
	r := Handler(d)(ctx, run)
	var out Output
	if r.Outcome != queue.OK || json.Unmarshal(r.Output, &out) != nil || !out.NoSpeech || out.SummarySHA256 != "" {
		t.Errorf("no speech: result %+v", r)
	}
	if n := len(e.fake.all()); n != 0 {
		t.Errorf("%d LLM calls without speech", n)
	}

	// The meeting type has no format (meeting-types file or directory).
	for name, edit := range map[string]func(*Deps){
		"type not mapped":   func(d *Deps) { d.MeetingTypes = map[string]string{} },
		"format dir absent": func(d *Deps) { d.MeetingTypes = map[string]string{"meeting-summary": "no-such-format"} },
	} {
		d := handlerDeps(e, pool, store)
		edit(&d)
		if r := Handler(d)(ctx, run); r.Outcome != queue.Hold || r.Reason != ReasonUnknownType {
			t.Errorf("%s: result %+v, want hold unknown_type", name, r)
		}
	}
	// A broken format is a configuration error.
	d = handlerDeps(e, pool, store)
	d.FormatsDir = t.TempDir()
	if err := writeBrokenFormat(d.FormatsDir); err != nil {
		t.Fatal(err)
	}
	if r := Handler(d)(ctx, run); r.Outcome != queue.Permanent || r.Reason != ReasonConfigError {
		t.Errorf("broken format: result %+v", r)
	}
}

// writeBrokenFormat writes a meeting-summary format whose metadata is wrong.
func writeBrokenFormat(dir string) error {
	if err := os.Mkdir(filepath.Join(dir, "meeting-summary"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meeting-summary", "format.json"), []byte(`{"id": "other"}`), 0o644)
}

// The media type, not the path, decides whether the notes are converted
// from HTML.
func TestPlainNotes(t *testing.T) {
	const html = "<p>Decision: <b>blue</b> &amp; green</p>"
	for _, c := range []struct {
		ref  meeting.FileRef
		want string
	}{
		{meeting.FileRef{Path: "shared/minutes.htm", MediaType: "text/html"}, "Decision: blue & green"},
		{meeting.FileRef{Path: "notes/notes.html", MediaType: "text/markdown"}, html},
		{meeting.FileRef{Path: "notes/notes.md", MediaType: "text/markdown"}, html},
	} {
		if got := strings.TrimSpace(plainNotes(c.ref.MediaType, []byte(html))); got != c.want {
			t.Errorf("%s (%s): %q, want %q", c.ref.Path, c.ref.MediaType, got, c.want)
		}
	}
}
