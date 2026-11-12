package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// leftovers counts, in every table of the schema, the rows whose JSON form
// contains one of the values. A table without such rows is left out.
func (e *env) leftovers(t *testing.T, values ...string) map[string]int {
	t.Helper()
	rows, err := e.pool.Query(bg, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 8 {
		t.Fatalf("tables %v (%v)", tables, err)
	}
	found := map[string]int{}
	for _, table := range tables {
		var n int
		q := `SELECT count(*) FROM ` + pgx.Identifier{table}.Sanitize() + ` t
			WHERE EXISTS (SELECT 1 FROM unnest($1::text[]) v WHERE row_to_json(t)::text LIKE '%' || v || '%')`
		if err := e.pool.QueryRow(bg, q, values).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n > 0 {
			found[table] = n
		}
	}
	return found
}

// ids returns the package_id and record_id of a job.
func (e *env) ids(t *testing.T, jobID string) (pkg, record string) {
	t.Helper()
	if err := e.pool.QueryRow(bg, `SELECT p.package_id, p.record_id FROM jobs j JOIN packages p USING (package_id)
		WHERE j.job_id = $1`, jobID).Scan(&pkg, &record); err != nil {
		t.Fatal(err)
	}
	return pkg, record
}

func (e *env) eraseAudit(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(bg, `SELECT count(*) FROM audit WHERE action = 'erase'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// decodeErased checks 200 and the exact key set of the erase answer.
func decodeErased(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "document_id,erased,sink_profile" {
		t.Fatalf("keys %v", keys)
	}
	if m["erased"] != true {
		t.Fatalf("erased %v", m["erased"])
	}
	return m
}

// cleanAfterDelivery puts a job into delivered, then cleaned as cleanup
// leaves it, except for the registration, which erase must remove too.
func (e *env) cleanAfterDelivery(t *testing.T, id string) {
	t.Helper()
	e.setState(t, id, "delivered")
	e.exec(t, `UPDATE steps SET state = 'succeeded', run_after = now() WHERE job_id = $1 AND name = 'cleanup'`, id)
	e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'delivered', cleaned_at = now() WHERE job_id = $1`, id)
	e.exec(t, `INSERT INTO audit (actor_kind, action, job_id, detail) VALUES ('system', 'job_state', $1, 'cleaned')`, id)
	e.exec(t, `INSERT INTO step_items (job_id, item, state) VALUES ($1, 'trk-0', 'succeeded')`, id)
}

// Erase of a cleaned, delivered job leaves no row with its job_id,
// package_id or record_id in any table, writes one audit row erase without
// job_id, answers the document ID and sink profile, and leaves other jobs
// alone. A second erase is 404.
func TestEraseCleanedJob(t *testing.T) {
	e := newEnv(t)
	other := e.seed(t, "bbb-test-b")
	e.setState(t, other, "running")
	otherBefore := e.snapshotRows(t, other)

	id := e.seed(t, "bbb-test-a")
	e.cleanAfterDelivery(t, id)
	pkg, record := e.ids(t, id)
	if got := e.leftovers(t, id, pkg, record); len(got) < 5 {
		t.Fatalf("seed rows %v, want rows in at least 5 tables", got)
	}
	audits := e.eraseAudit(t)

	rec := e.post(id, "erase", "{}")
	m := decodeErased(t, rec)
	if m["document_id"] != "doc-test-1" || m["sink_profile"] != "docs-test" {
		t.Fatalf("answer %v", m)
	}
	if got := e.leftovers(t, id, pkg, record); len(got) != 0 {
		t.Fatalf("rows left after erase: %v", got)
	}
	if n := e.eraseAudit(t); n != audits+1 {
		t.Fatalf("%d erase audit rows, want %d", n, audits+1)
	}
	var kind, actor string
	var jobID, detail *string
	if err := e.pool.QueryRow(bg, `SELECT actor_kind, actor_id, job_id::text, detail FROM audit
		WHERE action = 'erase' ORDER BY audit_id DESC LIMIT 1`).Scan(&kind, &actor, &jobID, &detail); err != nil {
		t.Fatal(err)
	}
	if kind != "admin" || actor != "test-admin" || jobID != nil || detail != nil {
		t.Fatalf("audit %s %s %v %v, want admin test-admin, job_id and detail NULL", kind, actor, jobID, detail)
	}
	jobs, err := e.blobs.ListJobs(bg)
	if err != nil || slices.ContainsFunc(jobs, func(j blob.Job) bool { return j.ID == id }) {
		t.Fatalf("blob prefix of the erased job still listed (%v)", err)
	}
	if after := e.snapshotRows(t, other); after != otherBefore {
		t.Fatalf("erase changed another job:\n%s\n%s", otherBefore, after)
	}
	if logs := e.logs.String(); strings.Contains(logs, id) || strings.Contains(logs, "doc-test-1") {
		t.Fatalf("the job_id or document ID was logged: %s", logs)
	}

	if rec := e.post(id, "erase", "{}"); rec.Code != http.StatusNotFound || problemType(rec) != "not-found" {
		t.Fatalf("second erase: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := get(e.mux, id, tokenAdmin); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after erase: %d, want 404", rec.Code)
	}
	if n := e.eraseAudit(t); n != audits+1 {
		t.Fatalf("the second erase wrote an audit row")
	}
}

// A cleaned job without a delivery record answers null for both.
func TestEraseWithoutDelivery(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "cleaned")
	rec := e.post(id, "erase", "")
	m := decodeErased(t, rec)
	if m["document_id"] != nil || m["sink_profile"] != nil {
		t.Fatalf("answer %v, want nulls", m)
	}
}

// Every state but cleaned is 409 invalid-job-state and changes nothing.
func TestEraseWrongState(t *testing.T) {
	e := newEnv(t)
	for _, state := range states {
		if state == "cleaned" {
			continue
		}
		t.Run(state, func(t *testing.T) {
			id := e.seed(t, "bbb-test-a")
			e.setState(t, id, state)
			before := e.snapshotRows(t, id)
			rec := e.post(id, "erase", "{}")
			if rec.Code != http.StatusConflict || problemType(rec) != "invalid-job-state" {
				t.Fatalf("got %d %s, want 409 invalid-job-state", rec.Code, rec.Body)
			}
			if after := e.snapshotRows(t, id); after != before {
				t.Fatalf("409 changed the job:\n%s\n%s", before, after)
			}
		})
	}
}

func TestEraseBadRequests(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "cleaned")
	for _, c := range []struct {
		name, job, token, body, slug string
		status                       int
	}{
		{"unknown-job", "0190f0a0-0000-7000-8000-000000000000", tokenAdmin, "{}", "not-found", 404},
		{"malformed-job", "nope", tokenAdmin, "{}", "not-found", 404},
		{"frontend-credential", id, tokenFE, "{}", "unauthorized", 401},
		{"no-credential", id, "", "{}", "unauthorized", 401},
		{"document-id", id, tokenAdmin, `{"document_id": "d"}`, "invalid-request", 400},
		{"unknown-field", id, tokenAdmin, `{"force": true}`, "invalid-request", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.snapshotRows(t, id)
			rec := e.do(http.MethodPost, "/api/v1/admin/jobs/"+c.job+"/erase", c.token, c.body)
			if rec.Code != c.status || problemType(rec) != c.slug {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, c.status, c.slug)
			}
			if after := e.snapshotRows(t, id); after != before {
				t.Fatal("a refused request changed the job")
			}
		})
	}
}

// A rerun pair shares the package and the registration. Erasing the old job
// keeps every row of the new one and leaves no row with the old job_id: the
// new job's rerun_of becomes NULL and packages.job_id moves to the new job.
// Erasing the new one too then leaves nothing of either.
func TestEraseRerunPair(t *testing.T) {
	e := newEnv(t)
	old := e.seed(t, "bbb-test-a")
	e.setState(t, old, "failed")
	m := decode(t, e.post(old, "rerun", "{}"))
	next := m["job_id"].(string)
	// The old job ends cleaned/purged once its cleanup ran.
	e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name = 'cleanup'`, old)
	e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'purged', cleaned_at = now() WHERE job_id = $1`, old)
	pkg, record := e.ids(t, old)
	before := e.snapshotRows(t, next)

	rec := e.post(old, "erase", "{}")
	if m := decodeErased(t, rec); m["document_id"] != nil {
		t.Fatalf("answer %v", m)
	}
	var rerunOf *string
	if err := e.pool.QueryRow(bg, `SELECT rerun_of::text FROM jobs WHERE job_id = $1`, next).Scan(&rerunOf); err != nil ||
		rerunOf != nil {
		t.Fatalf("rerun_of %v (%v), want NULL", rerunOf, err)
	}
	// Expected changes: rerun_of becomes NULL, the package points at next.
	before = strings.Replace(before, `"rerun_of":"`+old+`"`, `"rerun_of":null`, 1)
	before = strings.Replace(before, `"job_id":"`+old+`"`, `"job_id":"`+next+`"`, 1)
	if after := e.snapshotRows(t, next); after != before {
		t.Fatalf("erasing the old job changed the new one:\n%s\n%s", before, after)
	}
	if got := e.leftovers(t, old); len(got) != 0 {
		t.Fatalf("rows with the old job_id %v, want none", got)
	}
	var pkgJob string
	if err := e.pool.QueryRow(bg, `SELECT job_id::text FROM packages WHERE package_id = $1`, pkg).
		Scan(&pkgJob); err != nil || pkgJob != next {
		t.Fatalf("packages.job_id %s (%v), want the remaining job %s", pkgJob, err, next)
	}
	rc, err := e.blobs.Get(bg, next, pkg)
	if err != nil {
		t.Fatalf("manifest blob of the new job: %v", err)
	}
	_ = rc.Close()

	e.setState(t, next, "cleaned")
	if rec := e.post(next, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase of the new job: %d %s", rec.Code, rec.Body)
	}
	if got := e.leftovers(t, old, next, pkg, record); len(got) != 0 {
		t.Fatalf("rows left after erasing both: %v", got)
	}
}

// A failing blob delete is logged at WARN with the job_id and an error
// class, never the error text, and does not undo the erasure.
func TestEraseBlobDeleteFails(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "cleaned")
	e.mux = http.NewServeMux()
	New(Config{Pool: e.pool, Blobs: failingDelete{e.blobs}, Log: slog.New(slog.NewJSONHandler(e.logs, nil))}).
		Mount(e.mux, e.creds)
	rec := e.post(id, "erase", "{}")
	decodeErased(t, rec)
	if got := e.leftovers(t, id); len(got) != 0 {
		t.Fatalf("rows left: %v", got)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, `"job_id":"`+id+`"`) ||
		!strings.Contains(logs, `"error_class":"`) || strings.Contains(logs, "blob store down") {
		t.Fatalf("log: %s", logs)
	}
}

type failingDelete struct{ blob.Store }

func (failingDelete) DeletePrefix(context.Context, string) error {
	return errors.New("blob store down")
}

// snapshotRows renders every row of a job and its package and registration.
func (e *env) snapshotRows(t *testing.T, id string) string {
	t.Helper()
	var s string
	err := e.pool.QueryRow(bg, `
		SELECT concat_ws('|',
			(SELECT row_to_json(j)::text FROM jobs j WHERE job_id = $1),
			(SELECT json_agg(s ORDER BY name)::text FROM steps s WHERE job_id = $1),
			(SELECT json_agg(i ORDER BY item)::text FROM step_items i WHERE job_id = $1),
			(SELECT row_to_json(d)::text FROM deliveries d WHERE job_id = $1),
			(SELECT json_agg(a ORDER BY audit_id)::text FROM audit a WHERE job_id = $1),
			(SELECT row_to_json(p)::text FROM packages p JOIN jobs j USING (package_id) WHERE j.job_id = $1),
			(SELECT json_agg(f ORDER BY path)::text FROM package_files f JOIN jobs j USING (package_id)
				WHERE j.job_id = $1),
			(SELECT json_agg(r ORDER BY registration_id)::text FROM registrations r
				WHERE record_id = (SELECT p.record_id FROM packages p JOIN jobs j USING (package_id)
					WHERE j.job_id = $1)))`, id).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A tombstone that the other, cleaned job of a rerun pair references stays
// when the old job is erased, and goes with the erasure of that other job.
func TestEraseKeepsTombstoneOfOtherJob(t *testing.T) {
	e := newEnv(t)
	old := e.seed(t, "bbb-test-a")
	e.setState(t, old, "failed")
	next := decode(t, e.post(old, "rerun", "{}"))["job_id"].(string)
	_, record := e.ids(t, old)
	for _, id := range []string{old, next} {
		e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1`, id)
		e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'cancelled', current_step = 'cleanup',
			cleaned_at = now() WHERE job_id = $1`, id)
	}
	e.exec(t, `UPDATE registrations SET status = 'cancelled', cancelled_at = now(), owner_sub = NULL,
		owner_email = NULL WHERE record_id = $1`, record)

	if rec := e.post(old, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase old: %d %s", rec.Code, rec.Body)
	}
	var referenced bool
	if err := e.pool.QueryRow(bg, `SELECT registration_id IS NOT NULL FROM jobs WHERE job_id = $1`, next).
		Scan(&referenced); err != nil || !referenced {
		t.Fatalf("the new job lost its tombstone (%v)", err)
	}
	if rec := e.post(next, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase new: %d %s", rec.Code, rec.Body)
	}
	if got := e.leftovers(t, old, next, record); len(got) != 0 {
		t.Fatalf("rows left: %v", got)
	}
}

// In a rerun chain of three jobs, erasing the job the package points at
// moves packages.job_id to the newest remaining job.
func TestErasePackagePointsAtNewestJob(t *testing.T) {
	e := newEnv(t)
	first := e.seed(t, "bbb-test-a")
	e.setState(t, first, "failed")
	second := decode(t, e.post(first, "rerun", "{}"))["job_id"].(string)
	third := decode(t, e.post(second, "rerun", "{}"))["job_id"].(string)
	for _, id := range []string{first, second} {
		e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1`, id)
		e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'purged', current_step = 'cleanup',
			cleaned_at = now() WHERE job_id = $1`, id)
	}
	pkg, _ := e.ids(t, first)
	if rec := e.post(first, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase: %d %s", rec.Code, rec.Body)
	}
	var pkgJob string
	if err := e.pool.QueryRow(bg, `SELECT job_id::text FROM packages WHERE package_id = $1`, pkg).
		Scan(&pkgJob); err != nil || pkgJob != third {
		t.Fatalf("packages.job_id %s (%v), want the newest job %s", pkgJob, err, third)
	}
	if got := e.leftovers(t, first); len(got) != 0 {
		t.Fatalf("rows with the erased job_id: %v", got)
	}

	// The third job too: the package moves to the second, whose rerun audit
	// row loses the third job_id in detail.
	e.setState(t, third, "cleaned")
	if rec := e.post(third, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase third: %d %s", rec.Code, rec.Body)
	}
	if err := e.pool.QueryRow(bg, `SELECT job_id::text FROM packages WHERE package_id = $1`, pkg).
		Scan(&pkgJob); err != nil || pkgJob != second {
		t.Fatalf("packages.job_id %s (%v), want the remaining job %s", pkgJob, err, second)
	}
	if got := e.leftovers(t, first, third); len(got) != 0 {
		t.Fatalf("rows with an erased job_id: %v", got)
	}
	if n := e.auditCount(t, second, "rerun"); n != 1 {
		t.Fatalf("%d rerun audit rows of the second job, want 1 (kept, detail cleared)", n)
	}
}

// Erasing the newer job of a rerun pair while the old one stays leaves no
// row with its job_id: the old job's rerun audit row named it in detail.
func TestEraseNewerJobOfPair(t *testing.T) {
	e := newEnv(t)
	old := e.seed(t, "bbb-test-a")
	e.setState(t, old, "failed")
	next := decode(t, e.post(old, "rerun", "{}"))["job_id"].(string)
	var detail string
	if err := e.pool.QueryRow(bg, `SELECT detail FROM audit WHERE job_id = $1 AND action = 'rerun'`, old).
		Scan(&detail); err != nil || detail != next {
		t.Fatalf("rerun audit detail %q (%v), want the new job_id", detail, err)
	}
	e.setState(t, next, "cleaned")
	before := e.snapshotRows(t, old)

	if rec := e.post(next, "erase", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("erase: %d %s", rec.Code, rec.Body)
	}
	if got := e.leftovers(t, next); len(got) != 0 {
		t.Fatalf("rows with the erased job_id: %v", got)
	}
	// The old job keeps its rows; only the detail of its rerun row is cleared.
	before = strings.Replace(before, `"detail":"`+next+`"`, `"detail":null`, 1)
	if after := e.snapshotRows(t, old); after != before {
		t.Fatalf("erasing the new job changed the old one:\n%s\n%s", before, after)
	}
}
