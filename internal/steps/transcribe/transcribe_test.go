package transcribe_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/adapters/whisperx"
	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/transcribe"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

const (
	whisperxDir = "../../../testdata/providers/whisperx/"
	token       = "FIXTURE-NOT-A-SECRET-transcribe-test"
)

// --- fake whisperx server on 127.0.0.1 --------------------------------------

// fakeASR records requests per job and track and answers with answer.
type fakeASR struct {
	srv *httptest.Server
	ok  []byte // the list-form fixture reply

	mu          sync.Mutex
	calls       map[string]int // "<job_id>:<track_ref>" → requests
	inflight    int
	maxInflight int
	delay       time.Duration
	// answer returns status, headers and body for the n-th request (1-based)
	// of a track. Nil: always the fixture reply.
	answer func(ref string, n int) (int, map[string]string, []byte)
	// hang, when set, makes every request report on it and then wait until
	// the client gives up.
	hang chan string
	stop chan struct{} // closed before the server closes: ends hanging requests
}

func newFake(t *testing.T) *fakeASR {
	t.Helper()
	ok, err := os.ReadFile(whisperxDir + "list-form.spec-derived.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeASR{ok: ok, calls: map[string]int{}, stop: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.stop) }) // runs first

	return f
}

func (f *fakeASR) serve(w http.ResponseWriter, r *http.Request) {
	// Read the stream like a server would, without temp files.
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		_, _ = io.Copy(io.Discard, p)
	}
	job, rest, _ := strings.Cut(r.Header.Get("X-Request-ID"), ":")
	ref, _, _ := strings.Cut(rest, ":")

	f.mu.Lock()
	f.calls[job+":"+ref]++
	n := f.calls[job+":"+ref]
	f.inflight++
	f.maxInflight = max(f.maxInflight, f.inflight)
	answer, delay, hang := f.answer, f.delay, f.hang
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	if hang != nil {
		select {
		case hang <- ref:
		default:
		}
		// A request cancelled during the upload does not always end
		// r.Context(), so the test's end does too.
		select {
		case <-r.Context().Done():
		case <-f.stop:
		}
		return
	}
	time.Sleep(delay)

	status, header, body := http.StatusOK, map[string]string(nil), f.ok
	if answer != nil {
		status, header, body = answer(ref, n)
	}
	for k, v := range header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (f *fakeASR) count(jobID, ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[jobID+":"+ref]
}

// failing answers status for one track (or every track with ref "") and
// the fixture reply otherwise.
func (f *fakeASR) failing(ref string, status int, header map[string]string, body string) {
	f.answer = func(r string, _ int) (int, map[string]string, []byte) {
		if ref == "" || r == ref {
			return status, header, []byte(body)
		}
		return http.StatusOK, nil, f.ok
	}
}

// --- environment -------------------------------------------------------------

// countingStore counts reads per blob hash.
type countingStore struct {
	blob.Store
	mu   sync.Mutex
	gets map[string]int
}

func (s *countingStore) Get(ctx context.Context, jobID, sha string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.gets[sha]++
	s.mu.Unlock()
	return s.Store.Get(ctx, jobID, sha)
}

// recordingASR records the media type of every request it passes on.
type recordingASR struct {
	providers.ASR
	mu         sync.Mutex
	mediaTypes map[string]string // track ref → media type of the last request
}

func (r *recordingASR) Submit(ctx context.Context, req providers.AsrRequest) (providers.AsrHandle, error) {
	r.mu.Lock()
	r.mediaTypes[req.TrackRef] = req.Audio.MediaType
	r.mu.Unlock()
	return r.ASR.Submit(ctx, req)
}

type env struct {
	pool  *pgxpool.Pool
	store *countingStore
	fake  *fakeASR
	asr   *recordingASR
	h     queue.Handler
	logs  *bytes.Buffer
	jobs  int
	// mutate, when set, changes the normalized meeting of the next jobs.
	mutate func(*meeting.Meeting)
}

func newEnv(t *testing.T, maxInflight string) *env {
	t.Helper()
	pool := dbtest.New(t)
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: pool, store: &countingStore{Store: fs, gets: map[string]int{}}, fake: newFake(t),
		logs: &bytes.Buffer{}}

	keyFile := filepath.Join(t.TempDir(), "asr.key")
	if err := os.WriteFile(keyFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := providers.Parse([]byte(`schema_version: "1.0.0"
defaults: {asr: whisperx-test, llm: llm-test}
profiles:
  - {name: whisperx-test, kind: asr, dialect: whisperx-family, base_url: "` + e.fake.srv.URL + `", model: large-v3,
     credentials_file: "` + keyFile + `", max_inflight: ` + maxInflight + `,
     declaration: {data_categories: [voice], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-10-29"}}
  - {name: llm-test, kind: llm, dialect: openai-chat, base_url: "http://llm.recap.test/v1", model: m,
     context_tokens: 100, max_output_tokens: 10,
     declaration: {data_categories: [transcript], operator: o, location: on_prem, retention: r, deletion: manual,
                   used_for_training: "no", contract_ref: none, reviewed_by: r, reviewed_at: "2026-10-29"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(providers.KindASR, "")
	if err != nil {
		t.Fatal(err)
	}
	log := logging.New(e.logs, slog.LevelDebug)
	e.asr = &recordingASR{ASR: whisperx.New(p, log), mediaTypes: map[string]string{}}
	e.h = transcribe.Handler(transcribe.Deps{Pool: pool, Blobs: e.store, Log: log, ASR: e.asr, Profile: p})
	return e
}

// job seeds F-01 and runs normalize. unreadable marks tracks (0-based) as
// gaps the way normalize does for an undecodable file.
func (e *env) job(t *testing.T, unreadable ...int) (string, *meeting.Meeting) {
	t.Helper()
	ctx := context.Background()
	// A fresh record_id per job gives each job its own package.
	m0 := validatetest.F01(t)
	e.jobs++
	m0.RecordID = fmt.Sprintf("%s-%d", m0.RecordID, e.jobs)
	jobID := validatetest.Seed(t, e.pool, e.store, validatetest.Package{Manifest: m0})
	r := normalize.Handler(normalize.Deps{Pool: e.pool, Blobs: e.store, Log: slog.New(slog.DiscardHandler)})(
		ctx, queue.Run{JobID: jobID, Step: "normalize", Attempt: 1})
	if r.Outcome != queue.OK {
		t.Fatalf("normalize: %+v", r)
	}
	var out meeting.Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	rc, err := e.store.Get(ctx, jobID, out.NormalizedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	var m meeting.Meeting
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(unreadable) > 0 || e.mutate != nil {
		for _, i := range unreadable {
			m.Tracks[i].Gap = meeting.GapTrackUnreadable
		}
		if e.mutate != nil {
			e.mutate(&m)
		}
		if data, err = normalize.Marshal(&m); err != nil {
			t.Fatal(err)
		}
		sum := sha256Hex(data)
		if err := e.store.Put(ctx, jobID, sum, int64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		r.Output, _ = json.Marshal(meeting.Output{NormalizedSHA256: sum})
	}
	if _, err := e.pool.Exec(ctx, `UPDATE steps SET output = $2 WHERE job_id = $1 AND name = 'normalize'`,
		jobID, r.Output); err != nil {
		t.Fatal(err)
	}
	e.store.mu.Lock()
	clear(e.store.gets) // count only the reads of transcribe
	e.store.mu.Unlock()
	return jobID, &m
}

// step runs the handler and commits its rows as the engine does
// (architecture.md §6.3).
func (e *env) step(t *testing.T, jobID string, attempt int) queue.Result {
	t.Helper()
	ctx := context.Background()
	r := e.h(ctx, queue.Run{JobID: jobID, Step: transcribe.Step, Attempt: attempt})
	if r.Commit != nil {
		if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return r.Commit(ctx, tx) }); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	return r
}

type itemRow struct {
	State     string
	Attempt   int
	GapReason *string
	Output    transcribe.ItemOutput
}

func (e *env) items(t *testing.T, jobID string) map[string]itemRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT item, state, attempt, gap_reason, output FROM step_items WHERE job_id = $1`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]itemRow{}
	var name string
	var r itemRow
	if _, err := pgx.ForEachRow(rows, []any{&name, &r.State, &r.Attempt, &r.GapReason, &r.Output}, func() error {
		out[name] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func gap(r itemRow) string {
	if r.GapReason == nil {
		return ""
	}
	return *r.GapReason
}

// expectCleanLog checks the captured DEBUG log: no segment text of the
// fixture reply, no path with a BBB user ID, no token (privacy.md §6).
func (e *env) expectCleanLog(t *testing.T) {
	t.Helper()
	log := e.logs.String()
	if !strings.Contains(log, `"level":"DEBUG"`) {
		t.Fatal("log capture has no DEBUG lines")
	}
	for _, bad := range []string{"widget", "propose blue", "Agreed", "Monday", "w_fix", token, "Bearer"} {
		if strings.Contains(log, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
}

func expectOutcome(t *testing.T, r queue.Result, outcome queue.Outcome, reason, class string) {
	t.Helper()
	if r.Outcome != outcome || r.Reason != reason || r.ErrorClass != class {
		t.Fatalf("result = %s reason %q class %q code %q, want %s reason %q class %q",
			r.Outcome, r.Reason, r.ErrorClass, r.ErrorCode, outcome, reason, class)
	}
}

var refs = []string{"trk-1", "trk-2", "trk-3", "trk-4"}

// --- tests ---------------------------------------------------------------------

// Every track is sent once, streamed from the blob store; its AsrResult is
// stored as a blob and recorded with provenance; nothing is written to the
// temp directory.
func TestAllTracksTranscribed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	e := newEnv(t, "4")
	jobID, m := e.job(t)
	if len(m.Tracks) != 4 {
		t.Fatalf("F-01 has %d tracks", len(m.Tracks))
	}

	r := e.step(t, jobID, 1)
	expectOutcome(t, r, queue.OK, "", "")
	var out transcribe.Output
	if err := json.Unmarshal(r.Output, &out); err != nil || out != (transcribe.Output{Tracks: 4, Transcribed: 4}) {
		t.Fatalf("output %s", r.Output)
	}
	want, err := os.ReadFile(whisperxDir + "result.json")
	if err != nil {
		t.Fatal(err)
	}
	items := e.items(t, jobID)
	for i, ref := range refs {
		it := items[ref]
		if it.State != "succeeded" || it.Attempt != 1 || it.GapReason != nil ||
			it.Output.Profile != "whisperx-test" || it.Output.Dialect != "whisperx-family" || it.Output.Model != "large-v3" {
			t.Errorf("%s: %+v", ref, it)
		}
		rc, err := e.store.Get(context.Background(), jobID, it.Output.ResultSHA256)
		if err != nil {
			t.Fatalf("%s: result blob: %v", ref, err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !jsonEqual(got, want) {
			t.Errorf("%s: result blob %s", ref, got)
		}
		if n := e.fake.count(jobID, ref); n != 1 {
			t.Errorf("%s: %d requests", ref, n)
		}
		if n := e.store.gets[m.Tracks[i].SHA256]; n != 1 {
			t.Errorf("%s: audio read %d times from the blob store, want 1", ref, n)
		}
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp directory has %d entries; the upload must not touch local disk", len(entries))
	}
	e.expectCleanLog(t)
}

// The request carries the media type of the track in the normalized
// meeting, not a fixed one.
func TestTrackMediaTypeSent(t *testing.T) {
	e := newEnv(t, "4")
	e.mutate = func(m *meeting.Meeting) { m.Tracks[1].MediaType = "audio/ogg" }
	jobID, _ := e.job(t)
	expectOutcome(t, e.step(t, jobID, 1), queue.OK, "", "")
	want := map[string]string{"trk-1": "audio/webm", "trk-2": "audio/ogg", "trk-3": "audio/webm", "trk-4": "audio/webm"}
	e.asr.mu.Lock()
	defer e.asr.mu.Unlock()
	for ref, mt := range want {
		if got := e.asr.mediaTypes[ref]; got != mt {
			t.Errorf("%s: media type %q, want %q", ref, got, mt)
		}
	}
}

// retryable → transient while attempts are left; the retried step sends only
// the failed track (exactly one request per done track).
func TestRetriedStepSkipsDoneTracks(t *testing.T) {
	e := newEnv(t, "4")
	jobID, _ := e.job(t)
	e.fake.answer = func(ref string, n int) (int, map[string]string, []byte) {
		if ref == "trk-2" && n == 1 {
			return http.StatusServiceUnavailable, map[string]string{"Retry-After": "120"}, []byte(`{"detail":"busy"}`)
		}
		return http.StatusOK, nil, e.fake.ok
	}
	r := e.step(t, jobID, 1)
	expectOutcome(t, r, queue.Transient, "", "retryable")
	if r.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter %v, want 120s", r.RetryAfter)
	}
	if it := e.items(t, jobID)["trk-2"]; it.State != "pending" || it.Attempt != 1 || it.Output.ErrorCode != "http_503" {
		t.Errorf("trk-2 after attempt 1: %+v", it)
	}

	r = e.step(t, jobID, 2)
	expectOutcome(t, r, queue.OK, "", "")
	for _, ref := range refs {
		want := 1
		if ref == "trk-2" {
			want = 2
		}
		if n := e.fake.count(jobID, ref); n != want {
			t.Errorf("%s: %d requests, want %d", ref, n, want)
		}
	}
	if it := e.items(t, jobID)["trk-2"]; it.State != "succeeded" || it.Attempt != 2 {
		t.Errorf("trk-2 after attempt 2: %+v", it)
	}
	e.expectCleanLog(t)
}

// A track that fails on every attempt becomes a gap asr_failed on the
// step's last attempt; the step ends ok, never retries_exhausted.
func TestTrackExhaustsAttempts(t *testing.T) {
	e := newEnv(t, "4")
	jobID, _ := e.job(t)
	e.fake.failing("trk-1", http.StatusBadGateway, nil, `{"detail":"down"}`)
	last := queue.MaxAttempts(transcribe.Step)
	for a := 1; a < last; a++ {
		expectOutcome(t, e.step(t, jobID, a), queue.Transient, "", "retryable")
	}
	r := e.step(t, jobID, last)
	expectOutcome(t, r, queue.OK, "", "")
	if string(r.Output) != `{"tracks":4,"transcribed":3,"gaps":1}` {
		t.Errorf("output %s", r.Output)
	}
	it := e.items(t, jobID)["trk-1"]
	if it.State != "gap" || gap(it) != transcribe.GapASRFailed || it.Attempt != last {
		t.Errorf("trk-1: %+v", it)
	}
	if n := e.fake.count(jobID, "trk-1"); n != last {
		t.Errorf("trk-1: %d requests, want %d", n, last)
	}
	if n := e.fake.count(jobID, "trk-3"); n != 1 {
		t.Errorf("trk-3: %d requests, want 1", n)
	}
}

// The README adapter table for ASR, one row per case.
func TestAdapterTable(t *testing.T) {
	last := queue.MaxAttempts(transcribe.Step)
	for _, c := range []struct {
		name    string
		status  int
		body    string
		attempt int
		outcome queue.Outcome
		reason  string
		class   string
		state   string // of trk-1
		gap     string
	}{
		{"retryable", 503, `{}`, 1, queue.Transient, "", "retryable", "pending", ""},
		{"retryable-last", 503, `{}`, last, queue.OK, "", "", "gap", transcribe.GapASRFailed},
		{"invalid_response", 200, `not json`, 1, queue.Transient, "", "invalid_response", "pending", ""},
		{"invalid_response-last", 200, `{"segments":[{"text":"x"}]}`, last, queue.OK, "", "", "gap",
			transcribe.GapASRInvalidResponse},
		{"permanent_input", 422, `{"detail":[]}`, 1, queue.OK, "", "", "gap", transcribe.GapASRRejectedInput},
		{"permanent_config", 401, `{"detail":"no"}`, 1, queue.Permanent, transcribe.ReasonConfigError,
			"permanent_config", "pending", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, "4")
			jobID, _ := e.job(t)
			e.fake.failing("trk-1", c.status, nil, c.body)
			expectOutcome(t, e.step(t, jobID, c.attempt), c.outcome, c.reason, c.class)
			it := e.items(t, jobID)["trk-1"]
			if it.State != c.state || gap(it) != c.gap {
				t.Errorf("trk-1: %+v, want %s %q", it, c.state, c.gap)
			}
		})
	}
}

// When every track is a gap, including one normalize could not read, the
// job fails with all_tracks_failed; the unreadable track is never sent.
func TestAllTracksFailed(t *testing.T) {
	e := newEnv(t, "4")
	jobID, _ := e.job(t, 1)
	e.fake.failing("", http.StatusUnprocessableEntity, nil, `{"detail":"bad audio"}`)
	r := e.step(t, jobID, 1)
	expectOutcome(t, r, queue.Permanent, transcribe.ReasonAllTracksFailed, "")
	items := e.items(t, jobID)
	if _, ok := items["trk-2"]; ok || e.fake.count(jobID, "trk-2") != 0 {
		t.Errorf("unreadable trk-2 was sent or recorded: %+v", items["trk-2"])
	}
	for _, ref := range []string{"trk-1", "trk-3", "trk-4"} {
		if gap(items[ref]) != transcribe.GapASRRejectedInput {
			t.Errorf("%s: %+v", ref, items[ref])
		}
	}
}

// An unreadable track alone does not fail the job (gap, step ok).
func TestUnreadableTrackIsGap(t *testing.T) {
	e := newEnv(t, "4")
	jobID, _ := e.job(t, 2)
	r := e.step(t, jobID, 1)
	expectOutcome(t, r, queue.OK, "", "")
	if string(r.Output) != `{"tracks":4,"transcribed":3,"gaps":1}` || e.fake.count(jobID, "trk-3") != 0 {
		t.Errorf("output %s, trk-3 requests %d", r.Output, e.fake.count(jobID, "trk-3"))
	}
}

// max_inflight 1 holds across jobs: two jobs run at once, but never more
// than one request is in flight (architecture.md §7).
func TestMaxInflightAcrossJobs(t *testing.T) {
	e := newEnv(t, "1")
	job1, _ := e.job(t)
	job2, _ := e.job(t)
	e.fake.delay = 20 * time.Millisecond
	var wg sync.WaitGroup
	results := make([]queue.Result, 2)
	for i, id := range []string{job1, job2} {
		wg.Go(func() { results[i] = e.step(t, id, 1) })
	}
	wg.Wait()
	for _, r := range results {
		expectOutcome(t, r, queue.OK, "", "")
	}
	if e.fake.maxInflight != 1 {
		t.Errorf("max requests in flight = %d, want 1", e.fake.maxInflight)
	}

	// Control: with max_inflight 4 the same run overlaps, so the check above
	// would notice a missing limit.
	e = newEnv(t, "4")
	job1, _ = e.job(t)
	e.fake.delay = 20 * time.Millisecond
	expectOutcome(t, e.step(t, job1, 1), queue.OK, "", "")
	if e.fake.maxInflight < 2 {
		t.Errorf("max_inflight 4: max in flight = %d, want > 1", e.fake.maxInflight)
	}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A shutdown during the last attempt never turns tracks into gaps: the
// handler returns without rows, so nothing is decided.
func TestCancelledLastAttemptMakesNoGaps(t *testing.T) {
	e := newEnv(t, "1") // one request in flight, the other tracks wait on the semaphore
	jobID, _ := e.job(t)
	e.fake.hang = make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-e.fake.hang
		cancel()
	}()
	r := e.h(ctx, queue.Run{JobID: jobID, Step: transcribe.Step, Attempt: queue.MaxAttempts(transcribe.Step)})
	expectOutcome(t, r, queue.Transient, "", "internal")
	if r.Commit != nil {
		t.Error("a cancelled step must not write step_items")
	}
}

// Through the queue engine: SIGTERM while the last attempt waits on the
// provider. The step goes back to retrying with its attempt unchanged and
// no track becomes a gap (architecture.md §9). Takes the engine's shutdown
// drain (about 20 s).
func TestShutdownDuringLastAttempt(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the engine's shutdown drain")
	}
	e := newEnv(t, "1")
	jobID, _ := e.job(t)
	bg := context.Background()
	before := queue.MaxAttempts(transcribe.Step) - 1
	if _, err := e.pool.Exec(bg, `UPDATE jobs SET state = 'running', current_step = 'transcribe' WHERE job_id = $1`,
		jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(bg, `UPDATE steps SET state = 'retrying', attempt = $2, run_after = now()
		WHERE job_id = $1 AND name = 'transcribe'`, jobID, before); err != nil {
		t.Fatal(err)
	}
	e.fake.hang = make(chan string, 1)
	ctx, cancel := context.WithCancel(bg)
	go func() {
		<-e.fake.hang
		cancel()
	}()
	queue.New(e.pool, queue.Config{Owner: "test-owner", Slots: 1, Log: slog.New(slog.DiscardHandler),
		Handlers: map[string]queue.Handler{transcribe.Step: e.h}, Drain: 100 * time.Millisecond}).Run(ctx)

	var state, jobState string
	var attempt int
	if err := e.pool.QueryRow(bg, `SELECT s.state, s.attempt, j.state FROM steps s JOIN jobs j USING (job_id)
		WHERE s.job_id = $1 AND s.name = 'transcribe'`, jobID).Scan(&state, &attempt, &jobState); err != nil {
		t.Fatal(err)
	}
	if state != "retrying" || attempt != before || jobState != "running" {
		t.Errorf("after shutdown: step %s attempt %d, job %s; want retrying, %d, running", state, attempt, jobState, before)
	}
	if items := e.items(t, jobID); len(items) != 0 {
		t.Errorf("step_items written on shutdown: %+v", items)
	}
}
