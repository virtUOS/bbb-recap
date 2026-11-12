package registration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// These tests need TEST_DATABASE_URL (internal/db/dbtest); without it they
// skip. Jobs are created with queue.CreateJob and put into the state a row
// needs with SQL, as the pipeline steps do not exist here.

var bg = context.Background()

// Test credentials, written to a temporary RECAP_CLIENT_SECRETS_DIR.
const (
	tokenFE    = "TEST-NOT-A-SECRET-fe"
	tokenOther = "TEST-NOT-A-SECRET-other"
	tokenAdmin = "TEST-NOT-A-SECRET-admin"
	feID       = "fe-test"
)

// Owner values. No log line and no response may contain them.
var (
	ownerA = [2]string{"test-sub-a-0001", "a.testperson@example.org"}
	ownerB = [2]string{"test-sub-b-0002", "b.testperson@example.org"}
)

type testEnv struct {
	pool *pgxpool.Pool
	api  *API
	mux  *http.ServeMux
	logs *bytes.Buffer
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	for name, tok := range map[string]string{
		"frontend." + feID:    tokenFE,
		"frontend.other-test": tokenOther,
		"admin.test-admin":    tokenAdmin,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	creds, err := httpapi.LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := New(Config{Pool: pool, RegistrationTTL: 14 * 24 * time.Hour, Log: log})
	mux := http.NewServeMux()
	api.Mount(mux, creds)
	e := &testEnv{pool: pool, api: api, mux: mux, logs: logs}
	t.Cleanup(func() { e.checkLogs(t) })
	return e
}

// checkLogs: every line is JSON with time, level and msg, and no owner value
// appears at any level (CT-REG-07, privacy.md §6).
func (e *testEnv) checkLogs(t *testing.T) {
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("log line is not JSON: %s", line)
			continue
		}
		for _, k := range []string{"time", "level", "msg"} {
			if _, ok := m[k]; !ok {
				t.Errorf("log line lacks %q: %s", k, line)
			}
		}
	}
	for _, v := range []string{ownerA[0], ownerA[1], ownerB[0], ownerB[1]} {
		if strings.Contains(e.logs.String(), v) {
			t.Errorf("log contains owner value %q", v)
		}
	}
}

type result struct {
	status int
	header http.Header
	body   []byte
}

func (e *testEnv) do(t *testing.T, method, path, token string, body []byte) result {
	t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	res := result{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
	for _, v := range []string{ownerA[0], ownerA[1], ownerB[0], ownerB[1]} {
		if bytes.Contains(res.body, []byte(v)) {
			t.Fatalf("%s %s: response contains owner value %q: %s", method, path, v, res.body)
		}
	}
	return res
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newRecordID() string { return randomHex(20) + "-1761732000000" }

func body(t *testing.T, recordID, meetingID string, owner [2]string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"schema_version": "1.0.0",
		"meeting_id":     meetingID,
		"record_id":      recordID,
		"owner":          map[string]string{"sub": owner[0], "email": owner[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func path(recordID string) string { return "/api/v1/registrations/" + recordID }

func (e *testEnv) put(t *testing.T, token, recordID string, b []byte) result {
	t.Helper()
	return e.do(t, http.MethodPut, path(recordID), token, b)
}

func (e *testEnv) register(t *testing.T, recordID, meetingID string, owner [2]string) response {
	t.Helper()
	r := e.put(t, tokenFE, recordID, body(t, recordID, meetingID, owner))
	return expectReg(t, r, http.StatusCreated, statusRegistered)
}

func expectStatus(t *testing.T, r result, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("got HTTP %d %s, want %d", r.status, r.body, want)
	}
}

func expectProblem(t *testing.T, r result, status int, slug string) httpapi.Problem {
	t.Helper()
	expectStatus(t, r, status)
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type %q", ct)
	}
	var p httpapi.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != httpapi.ProblemTypePrefix+slug {
		t.Fatalf("problem type %q, want %s", p.Type, slug)
	}
	return p
}

// expectReg checks a registration response: status code, key set, status,
// and expires_at set exactly when registered.
func expectReg(t *testing.T, r result, code int, status string) response {
	t.Helper()
	expectStatus(t, r, code)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema_version", "record_id", "meeting_id", "frontend_id", "status", "created_at",
		"updated_at", "expires_at", "job"}
	if len(keys) != len(want) {
		t.Fatalf("response keys %v, want %v", keys, want)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Fatalf("response lacks %q: %s", k, r.body)
		}
	}
	var resp response
	if err := json.Unmarshal(r.body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != status {
		t.Fatalf("status %q, want %q: %s", resp.Status, status, r.body)
	}
	if (status == statusRegistered) != (resp.ExpiresAt != nil) {
		t.Fatalf("expires_at %v with status %s", resp.ExpiresAt, status)
	}
	return resp
}

// --- database helpers ---

// seedJob creates a committed package for recordID and its job in state
// received, as the ingest commit does.
func seedJob(t *testing.T, pool *pgxpool.Pool, recordID string) string {
	t.Helper()
	id := queue.NewJobID()
	err := pgx.BeginFunc(bg, pool, func(tx pgx.Tx) error {
		pkg := randomHex(32)
		if _, err := tx.Exec(bg, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, 'bbb-test-a', $3, 'meeting-test', 'committed', 0)`, pkg, id, recordID); err != nil {
			return err
		}
		return queue.CreateJob(bg, tx, queue.NewJob{JobID: id, PackageID: pkg, ConfigHash: randomHex(32)})
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(bg, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func packageOf(t *testing.T, pool *pgxpool.Pool, jobID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(bg, `SELECT package_id FROM jobs WHERE job_id = $1`, jobID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// join sets jobs.registration_id, as validate does.
func join(t *testing.T, pool *pgxpool.Pool, jobID, recordID string) {
	t.Helper()
	exec(t, pool, `UPDATE jobs SET registration_id = (SELECT registration_id FROM registrations WHERE record_id = $2)
		WHERE job_id = $1`, jobID, recordID)
}

// atDeliver moves a job to its deliver step, with deliver in stepState.
func atDeliver(t *testing.T, pool *pgxpool.Pool, jobID, jobState, stepState string) {
	t.Helper()
	exec(t, pool, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name NOT IN ('deliver', 'cleanup')`, jobID)
	exec(t, pool, `UPDATE steps SET state = $2 WHERE job_id = $1 AND name = 'deliver'`, jobID, stepState)
	exec(t, pool, `UPDATE jobs SET current_step = 'deliver', state = $2 WHERE job_id = $1`, jobID, jobState)
}

func hold(t *testing.T, pool *pgxpool.Pool, jobID, reason string) {
	t.Helper()
	exec(t, pool, `UPDATE jobs SET state = 'held', held_reason = $2 WHERE job_id = $1`, jobID, reason)
}

type jobRow struct {
	state, step, held string
	cancelRequested   bool
}

func getJob(t *testing.T, pool *pgxpool.Pool, jobID string) jobRow {
	t.Helper()
	var j jobRow
	err := pool.QueryRow(bg, `
		SELECT state, coalesce(current_step, ''), coalesce(held_reason, ''), cancel_requested_at IS NOT NULL
		FROM jobs WHERE job_id = $1`, jobID).Scan(&j.state, &j.step, &j.held, &j.cancelRequested)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

type regRow struct {
	sub, email, status string
	expires, cancelled *time.Time
	created, updated   time.Time
}

func getRow(t *testing.T, pool *pgxpool.Pool, recordID string) regRow {
	t.Helper()
	var r regRow
	err := pool.QueryRow(bg, `
		SELECT coalesce(owner_sub, ''), coalesce(owner_email, ''), status, expires_at, cancelled_at, created_at, updated_at
		FROM registrations WHERE record_id = $1`, recordID).
		Scan(&r.sub, &r.email, &r.status, &r.expires, &r.cancelled, &r.created, &r.updated)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(bg, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- PUT matrix (metadata-interface.md §2.4) ---

func TestPutNewThenGet(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	resp := e.register(t, rid, "meeting-1", ownerA)
	if resp.RecordID != rid || resp.MeetingID != "meeting-1" || resp.FrontendID != feID || resp.Job != nil {
		t.Fatalf("response %+v", resp)
	}
	row := getRow(t, e.pool, rid)
	if row.sub != ownerA[0] || row.email != ownerA[1] || row.status != statusRegistered {
		t.Fatalf("stored row %+v", row)
	}
	// expires_at = time of the PUT + RECAP_REGISTRATION_TTL.
	if d := row.expires.Sub(row.updated); d != 14*24*time.Hour {
		t.Fatalf("expires_at − updated_at = %s, want 336h", d)
	}
	g := e.do(t, http.MethodGet, path(rid), tokenFE, nil)
	expectReg(t, g, http.StatusOK, statusRegistered)
}

func TestPutIdenticalChangesNothing(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	b := body(t, rid, "meeting-1", ownerA)
	first := e.put(t, tokenFE, rid, b)
	expectReg(t, first, http.StatusCreated, statusRegistered)
	before := getRow(t, e.pool, rid)
	time.Sleep(1100 * time.Millisecond) // a change would show in the seconds of updated_at
	again := e.put(t, tokenFE, rid, b)
	expectReg(t, again, http.StatusOK, statusRegistered)
	if !bytes.Equal(first.body, again.body) {
		t.Fatalf("identical PUT changed the response:\n%s\n%s", first.body, again.body)
	}
	if after := getRow(t, e.pool, rid); !after.updated.Equal(before.updated) || !after.expires.Equal(*before.expires) {
		t.Fatalf("identical PUT changed the row: %+v → %+v", before, after)
	}
}

func TestPutOtherOwnerWithoutJob(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	before := getRow(t, e.pool, rid)
	r := e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB))
	expectReg(t, r, http.StatusOK, statusRegistered)
	after := getRow(t, e.pool, rid)
	if after.sub != ownerB[0] || after.email != ownerB[1] {
		t.Fatal("owner not replaced")
	}
	if !after.updated.After(before.updated) || !after.expires.After(*before.expires) || !after.created.Equal(before.created) {
		t.Fatalf("timestamps %+v → %+v", before, after)
	}
}

// Owner correction while deliver is pending resumes an owner_unresolved job in
// the same transaction (architecture.md §5.1).
func TestPutOtherOwnerReleasesOwnerUnresolved(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	atDeliver(t, e.pool, jobID, "running", "pending")
	hold(t, e.pool, jobID, "owner_unresolved")

	r := e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB))
	resp := expectReg(t, r, http.StatusOK, "matched")
	if resp.Job == nil || resp.Job.JobID != jobID || resp.Job.State != "running" || resp.Job.HeldReason != nil {
		t.Fatalf("job in response %+v", resp.Job)
	}
	if j := getJob(t, e.pool, jobID); j.state != "running" || j.held != "" || j.step != "deliver" {
		t.Fatalf("job after PUT %+v, want running at deliver", j)
	}
	if row := getRow(t, e.pool, rid); row.sub != ownerB[0] {
		t.Fatal("owner not replaced")
	}
}

// A late registration resumes a job held no_registration; validate has not
// joined it, so it is found through packages.record_id.
func TestPutReleasesNoRegistration(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	jobID := seedJob(t, e.pool, rid)
	hold(t, e.pool, jobID, "no_registration")
	exec(t, e.pool, `UPDATE jobs SET detail = 'meeting-id-mismatch' WHERE job_id = $1`, jobID)
	other := seedJob(t, e.pool, newRecordID()) // another record: stays held
	hold(t, e.pool, other, "no_registration")
	invalid := seedJob(t, e.pool, rid+"x") // never matches by prefix
	hold(t, e.pool, invalid, "no_registration")

	e.register(t, rid, "meeting-1", ownerA) // not matched yet: validate joins later
	if j := getJob(t, e.pool, jobID); j.state != "running" || j.held != "" || j.step != "validate" {
		t.Fatalf("job after PUT %+v, want running at validate", j)
	}
	if n := countRows(t, e.pool, `SELECT count(*) FROM jobs WHERE job_id = $1 AND detail IS NULL`, jobID); n != 1 {
		t.Fatal("detail not cleared")
	}
	for _, id := range []string{other, invalid} {
		if j := getJob(t, e.pool, id); j.state != "held" {
			t.Fatalf("job of another record released: %+v", j)
		}
	}
}

// A registration PUT racing with validate: validate has locked the job (it
// is running and saw no registration) and commits a no_registration hold
// while the PUT runs. The PUT must wait for the job lock and then release
// the hold, so the job is not left held (architecture.md §5.1, §6.2).
func TestPutWaitsForValidateHold(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	jobID := seedJob(t, e.pool, rid)
	exec(t, e.pool, `UPDATE jobs SET state = 'running' WHERE job_id = $1`, jobID)

	validate, err := e.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = validate.Rollback(bg) }()
	if _, err := validate.Exec(bg, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPut, path(rid), bytes.NewReader(body(t, rid, "meeting-1", ownerA)))
		req.Header.Set("Authorization", "Bearer "+tokenFE)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		done <- rec.Code
	}()

	waitBlockedOrDone(t, e.pool, done)

	if _, err := validate.Exec(bg, `UPDATE jobs SET state = 'held', held_reason = 'no_registration' WHERE job_id = $1`,
		jobID); err != nil {
		t.Fatal(err)
	}
	if err := validate.Commit(bg); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("PUT: HTTP %d", code)
	}
	if j := getJob(t, e.pool, jobID); j.state != "running" || j.held != "" {
		t.Fatalf("job after the race %+v, want released", j)
	}
}

// waitBlockedOrDone waits until a request waits on a row lock, or (without
// the lock it should take) has already finished.
func waitBlockedOrDone(t *testing.T, pool *pgxpool.Pool, done chan int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(bg, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 || len(done) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("request neither blocked nor finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Other held reasons need an admin; a registration PUT leaves them alone.
func TestPutLeavesOtherHolds(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	jobID := seedJob(t, e.pool, rid)
	hold(t, e.pool, jobID, "unknown_type")
	e.register(t, rid, "meeting-1", ownerA)
	if j := getJob(t, e.pool, jobID); j.state != "held" || j.held != "unknown_type" {
		t.Fatalf("job %+v", j)
	}
}

func TestPutOtherOwnerLocked(t *testing.T) {
	for _, c := range []struct {
		name     string
		jobState string
		step     string
		delivery string // deliveries.state, empty for none
	}{
		{"deliver-claimed", "running", "claimed", ""},
		{"delivery-uncertain", "held", "pending", "sending"},
		{"delivered", "delivered", "succeeded", "sent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			rid := newRecordID()
			e.register(t, rid, "meeting-1", ownerA)
			jobID := seedJob(t, e.pool, rid)
			join(t, e.pool, jobID, rid)
			atDeliver(t, e.pool, jobID, c.jobState, c.step)
			if c.jobState == "held" {
				hold(t, e.pool, jobID, "delivery_uncertain")
			}
			if c.delivery != "" {
				exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-test', $2)`,
					jobID, c.delivery)
			}
			expectProblem(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB)), http.StatusConflict, "registration-locked")
			if row := getRow(t, e.pool, rid); row.sub != ownerA[0] {
				t.Fatal("owner replaced although locked")
			}
			// The identical body is still fine.
			expectReg(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerA)), http.StatusOK, "matched")
		})
	}
}

func TestPutOtherMeetingID(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	expectProblem(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-2", ownerA)), http.StatusConflict, "immutable-field")
}

func TestPutCancelledWithoutJob(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNoContent)
	before := getRow(t, e.pool, rid)
	time.Sleep(10 * time.Millisecond)
	expectReg(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB)), http.StatusOK, statusRegistered)
	after := getRow(t, e.pool, rid)
	if after.status != statusRegistered || after.sub != ownerB[0] || after.cancelled != nil || after.expires == nil ||
		!after.created.After(before.created) {
		t.Fatalf("row after re-registration %+v", after)
	}
}

func TestPutCancelledWithJob(t *testing.T) {
	for _, joined := range []bool{true, false} {
		e := newTestEnv(t)
		rid := newRecordID()
		e.register(t, rid, "meeting-1", ownerA)
		expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNoContent)
		jobID := seedJob(t, e.pool, rid) // e.g. a later package that the tombstone discards
		if joined {
			join(t, e.pool, jobID, rid)
		}
		expectProblem(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerA)), http.StatusConflict, "registration-cancelled")
		if row := getRow(t, e.pool, rid); row.status != statusCancelled || row.sub != "" {
			t.Fatalf("tombstone changed: %+v", row)
		}
	}
}

func TestPutOtherFrontend(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	r := e.put(t, tokenOther, rid, body(t, rid, "meeting-1", ownerA))
	expectProblem(t, r, http.StatusConflict, "conflict")
	if bytes.Contains(r.body, []byte("meeting-1")) || bytes.Contains(r.body, []byte(feID)) {
		t.Fatalf("problem reveals details: %s", r.body)
	}
}

func TestPutInvalid(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	valid := body(t, rid, "meeting-1", ownerA)
	edit := func(f func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(valid, &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	for _, c := range []struct {
		name   string
		token  string
		body   []byte
		status int
		slug   string
	}{
		{"no-credential", "", valid, 401, "unauthorized"},
		{"unknown-credential", "TEST-NOT-A-SECRET-unknown", valid, 401, "unauthorized"},
		{"admin-credential", tokenAdmin, valid, 401, "unauthorized"},
		{"not-json", tokenFE, []byte("not json"), 400, "invalid-request"},
		{"json-array", tokenFE, []byte("[]"), 400, "invalid-request"},
		{"too-large", tokenFE, append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), maxBody)...), 413, "payload-too-large"},
		{"major-2", tokenFE, edit(func(m map[string]any) { m["schema_version"] = "2.0.0" }), 422, "unsupported-schema-version"},
		{"major-0", tokenFE, edit(func(m map[string]any) { m["schema_version"] = "0.9.0" }), 422, "unsupported-schema-version"},
		{"version-garbage", tokenFE, edit(func(m map[string]any) { m["schema_version"] = "one" }), 400, "invalid-request"},
		{"extra-field", tokenFE, edit(func(m map[string]any) { m["meeting_type"] = "x" }), 400, "invalid-request"},
		{"bad-email", tokenFE, edit(func(m map[string]any) { m["owner"].(map[string]any)["email"] = "nope" }), 400, "invalid-request"},
		{"path-body-mismatch", tokenFE, edit(func(m map[string]any) { m["record_id"] = newRecordID() }), 400, "invalid-request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := expectProblem(t, e.put(t, c.token, rid, c.body), c.status, c.slug)
			if strings.Contains(p.Detail, "nope") {
				t.Fatalf("detail quotes the body: %q", p.Detail)
			}
		})
	}
	if n := countRows(t, e.pool, `SELECT count(*) FROM registrations`); n != 0 {
		t.Fatalf("%d registrations stored by rejected PUTs", n)
	}
}

// A PUT whose transaction fails leaves neither the registration nor the
// release behind. The failure is injected after the release: a trigger
// rejects the audit row that ReleaseHeld writes.
func TestPutRollback(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	jobID := seedJob(t, e.pool, rid)
	hold(t, e.pool, jobID, "no_registration")
	exec(t, e.pool, `CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	exec(t, e.pool, `CREATE TRIGGER test_fail BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION test_fail()`)

	expectProblem(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerA)), http.StatusInternalServerError, "internal")
	if n := countRows(t, e.pool, `SELECT count(*) FROM registrations WHERE record_id = $1`, rid); n != 0 {
		t.Fatal("registration stored by a rolled-back PUT")
	}
	if j := getJob(t, e.pool, jobID); j.state != "held" || j.held != "no_registration" {
		t.Fatalf("release survived the rollback: %+v", j)
	}
	// Only the error class is logged, never the database's error text.
	if !strings.Contains(e.logs.String(), `"record_id":"`+rid+`"`) ||
		!strings.Contains(e.logs.String(), `"error_class":"sqlstate-P0001"`) ||
		strings.Contains(e.logs.String(), "injected failure") {
		t.Fatalf("failure log: %s", e.logs)
	}
}

func TestGet(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusNotFound, "not-found")
	e.register(t, rid, "meeting-1", ownerA)
	r := e.do(t, http.MethodGet, path(rid), tokenOther, nil)
	expectProblem(t, r, http.StatusNotFound, "not-found")
	if bytes.Contains(r.body, []byte("meeting-1")) {
		t.Fatal("foreign GET reveals the meeting_id")
	}
	expectProblem(t, e.do(t, http.MethodGet, path(rid), "", nil), http.StatusUnauthorized, "unauthorized")

	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	hold(t, e.pool, jobID, "owner_unresolved")
	resp := expectReg(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusOK, "matched")
	if resp.Job == nil || resp.Job.State != "held" || resp.Job.HeldReason == nil || *resp.Job.HeldReason != "owner_unresolved" {
		t.Fatalf("job %+v", resp.Job)
	}
}

// --- DELETE matrix (metadata-interface.md §2.5) ---

func TestDeleteRegistered(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNoContent)
	row := getRow(t, e.pool, rid)
	if row.status != statusCancelled || row.sub != "" || row.email != "" || row.cancelled == nil || row.expires != nil {
		t.Fatalf("tombstone %+v", row)
	}
	expectReg(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusOK, statusCancelled)
	// Already cancelled.
	expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNoContent)
}

func TestDeleteNotFoundAndForeign(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNotFound, "not-found")
	e.register(t, rid, "meeting-1", ownerA)
	expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenOther, nil), http.StatusNotFound, "not-found")
	if row := getRow(t, e.pool, rid); row.status != statusRegistered || row.sub == "" {
		t.Fatal("foreign DELETE changed the registration")
	}
}

// A job before deliver is cancelled in the same transaction: 202, the job
// moves to cleanup (queue.Cancel), the owner is deleted now.
func TestDeleteJobBeforeDeliver(t *testing.T) {
	for _, c := range []struct{ name, state, held string }{
		{"received", "received", ""},
		{"held-owner_unresolved", "held", "owner_unresolved"},
		{"failed", "failed", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			rid := newRecordID()
			e.register(t, rid, "meeting-1", ownerA)
			jobID := seedJob(t, e.pool, rid)
			join(t, e.pool, jobID, rid)
			switch c.state {
			case "held":
				atDeliver(t, e.pool, jobID, "running", "pending")
				hold(t, e.pool, jobID, c.held)
			case "failed":
				exec(t, e.pool, `UPDATE jobs SET state = 'failed', failure_reason = 'retries_exhausted' WHERE job_id = $1`, jobID)
			}
			expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusAccepted)
			j := getJob(t, e.pool, jobID)
			if !j.cancelRequested || j.step != "cleanup" || j.state == "held" || j.state == "failed" {
				t.Fatalf("job after DELETE %+v, want cancel requested and at cleanup", j)
			}
			row := getRow(t, e.pool, rid)
			if row.status != statusCancelled || row.sub != "" || row.email != "" {
				t.Fatalf("registration after DELETE %+v", row)
			}
			resp := expectReg(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusOK, statusCancelled)
			if resp.Job == nil || resp.Job.JobID != jobID {
				t.Fatalf("job %+v", resp.Job)
			}
			expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusNoContent)
		})
	}
}

// A registration DELETE racing with validate: validate has locked the job
// and commits the join (jobs.registration_id) while the DELETE runs. The
// DELETE must wait for the job lock and cancel the joined job (202), not
// tombstone the registration and leave the job running without an owner.
func TestDeleteWaitsForValidateJoin(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, rid)
	exec(t, e.pool, `UPDATE jobs SET state = 'running' WHERE job_id = $1`, jobID)

	validate, err := e.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = validate.Rollback(bg) }()
	if _, err := validate.Exec(bg, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete, path(rid), nil)
		req.Header.Set("Authorization", "Bearer "+tokenFE)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	waitBlockedOrDone(t, e.pool, done)
	if _, err := validate.Exec(bg, `UPDATE jobs SET registration_id = (SELECT registration_id FROM registrations
		WHERE record_id = $2) WHERE job_id = $1`, jobID, rid); err != nil {
		t.Fatal(err)
	}
	if err := validate.Commit(bg); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusAccepted {
		t.Fatalf("DELETE: HTTP %d, want 202", code)
	}
	if j := getJob(t, e.pool, jobID); !j.cancelRequested || j.step != "cleanup" {
		t.Fatalf("job after the race %+v, want cancelled", j)
	}
}

// A job cleaned with outcome purged made no document: it does not block the
// DELETE (§2.5). Alone it gives 204; next to a live job the
// live one is cancelled and the answer is 202.
func TestDeletePurgedJob(t *testing.T) {
	for _, withLive := range []bool{false, true} {
		e := newTestEnv(t)
		rid := newRecordID()
		e.register(t, rid, "meeting-1", ownerA)
		purged := seedJob(t, e.pool, rid)
		join(t, e.pool, purged, rid)
		exec(t, e.pool, `UPDATE jobs SET state = 'cleaned', outcome = 'purged', current_step = 'cleanup' WHERE job_id = $1`, purged)
		want := http.StatusNoContent
		var live string
		if withLive {
			live = queue.NewJobID() // an admin rerun on the same package
			err := pgx.BeginFunc(bg, e.pool, func(tx pgx.Tx) error {
				return queue.CreateJob(bg, tx, queue.NewJob{JobID: live, ConfigHash: randomHex(32),
					PackageID: packageOf(t, e.pool, purged), RerunOf: purged})
			})
			if err != nil {
				t.Fatal(err)
			}
			join(t, e.pool, live, rid)
			want = http.StatusAccepted
		}
		expectStatus(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), want)
		row := getRow(t, e.pool, rid)
		if row.status != statusCancelled || row.sub != "" || row.email != "" || row.cancelled == nil {
			t.Fatalf("withLive=%v: registration %+v, want a tombstone", withLive, row)
		}
		if j := getJob(t, e.pool, purged); j.state != "cleaned" || j.cancelRequested {
			t.Fatalf("withLive=%v: purged job changed: %+v", withLive, j)
		}
		if withLive {
			if j := getJob(t, e.pool, live); !j.cancelRequested || j.step != "cleanup" {
				t.Fatalf("live job not cancelled: %+v", j)
			}
		}
	}
}

// A job cleaned after delivery still answers 409.
func TestDeleteCleanedDelivered(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	atDeliver(t, e.pool, jobID, "cleaned", "succeeded")
	exec(t, e.pool, `UPDATE jobs SET outcome = 'delivered' WHERE job_id = $1`, jobID)
	exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-test', 'sent')`, jobID)
	expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusConflict, "already-delivered")
}

// Unexpected errors on GET and DELETE answer 500 internal and log the class only.
func TestInternalError(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	exec(t, e.pool, `ALTER TABLE jobs RENAME TO jobs_gone`)
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusInternalServerError, "internal")
	expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusInternalServerError, "internal")
	if !strings.Contains(e.logs.String(), `"error_class":"sqlstate-42P01"`) || strings.Contains(e.logs.String(), "jobs_gone") {
		t.Fatalf("failure log: %s", e.logs)
	}
	exec(t, e.pool, `ALTER TABLE jobs_gone RENAME TO jobs`)
}

func TestDeleteAfterDeliverStarted(t *testing.T) {
	for _, c := range []struct {
		name, jobState, step, delivery string
	}{
		{"deliver-claimed", "running", "claimed", ""},
		{"delivery-uncertain", "held", "pending", "sending"},
		{"delivered", "delivered", "succeeded", "sent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			rid := newRecordID()
			e.register(t, rid, "meeting-1", ownerA)
			jobID := seedJob(t, e.pool, rid)
			join(t, e.pool, jobID, rid)
			atDeliver(t, e.pool, jobID, c.jobState, c.step)
			if c.jobState == "held" {
				hold(t, e.pool, jobID, "delivery_uncertain")
			}
			if c.delivery != "" {
				exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-test', $2)`,
					jobID, c.delivery)
			}
			expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusConflict, "already-delivered")
			if row := getRow(t, e.pool, rid); row.status != statusRegistered || row.sub != ownerA[0] {
				t.Fatalf("registration changed by a refused DELETE: %+v", row)
			}
			if j := getJob(t, e.pool, jobID); j.cancelRequested {
				t.Fatal("job cancelled by a refused DELETE")
			}
		})
	}
}

// --- rate limit (§2.6) ---

func TestLimiter(t *testing.T) {
	l := newLimiter(2, time.Minute)
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	for i := range 2 {
		if _, ok := l.allow("a", t0); !ok {
			t.Fatalf("request %d refused", i+1)
		}
	}
	wait, ok := l.allow("a", t0.Add(15*time.Second))
	if ok || wait != 45*time.Second {
		t.Fatalf("third request: ok=%v wait=%s", ok, wait)
	}
	if _, ok := l.allow("b", t0); !ok {
		t.Fatal("another key is limited")
	}
	if _, ok := l.allow("a", t0.Add(time.Minute)); !ok {
		t.Fatal("new window refused")
	}
	if got := retryAfter(1500 * time.Millisecond); got != "2" {
		t.Fatalf("retryAfter = %s", got)
	}
}

func TestRateLimitedResponse(t *testing.T) {
	e := newTestEnv(t)
	e.api.limiter = newLimiter(1, time.Minute)
	rid := newRecordID()
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusNotFound, "not-found")
	r := e.do(t, http.MethodGet, path(rid), tokenFE, nil)
	expectProblem(t, r, http.StatusTooManyRequests, "rate-limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Per front end: the other one is not limited.
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenOther, nil), http.StatusNotFound, "not-found")
}
