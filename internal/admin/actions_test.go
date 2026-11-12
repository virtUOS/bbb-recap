package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// env is an admin API with a database and a filesystem blob store.
type env struct {
	pool  *pgxpool.Pool
	mux   *http.ServeMux
	blobs blob.Store
	logs  *bytes.Buffer
	creds *httpapi.Credentials
	api   *API // the Go functions behind the handlers
}

const testRetention = time.Hour

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{pool: dbtest.New(t), logs: &bytes.Buffer{}, mux: http.NewServeMux()}
	dir := t.TempDir()
	for name, tok := range map[string]string{"admin.test-admin": tokenAdmin, "frontend.fe-test": tokenFE} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if e.creds, err = httpapi.LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if e.blobs, err = blob.NewFS(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	e.api = New(Config{Pool: e.pool, Blobs: e.blobs, Results: e.blobs.(blob.Results), ConfigHash: "config-hash-test", RetentionAfterDelivery: testRetention,
		Log: slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	e.api.Mount(e.mux, e.creds)
	return e
}

func (e *env) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) post(jobID, action, body string) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, "/api/v1/admin/jobs/"+jobID+"/"+action, tokenAdmin, body)
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(bg, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Fixture-like owner values that must never appear in an admin answer.
const (
	ownerSub    = "fixture-sub-admin-test"
	ownerEmail  = "owner.admin-test@example.org"
	meetingName = "Fixture Admin Test Meeting"
)

// seed creates a committed package with its blobs (a manifest, whose hash
// is the package_id, and one audio file) and its job, registered and joined.
func (e *env) seed(t *testing.T, server string) string {
	t.Helper()
	id := queue.NewJobID()
	manifest := []byte(`{"meeting_name":"` + meetingName + `","n":"` + randomHex(8) + `"}`)
	audio := []byte("audio-" + randomHex(8))
	pkg, audioSum := sum(manifest), sum(audio)
	for _, b := range [][]byte{manifest, audio} {
		if err := e.blobs.Put(bg, id, sum(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	record := "record-" + randomHex(8)
	err := pgx.BeginFunc(bg, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(bg, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, $3, $4, 'meeting-test-1', 'committed', 0)`, pkg, id, server, record); err != nil {
			return err
		}
		if _, err := tx.Exec(bg, `
			INSERT INTO package_files (package_id, path, role, size, sha256, verified_at)
			VALUES ($1, 'audio/a.opus', 'audio-microphone', $2, $3, now())`, pkg, len(audio), audioSum); err != nil {
			return err
		}
		if err := queue.CreateJob(bg, tx, queue.NewJob{JobID: id, PackageID: pkg, ConfigHash: "old-hash"}); err != nil {
			return err
		}
		_, err := tx.Exec(bg, `
			WITH r AS (
				INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status)
				VALUES ($2, 'fe-test', 'meeting-test-1', $3, $4, 'registered') RETURNING registration_id)
			UPDATE jobs SET registration_id = (SELECT registration_id FROM r), sink_profile = 'docs-test'
			WHERE job_id = $1`, id, record, ownerSub, ownerEmail)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The job states the tests put a job into, by direct writes (the queue
// tests cover how the engine gets there).
var states = []string{"received", "running", "held/no_registration", "held/delivery_uncertain", "failed",
	"delivered", "cleaned"}

func (e *env) setState(t *testing.T, id, state string) {
	t.Helper()
	done := func(upTo string) {
		e.exec(t, `UPDATE steps SET state = 'succeeded', attempt = 1
			WHERE job_id = $1 AND name = ANY($2)`, id, queue.Steps[:indexOf(upTo)])
		e.exec(t, `UPDATE jobs SET current_step = $2 WHERE job_id = $1`, id, upTo)
	}
	switch state {
	case "received":
	case "running":
		done("normalize")
		e.exec(t, `UPDATE jobs SET state = 'running' WHERE job_id = $1`, id)
	case "held/no_registration":
		e.exec(t, `UPDATE jobs SET state = 'held', held_reason = 'no_registration', detail = 'no-registration'
			WHERE job_id = $1`, id)
	case "held/delivery_uncertain":
		done("deliver")
		e.exec(t, `UPDATE jobs SET state = 'held', held_reason = 'delivery_uncertain' WHERE job_id = $1`, id)
		e.exec(t, `INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, 'docs-test', 'sending')`, id)
	case "failed":
		done("normalize")
		e.exec(t, `UPDATE steps SET state = 'failed', attempt = 1 WHERE job_id = $1 AND name = 'normalize'`, id)
		e.exec(t, `UPDATE jobs SET state = 'failed', failure_reason = 'events_mismatch' WHERE job_id = $1`, id)
	case "delivered":
		done("cleanup")
		e.exec(t, `UPDATE steps SET run_after = now() + interval '1 hour' WHERE job_id = $1 AND name = 'cleanup'`, id)
		e.exec(t, `UPDATE jobs SET state = 'delivered', delivered_at = now() WHERE job_id = $1`, id)
		e.exec(t, `INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
			VALUES ($1, 'docs-test', 'sent', 'doc-test-1', now())`, id)
	case "cleaned":
		done("cleanup")
		e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name = 'cleanup'`, id)
		e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'delivered', cleaned_at = now() WHERE job_id = $1`, id)
	default:
		t.Fatalf("unknown state %s", state)
	}
}

func indexOf(step string) int {
	for i, s := range queue.Steps {
		if s == step {
			return i
		}
	}
	panic(step)
}

// snapshot renders everything an action may change about a job.
func (e *env) snapshot(t *testing.T, id string) string {
	t.Helper()
	var s string
	err := e.pool.QueryRow(bg, `
		SELECT concat_ws('|',
			(SELECT row_to_json(j)::text FROM jobs j WHERE job_id = $1),
			(SELECT json_agg(s ORDER BY name)::text FROM steps s WHERE job_id = $1),
			(SELECT row_to_json(d)::text FROM deliveries d WHERE job_id = $1),
			(SELECT count(*)::text FROM audit),
			(SELECT count(*)::text FROM jobs))`, id).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) auditCount(t *testing.T, id, action string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(bg, `SELECT count(*) FROM audit WHERE job_id = $1 AND action = $2`, id, action).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func problemType(rec *httptest.ResponseRecorder) string {
	var p httpapi.Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return strings.TrimPrefix(p.Type, httpapi.ProblemTypePrefix)
}

// Every action in every job state (architecture.md §5.4): allowed → 200
// with the job body and one audit row of the action; else 409
// invalid-job-state and nothing changes.
func TestActionMatrix(t *testing.T) {
	allowed := map[string][]string{
		"retry":   {"held/delivery_uncertain", "failed"},
		"release": {"held/no_registration", "held/delivery_uncertain"},
		"purge":   {"held/no_registration", "held/delivery_uncertain", "failed"},
		"rerun":   {"received", "running", "held/no_registration", "held/delivery_uncertain", "failed", "delivered"},
	}
	e := newEnv(t)
	for action, ok := range allowed {
		for _, state := range states {
			t.Run(action+"/"+state, func(t *testing.T) {
				id := e.seed(t, "bbb-test-a")
				e.setState(t, id, state)
				before := e.snapshot(t, id)
				rec := e.post(id, action, "{}")
				if !slices.Contains(ok, state) {
					if rec.Code != http.StatusConflict || problemType(rec) != "invalid-job-state" {
						t.Fatalf("got %d %s, want 409 invalid-job-state", rec.Code, rec.Body)
					}
					if after := e.snapshot(t, id); after != before {
						t.Fatalf("409 changed the job:\n%s\n%s", before, after)
					}
					return
				}
				m := decode(t, rec)
				if action == "rerun" {
					if m["job_id"] == id || m["state"] != "received" {
						t.Fatalf("rerun answer %v, want the new job", m)
					}
				} else if m["job_id"] != id {
					t.Fatalf("answer job_id %v, want %s", m["job_id"], id)
				}
				if n := e.auditCount(t, id, action); n != 1 {
					t.Fatalf("%d audit rows of %s, want 1", n, action)
				}
				var kind, actor string
				if err := e.pool.QueryRow(bg, `SELECT actor_kind, actor_id FROM audit WHERE job_id = $1 AND action = $2`,
					id, action).Scan(&kind, &actor); err != nil || kind != "admin" || actor != "test-admin" {
					t.Fatalf("audit actor %s %s (%v), want admin test-admin", kind, actor, err)
				}
			})
		}
	}
}

func TestRetryDeliveryUncertain(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "held/delivery_uncertain")
	m := decode(t, e.post(id, "retry", "{}"))
	if m["state"] != "running" || m["held_reason"] != nil || m["delivery"] != nil {
		t.Fatalf("body %v, want running without delivery record", m)
	}
}

func TestReleaseDeliveryUncertain(t *testing.T) {
	for _, body := range []string{"{}", "", `{"document_id": "doc-found-1"}`} {
		t.Run(body, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed(t, "bbb-test-a")
			e.setState(t, id, "held/delivery_uncertain")
			m := decode(t, e.post(id, "release", body))
			d, _ := m["delivery"].(map[string]any)
			wantDoc := any(nil)
			if strings.Contains(body, "doc-found-1") {
				wantDoc = "doc-found-1"
			}
			if m["state"] != "delivered" || d["state"] != "confirmed_by_admin" || d["document_id"] != wantDoc {
				t.Fatalf("body %v, want delivered, confirmed_by_admin, document_id %v", m, wantDoc)
			}
			var due float64
			if err := e.pool.QueryRow(bg, `SELECT extract(epoch FROM run_after - now()) FROM steps
				WHERE job_id = $1 AND name = 'cleanup'`, id).Scan(&due); err != nil {
				t.Fatal(err)
			}
			if d := time.Duration(due * float64(time.Second)); d < testRetention-10*time.Second || d > testRetention {
				t.Errorf("cleanup due in %v, want the retention %v", d, testRetention)
			}
			if strings.Contains(e.logs.String(), "doc-found-1") {
				t.Errorf("the document ID was logged: %s", e.logs)
			}
		})
	}
}

func TestRerunCopiesBlobsAndTakesOverRegistration(t *testing.T) {
	e := newEnv(t)
	old := e.seed(t, "bbb-test-a")
	e.setState(t, old, "failed")
	m := decode(t, e.post(old, "rerun", "{}"))
	next, _ := m["job_id"].(string)
	if !blob.ValidJobID(next) || next == old {
		t.Fatalf("new job_id %q", next)
	}
	var rerunOf, pkg, hash, step string
	var oldReg, newReg *int64
	if err := e.pool.QueryRow(bg, `
		SELECT n.rerun_of::text, n.package_id, n.config_hash, n.current_step, o.registration_id, n.registration_id
		FROM jobs n JOIN jobs o ON o.job_id = n.rerun_of WHERE n.job_id = $1`, next).
		Scan(&rerunOf, &pkg, &hash, &step, &oldReg, &newReg); err != nil {
		t.Fatal(err)
	}
	if rerunOf != old || hash != "config-hash-test" || step != "validate" || oldReg != nil || newReg == nil {
		t.Fatalf("rerun_of %s hash %s step %s registration old %v new %v", rerunOf, hash, step, oldReg, newReg)
	}
	rows, _ := e.pool.Query(bg, `SELECT sha256 FROM package_files WHERE package_id = $1 UNION SELECT $1`, pkg)
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(hashes) != 2 {
		t.Fatalf("hashes %v %v", hashes, err)
	}
	for _, h := range hashes {
		rc, err := e.blobs.Get(bg, next, h)
		if err != nil {
			t.Fatalf("blob %s under the new prefix: %v", h, err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("blob %s: %v", h, err)
		}
		_ = rc.Close()
	}
	o := decode(t, get(e.mux, old, tokenAdmin))
	if o["state"] != "running" {
		t.Fatalf("old job %v, want running (cleanup only)", o)
	}
	var cur string
	var purging bool
	if err := e.pool.QueryRow(bg, `SELECT current_step, purge_requested_at IS NOT NULL FROM jobs WHERE job_id = $1`,
		old).Scan(&cur, &purging); err != nil || cur != "cleanup" || !purging {
		t.Fatalf("old job at %s purging %v (%v)", cur, purging, err)
	}
	if rec := e.post(old, "rerun", "{}"); rec.Code != http.StatusConflict {
		t.Fatalf("second rerun of the old job: %d, want 409", rec.Code)
	}
}

// A rerun whose package blobs are gone answers 409 and leaves no new job.
func TestRerunWithoutContent(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "held/no_registration")
	if err := e.blobs.DeletePrefix(bg, id); err != nil {
		t.Fatal(err)
	}
	before := e.snapshot(t, id)
	if rec := e.post(id, "rerun", "{}"); rec.Code != http.StatusConflict || problemType(rec) != "invalid-job-state" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if after := e.snapshot(t, id); after != before {
		t.Fatal("rerun without content changed the job")
	}
	jobs, err := e.blobs.ListJobs(bg)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("blob prefixes %v (%v), want none left", jobs, err)
	}
}

func TestActionBadRequests(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "held/no_registration")
	for _, c := range []struct {
		name, job, action, token, body, slug string
		status                               int
	}{
		{"unknown-action", id, "delete", tokenAdmin, "{}", "not-found", 404},
		{"unknown-job", queue.NewJobID(), "retry", tokenAdmin, "{}", "not-found", 404},
		{"malformed-job", "nope", "retry", tokenAdmin, "{}", "not-found", 404},
		{"frontend-credential", id, "release", tokenFE, "{}", "unauthorized", 401},
		{"no-credential", id, "release", "", "{}", "unauthorized", 401},
		{"not-json", id, "release", tokenAdmin, "release please", "invalid-request", 400},
		{"unknown-field", id, "release", tokenAdmin, `{"force": true}`, "invalid-request", 400},
		{"empty-document-id", id, "release", tokenAdmin, `{"document_id": ""}`, "invalid-request", 400},
		{"document-id-on-retry", id, "retry", tokenAdmin, `{"document_id": "d"}`, "invalid-request", 400},
		{"document-id-other-hold", id, "release", tokenAdmin, `{"document_id": "d"}`, "invalid-request", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.snapshot(t, id)
			rec := e.do(http.MethodPost, "/api/v1/admin/jobs/"+c.job+"/"+c.action, c.token, c.body)
			if rec.Code != c.status || problemType(rec) != c.slug {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, c.status, c.slug)
			}
			if after := e.snapshot(t, id); after != before {
				t.Fatal("a refused request changed the job")
			}
		})
	}
}

// A rolled-back action leaves neither the change nor the audit row: the
// audit insert fails, so the job update is rolled back with it.
func TestActionRollback(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "failed")
	e.exec(t, `ALTER TABLE audit ADD CONSTRAINT no_retry CHECK (action <> 'retry')`)
	before := e.snapshot(t, id)
	rec := e.post(id, "retry", "{}")
	if rec.Code != http.StatusInternalServerError || problemType(rec) != "internal" {
		t.Fatalf("got %d %s, want 500 internal", rec.Code, rec.Body)
	}
	if after := e.snapshot(t, id); after != before {
		t.Fatalf("rolled-back retry changed the job:\n%s\n%s", before, after)
	}
	if !strings.Contains(e.logs.String(), `"error_class":"sqlstate-23514"`) {
		t.Errorf("log: %s", e.logs)
	}
}

// GET and POST treat the path job_id alike: UUIDs are case-insensitive.
func TestUppercaseJobID(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "failed")
	if m := decode(t, get(e.mux, strings.ToUpper(id), tokenAdmin)); m["job_id"] != id {
		t.Fatalf("GET: job_id %v", m["job_id"])
	}
	if m := decode(t, e.post(strings.ToUpper(id), "retry", "{}")); m["job_id"] != id {
		t.Fatalf("POST: job_id %v", m["job_id"])
	}
}
