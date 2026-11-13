package render

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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

const (
	formatsDir = "../../../formats"
	renderDir  = "../../../testdata/render"
)

func readFile(t *testing.T, elem ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(elem...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// put stores b as a blob of the job and returns its key.
func put(t *testing.T, store blob.Store, jobID string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if err := store.Put(context.Background(), jobID, key, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return key
}

// seed writes a job with the F-01 render case as the earlier steps would
// leave it: the normalized meeting (name, poll aggregates), the transcript,
// the summary (unless noSpeech) and the step outputs; created_at is the
// golden's generated_at, the sink profile docs-main.
func seed(t *testing.T, pool *pgxpool.Pool, store blob.Store, lang string, noSpeech bool) string {
	t.Helper()
	ctx := context.Background()
	jobID := validatetest.Seed(t, pool, store, validatetest.Package{})
	f, err := format.Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	m := meeting.Meeting{Version: meeting.Version, MeetingName: "Fixture Widget Meeting", Polls: []meeting.Poll{{
		Question: "Blue or green?", Options: []meeting.PollOption{{Text: "Blue", Votes: 2}, {Text: "Green"}},
	}}}
	mb, _ := json.Marshal(m)
	nout, _ := json.Marshal(meeting.Output{NormalizedSHA256: put(t, store, jobID, mb)})
	aout, _ := json.Marshal(assemble.Output{TranscriptSHA256: put(t, store, jobID,
		readFile(t, renderDir, "F-01", "transcript.json"))})
	sout := summarize.Output{Profile: "vllm-lab", Dialect: "openai-chat", Model: "example-model", Language: lang,
		FormatID: f.ID, FormatVersion: f.Version, FormatSHA256: f.SHA256, NoSpeech: noSpeech}
	if !noSpeech {
		sout.SummarySHA256 = put(t, store, jobID, readFile(t, renderDir, "F-01", "summary.json"))
	}
	sb, _ := json.Marshal(sout)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'normalize'`, []any{jobID, nout}},
		{`UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'assemble'`, []any{jobID, aout}},
		{`UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'summarize'`, []any{jobID, sb}},
		{`UPDATE jobs SET meeting_type = 'meeting-summary', sink_profile = 'docs-main',
			created_at = '2025-10-29T10:20:00Z' WHERE job_id = $1`, []any{jobID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return jobID
}

func get(t *testing.T, store blob.Store, jobID, key string) []byte {
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

// newStore returns a fresh database and blob store (one seeded package each).
func newStore(t *testing.T) (*pgxpool.Pool, blob.Store) {
	t.Helper()
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dbtest.New(t), store
}

func deps(pool *pgxpool.Pool, store blob.Store, log *slog.Logger) Deps {
	return Deps{Pool: pool, Blobs: store, Log: log, FormatsDir: formatsDir, Zone: time.UTC,
		MaxDocumentBytes: map[string]int64{"docs-main": 2097152}}
}

// The handler on F-01 blobs gives the golden summary.md and transcript.vtt
// (en and de) and the title; a repeat gives the same keys. The DEBUG log
// has no content.
func TestHandlerF01(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	for _, c := range []struct{ lang, md, vtt string }{
		{"en", "summary.md", "transcript.vtt"},
		{"de", "summary.de.md", "transcript.de.vtt"},
	} {
		t.Run(c.lang, func(t *testing.T) {
			pool, store := newStore(t)
			h := Handler(deps(pool, store, logging.New(&logs, slog.LevelDebug)))
			jobID := seed(t, pool, store, c.lang, false)
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
			if out.FormatID != "meeting-summary" || out.FormatVersion != "1.0.0" || len(out.FormatSHA256) != 64 ||
				out.Language != c.lang {
				t.Errorf("output %+v", out)
			}
			if strings.Contains(string(r.Output), "Fixture") {
				t.Errorf("step output carries content: %s", r.Output)
			}
			if got, want := get(t, store, jobID, out.SummarySHA256), readFile(t, renderDir, "F-01", c.md); !bytes.Equal(got, want) {
				t.Errorf("summary.md differs from %s:\n%s", c.md, got)
			}
			if got, want := get(t, store, jobID, out.TranscriptSHA256), readFile(t, renderDir, "F-01", c.vtt); !bytes.Equal(got, want) {
				t.Errorf("transcript.vtt differs from %s:\n%s", c.vtt, got)
			}
			if got := string(get(t, store, jobID, out.TitleSHA256)); got != "Fixture Widget Meeting 2025-10-29" {
				t.Errorf("title %q", got)
			}
		})
	}
	for _, s := range []string{"Fixture", "Testperson", "widget", "Blue"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains %q", s)
		}
	}
}

// summary.md over the sink profile's max_document_bytes fails permanent
// rejected_by_sink and stores nothing new; no speech renders the notice; a
// missing format holds unknown_type.
func TestHandlerLimitsAndHolds(t *testing.T) {
	ctx := context.Background()
	pool, store := newStore(t)
	log := slog.New(slog.DiscardHandler)
	jobID := seed(t, pool, store, "en", false)
	run := queue.Run{JobID: jobID, Step: Step, Attempt: 1}

	d := deps(pool, store, log)
	d.MaxDocumentBytes["docs-main"] = 1000 // the F-01 golden has 1299 bytes
	if r := Handler(d)(ctx, run); r.Outcome != queue.Permanent || r.Reason != ReasonRejectedBySink {
		t.Errorf("over the limit: result %+v, want permanent rejected_by_sink", r)
	}
	d.MaxDocumentBytes["docs-main"] = 1299
	if r := Handler(d)(ctx, run); r.Outcome != queue.OK {
		t.Errorf("at the limit: result %+v, want ok", r)
	}

	d = deps(pool, store, log)
	d.FormatsDir = t.TempDir()
	if r := Handler(d)(ctx, run); r.Outcome != queue.Hold || r.Reason != ReasonUnknownType {
		t.Errorf("format missing: result %+v, want hold unknown_type", r)
	}

	pool, store = newStore(t)
	quiet := seed(t, pool, store, "en", true)
	r := Handler(deps(pool, store, log))(ctx, queue.Run{JobID: quiet, Step: Step, Attempt: 1})
	var out Output
	if r.Outcome != queue.OK || json.Unmarshal(r.Output, &out) != nil {
		t.Fatalf("no speech: result %+v", r)
	}
	md := string(get(t, store, quiet, out.SummarySHA256))
	if !strings.Contains(md, "No speech was recognized") || strings.Contains(md, "## Overview") {
		t.Errorf("no-speech summary.md:\n%s", md)
	}
}

// An earlier step's stored output that does not decode, or a transcript
// whose origin_utc does not parse, fails permanent internal_error, and the
// database accepts that failure reason (jobs_failure_reason_check).
func TestHandlerInternalError(t *testing.T) {
	ctx := context.Background()
	pool, store := newStore(t)
	jobID := seed(t, pool, store, "en", false)
	run := queue.Run{JobID: jobID, Step: Step, Attempt: 1}
	h := Handler(deps(pool, store, slog.New(slog.DiscardHandler)))
	var tr map[string]any
	if err := json.Unmarshal(readFile(t, renderDir, "F-01", "transcript.json"), &tr); err != nil {
		t.Fatal(err)
	}
	tr["time_base"].(map[string]any)["origin_utc"] = "not a time"
	bad, _ := json.Marshal(tr)
	for name, blob := range map[string][]byte{"origin_utc": bad, "not JSON": []byte("{")} {
		aout, _ := json.Marshal(assemble.Output{TranscriptSHA256: put(t, store, jobID, blob)})
		if _, err := pool.Exec(ctx, `UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'assemble'`,
			jobID, aout); err != nil {
			t.Fatal(err)
		}
		if r := h(ctx, run); r.Outcome != queue.Permanent || r.Reason != ReasonInternalError {
			t.Errorf("%s: result %+v, want permanent internal_error", name, r)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET state = 'failed', failure_reason = $2 WHERE job_id = $1`,
		jobID, ReasonInternalError); err != nil {
		t.Errorf("failure_reason internal_error rejected: %v", err)
	}
}
