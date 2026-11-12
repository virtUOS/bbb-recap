package adminui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/admin"
	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// These tests need TEST_DATABASE_URL (internal/db/dbtest); without it they skip.

// The owner and content values of the F-01 fixture (docs/reference/fixtures.md
// §4, §5) are seeded into every job's registration and blobs; no page may
// show them. The admin logs in as the fixture admin, so a hit on the owner's
// subject cannot come from the header or an audit actor. The meeting and
// record IDs are F-01-like IDs; the job page shows them, as the API does.
const (
	ownerSub     = "fixture-sub-ada-0001"
	ownerEmail   = "ada.testperson@example.org"
	meetingName  = "Fixture Meeting Widget Planning"
	transcript   = "Ada Testperson: the widget ships on Friday."
	adminSubject = testSubject
	meetingID    = "fixture-meeting-01"
	testDocID    = "doc-test-ui-1"
)

var bg = context.Background()

// viewEnv is the UI with a database, a filesystem blob store and the admin API.
type viewEnv struct {
	*env
	pool  *pgxpool.Pool
	blobs blob.Store
	pages []string // every body the tests received, for the privacy scan
}

func newViewEnv(t *testing.T) *viewEnv {
	t.Helper()
	pool := dbtest.New(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	api := admin.New(admin.Config{Pool: pool, Blobs: blobs, ConfigHash: "config-hash-test",
		RetentionAfterDelivery: time.Hour,
		Log:                    slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	e := &viewEnv{env: newEnvWith(t, admins(), adminSubject, api, logs), pool: pool, blobs: blobs}
	t.Cleanup(func() { e.scan(t) })
	return e
}

// scan fails if a received page or the log holds an owner or content value.
func (e *viewEnv) scan(t *testing.T) {
	t.Helper()
	if len(e.pages) == 0 {
		return
	}
	needles := []string{ownerSub, ownerEmail, meetingName, transcript, "Testperson", "testperson", "Widget", "widget"}
	for i, p := range e.pages {
		// A Go value printed whole (a template bug) could carry anything.
		for _, n := range []string{"{", "&lt;nil&gt;"} {
			if strings.Contains(p, n) {
				t.Errorf("page %d contains %q, a value printed whole:\n%s", i, n, p)
			}
		}
		for _, n := range needles {
			if strings.Contains(p, n) {
				t.Errorf("page %d contains %q:\n%s", i, n, p)
			}
		}
	}
	logs := e.logs.String()
	for _, n := range append(needles, adminSubject) {
		if strings.Contains(logs, n) {
			t.Errorf("log contains %q", n)
		}
	}
}

// page GETs path, following redirects, and keeps the body for the scan.
func (e *viewEnv) page(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, body := e.get(t, path)
	e.pages = append(e.pages, body)
	return resp.StatusCode, body
}

// submit POSTs form to path without following the redirect. It returns the
// status, the Location and the body (kept for the scan).
func (e *viewEnv) submit(t *testing.T, path string, form url.Values) (int, string, string) {
	t.Helper()
	resp, err := e.noFollow().PostForm(e.ui.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	e.pages = append(e.pages, string(b))
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func (e *viewEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(bg, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// seed creates a committed package with its blobs (a manifest with the
// meeting name, an audio file and a transcript) and its job, registered for
// the fixture owner. It returns the job ID.
func (e *viewEnv) seed(t *testing.T) string {
	t.Helper()
	id := queue.NewJobID()
	manifest := []byte(`{"meeting_name":"` + meetingName + `","n":"` + randomHex(8) + `"}`)
	audio := []byte("audio-" + randomHex(8))
	for _, b := range [][]byte{manifest, audio, []byte(transcript)} {
		if err := e.blobs.Put(bg, id, sum(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	pkg, record := sum(manifest), "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-"+randomHex(6)
	err := pgx.BeginFunc(bg, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(bg, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, 'bbb-test-a', $3, $4, 'committed', 0)`, pkg, id, record, meetingID); err != nil {
			return err
		}
		if _, err := tx.Exec(bg, `
			INSERT INTO package_files (package_id, path, role, size, sha256, verified_at)
			VALUES ($1, 'audio/a.opus', 'audio-microphone', $2, $3, now())`, pkg, len(audio), sum(audio)); err != nil {
			return err
		}
		if err := queue.CreateJob(bg, tx, queue.NewJob{JobID: id, PackageID: pkg, ConfigHash: "old-hash"}); err != nil {
			return err
		}
		_, err := tx.Exec(bg, `
			WITH r AS (
				INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status)
				VALUES ($2, 'fe-test', $5, $3, $4, 'registered') RETURNING registration_id)
			UPDATE jobs SET registration_id = (SELECT registration_id FROM r), sink_profile = 'docs-test'
			WHERE job_id = $1`, id, record, ownerSub, ownerEmail, meetingID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// states are the job states the tests put a job into by direct writes (the
// queue tests cover how the engine gets there).
var states = []string{"received", "running", "held/no_registration", "held/delivery_uncertain", "failed",
	"delivered", "cleaned"}

func (e *viewEnv) setState(t *testing.T, id, state string) {
	t.Helper()
	done := func(upTo string) {
		e.exec(t, `UPDATE steps SET state = 'succeeded', attempt = 1
			WHERE job_id = $1 AND name = ANY($2)`, id, queue.Steps[:slices.Index(queue.Steps, upTo)])
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
		e.exec(t, `UPDATE steps SET state = 'failed', attempt = 3, last_error_class = 'permanent',
			last_error_code = 'events-mismatch' WHERE job_id = $1 AND name = 'normalize'`, id)
		e.exec(t, `UPDATE jobs SET state = 'failed', failure_reason = 'events_mismatch' WHERE job_id = $1`, id)
		e.exec(t, `INSERT INTO audit (actor_kind, action, job_id, detail)
			VALUES ('system', 'job_state', $1, 'failed/events_mismatch')`, id)
	case "delivered":
		done("cleanup")
		e.exec(t, `UPDATE steps SET run_after = now() + interval '1 hour' WHERE job_id = $1 AND name = 'cleanup'`, id)
		e.exec(t, `UPDATE jobs SET state = 'delivered', delivered_at = now() WHERE job_id = $1`, id)
		e.exec(t, `INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
			VALUES ($1, 'docs-test', 'sent', $2, now())`, id, testDocID)
	case "cleaned":
		done("cleanup")
		e.exec(t, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name = 'cleanup'`, id)
		e.exec(t, `UPDATE jobs SET state = 'cleaned', outcome = 'delivered', cleaned_at = now() WHERE job_id = $1`, id)
		e.exec(t, `INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
			VALUES ($1, 'docs-test', 'sent', $2, now())`, id, testDocID)
	default:
		t.Fatalf("unknown state %s", state)
	}
}

// snapshot renders everything an action may change about a job.
func (e *viewEnv) snapshot(t *testing.T, id string) string {
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

// auditRows returns "actor_kind actor_id" of the audit rows of action, for
// jobID or, with jobID "", of rows without a job.
func (e *viewEnv) auditRows(t *testing.T, jobID, action string) []string {
	t.Helper()
	rows, err := e.pool.Query(bg, `
		SELECT actor_kind || ' ' || coalesce(actor_id, '') FROM audit
		WHERE action = $2 AND job_id IS NOT DISTINCT FROM nullif($1, '')::uuid`, jobID, action)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Each view renders for a job in every state: the list (also filtered),
// the job page with its steps and actions, every confirmation page (the
// rerun hint only for a delivered job) and the audit list. The cleanup scan
// checks every page for owner values.
func TestViewsEveryState(t *testing.T) {
	e := newViewEnv(t)
	ids := map[string]string{}
	for _, st := range states {
		ids[st] = e.seed(t)
		e.setState(t, ids[st], st)
	}
	e.login(t)

	code, body := e.page(t, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("job list = %d", code)
	}
	for st, id := range ids {
		if !strings.Contains(body, id) {
			t.Errorf("job list lacks the %s job", st)
		}
	}
	// Newest first: the last seeded job comes first.
	if strings.Index(body, ids["cleaned"]) > strings.Index(body, ids["received"]) {
		t.Error("job list is not newest first")
	}

	for _, f := range []struct {
		query string
		want  []string
	}{
		{"state=held", []string{"held/no_registration", "held/delivery_uncertain"}},
		{"held_reason=delivery_uncertain", []string{"held/delivery_uncertain"}},
		{"state=cleaned", []string{"cleaned"}},
	} {
		code, body := e.page(t, "/admin/?"+f.query)
		if code != http.StatusOK {
			t.Fatalf("%s = %d", f.query, code)
		}
		for st, id := range ids {
			if got, want := strings.Contains(body, id), slices.Contains(f.want, st); got != want {
				t.Errorf("?%s: %s job shown %v, want %v", f.query, st, got, want)
			}
		}
	}
	if code, body := e.page(t, "/admin/?state=bogus"); code != http.StatusBadRequest ||
		!strings.Contains(body, "400 invalid-request") || !strings.Contains(body, `<a href="/admin/">Back</a>`) {
		t.Errorf("bad filter = %d", code)
	}

	wantActions := map[string][]string{
		"received":                {"rerun"},
		"running":                 {"rerun"},
		"held/no_registration":    {"release", "rerun", "purge"},
		"held/delivery_uncertain": {"retry", "release", "rerun", "purge"},
		"failed":                  {"retry", "rerun", "purge"},
		"delivered":               {"rerun"},
		"cleaned":                 {"erase"},
	}
	for st, id := range ids {
		code, body := e.page(t, "/admin/jobs/"+id)
		if code != http.StatusOK {
			t.Fatalf("job page %s = %d", st, code)
		}
		state, reason, _ := strings.Cut(st, "/")
		var record string
		if err := e.pool.QueryRow(bg, `SELECT p.record_id FROM jobs j JOIN packages p USING (package_id)
			WHERE j.job_id = $1`, id).Scan(&record); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"<dd>" + state + "</dd>", "<td>validate</td>", "<td>cleanup</td>",
			"/admin/audit?job_id=" + id, "<dt>Meeting ID</dt><dd><code>" + meetingID + "</code></dd>",
			"<dt>Record ID</dt><dd><code>" + record + "</code></dd>"} {
			if !strings.Contains(body, want) {
				t.Errorf("job page %s lacks %q", st, want)
			}
		}
		if st == "held/no_registration" && !strings.Contains(body, "<dt>Detail</dt><dd><code>no-registration</code></dd>") {
			t.Errorf("job page %s lacks the detail code", st)
		}
		if reason != "" && !strings.Contains(body, "<dt>Held reason</dt><dd>"+reason+"</dd>") {
			t.Errorf("job page %s lacks the held reason", st)
		}
		if hasDoc := strings.Contains(body, "<code>"+testDocID+"</code>"); hasDoc != (st == "delivered" || st == "cleaned") {
			t.Errorf("job page %s shows the document ID: %v", st, hasDoc)
		}
		for _, a := range []string{"retry", "release", "rerun", "purge", "erase"} {
			link := `href="/admin/jobs/` + id + `/` + a + `"`
			if got, want := strings.Contains(body, link), slices.Contains(wantActions[st], a); got != want {
				t.Errorf("job page %s: action %s offered %v, want %v", st, a, got, want)
			}
			code, cbody := e.page(t, "/admin/jobs/"+id+"/"+a)
			if code != http.StatusOK || !csrfRE.MatchString(cbody) ||
				!strings.Contains(cbody, `action="/admin/jobs/`+id+`/`+a+`"`) {
				t.Errorf("confirmation page %s/%s = %d", st, a, code)
			}
			if docField := strings.Contains(cbody, `name="document_id"`); docField != (a == "release" && st == "held/delivery_uncertain") {
				t.Errorf("confirmation page %s/%s: document ID field %v", st, a, docField)
			}
			if hint := strings.Contains(cbody, "may not pick up a new result"); hint != (a == "rerun" && st == "delivered") {
				t.Errorf("confirmation page %s/%s: rerun hint %v", st, a, hint)
			}
		}
	}
	// The failed job's step shows its attempts and error codes.
	if _, body := e.page(t, "/admin/jobs/"+ids["failed"]); !strings.Contains(body,
		"<td>normalize</td><td>failed</td><td>3</td>") || !strings.Contains(body, "<code>events-mismatch</code>") {
		t.Errorf("failed job page lacks the step's attempt or error code:\n%s", body)
	}

	for _, path := range []string{"/admin/jobs/" + queue.NewJobID(), "/admin/jobs/not-a-uuid",
		"/admin/jobs/" + ids["failed"] + "/delete"} {
		if code, _ := e.page(t, path); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}

	code, body = e.page(t, "/admin/audit")
	if code != http.StatusOK || !strings.Contains(body, "<code>failed/events_mismatch</code>") {
		t.Fatalf("audit = %d:\n%s", code, body)
	}
	code, body = e.page(t, "/admin/audit?job_id="+ids["failed"])
	if code != http.StatusOK || !strings.Contains(body, ids["failed"]) || strings.Contains(body, ids["received"]) {
		t.Fatalf("audit of one job = %d:\n%s", code, body)
	}
	if code, body := e.page(t, "/admin/audit?job_id=x"); code != http.StatusBadRequest ||
		!strings.Contains(body, `<a href="/admin/audit">Back</a>`) {
		t.Errorf("audit bad job_id = %d, want 400 with a link back to the audit list:\n%s", code, body)
	}
}

// The job list pages at 50, newest first, and keeps the filter.
func TestJobListPaging(t *testing.T) {
	e := newViewEnv(t)
	var ids []string
	for range 53 {
		id := e.seed(t)
		e.setState(t, id, "failed")
		ids = append(ids, id)
	}
	e.login(t)
	_, body := e.page(t, "/admin/?state=failed")
	next := regexpFind(t, body, `href="(/admin/\?[^"]*cursor=[^"]+)"`)
	if !strings.Contains(next, "state=failed") {
		t.Errorf("next link %q lost the filter", next)
	}
	if strings.Contains(body, ids[0]) || !strings.Contains(body, ids[52]) {
		t.Error("first page is not the newest 50")
	}
	_, body2 := e.page(t, strings.ReplaceAll(next, "&amp;", "&"))
	for i, id := range ids {
		if got, want := strings.Contains(body2, id), i < 3; got != want {
			t.Errorf("second page shows job %d: %v, want %v", i, got, want)
		}
	}
	if strings.Contains(body2, "Next page") || !strings.Contains(body2, "First page") {
		t.Error("second page links")
	}
}

func regexpFind(t *testing.T, s, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s", re)
	}
	return m[1]
}

// Every action in every job state: the POST needs the CSRF token; allowed
// → 303 to the job page (for rerun the new job) with a one-line result,
// the state changes as the admin API does and one audit row with the
// admin's subject; else the 409 message and nothing changes.
func TestActionMatrix(t *testing.T) {
	allowed := map[string][]string{
		"retry":   {"held/delivery_uncertain", "failed"},
		"release": {"held/no_registration", "held/delivery_uncertain"},
		"purge":   {"held/no_registration", "held/delivery_uncertain", "failed"},
		"rerun":   {"received", "running", "held/no_registration", "held/delivery_uncertain", "failed", "delivered"},
		"erase":   {"cleaned"},
	}
	// The job state after the action (architecture.md §5.1, §5.4); purge
	// asks cleanup to run, so the job stays until the worker runs it.
	wantState := map[string]string{"retry": "running", "rerun": "received"}
	e := newViewEnv(t)
	csrf := e.login(t)
	for _, action := range []string{"retry", "release", "rerun", "purge", "erase"} {
		for _, st := range states {
			t.Run(action+"/"+st, func(t *testing.T) {
				id := e.seed(t)
				e.setState(t, id, st)
				before := e.snapshot(t, id)
				path := "/admin/jobs/" + id + "/" + action

				for _, token := range []string{"", "wrong-" + csrf} {
					form := url.Values{}
					if token != "" {
						form.Set(csrfField, token)
					}
					if code, _, _ := e.submit(t, path, form); code != http.StatusForbidden {
						t.Fatalf("POST with CSRF %q = %d, want 403", token, code)
					}
				}
				if after := e.snapshot(t, id); after != before {
					t.Fatal("a POST without a valid CSRF token changed the job")
				}

				code, loc, body := e.submit(t, path, url.Values{csrfField: {csrf}})
				if !slices.Contains(allowed[action], st) {
					if code != http.StatusConflict || !strings.Contains(body, "409 invalid-job-state") {
						t.Fatalf("got %d, want the 409 message:\n%s", code, body)
					}
					if after := e.snapshot(t, id); after != before {
						t.Fatalf("409 changed the job:\n%s\n%s", before, after)
					}
					return
				}
				if code != http.StatusSeeOther {
					t.Fatalf("got %d, want 303:\n%s", code, body)
				}
				if action == "erase" {
					e.checkErased(t, id, loc)
					return
				}
				shown := strings.TrimPrefix(loc, "/admin/jobs/")
				if (action == "rerun") == (shown == id) || !strings.HasPrefix(loc, "/admin/jobs/") {
					t.Fatalf("redirect to %s", loc)
				}
				code, page := e.page(t, loc)
				if code != http.StatusOK || !strings.Contains(page, `class="flash"`) {
					t.Fatalf("job page after %s = %d without the result line:\n%s", action, code, page)
				}
				if action == "rerun" && !strings.Contains(page, "<dt>Rerun of</dt><dd><a href=\"/admin/jobs/"+id+"\">") {
					t.Error("new job page does not show rerun_of")
				}
				if _, again := e.page(t, loc); strings.Contains(again, `class="flash"`) {
					t.Error("the result line is shown twice")
				}
				var state string
				if err := e.pool.QueryRow(bg, `SELECT state FROM jobs WHERE job_id = $1`, shown).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if w, ok := wantState[action]; ok && state != w {
					t.Errorf("state %s, want %s", state, w)
				}
				if got := e.auditRows(t, id, action); len(got) != 1 || got[0] != "admin "+adminSubject {
					t.Fatalf("audit rows of %s: %q, want one by admin %s", action, got, adminSubject)
				}
			})
		}
	}
}

// checkErased follows the redirect of an erase: the page shows the Docs
// document ID once; the job is gone; one erase row by the admin, without job.
func (e *viewEnv) checkErased(t *testing.T, id, loc string) {
	t.Helper()
	if loc != "/admin/erased" {
		t.Fatalf("erase redirects to %s", loc)
	}
	code, page := e.page(t, loc)
	if code != http.StatusOK || !strings.Contains(page, `<code class="docid">`+testDocID+`</code>`) ||
		!strings.Contains(page, "<code>docs-test</code>") {
		t.Fatalf("erased page = %d without the document ID:\n%s", code, page)
	}
	if _, again := e.page(t, loc); strings.Contains(again, testDocID) {
		t.Error("the document ID is shown twice")
	}
	if code, _ := e.page(t, "/admin/jobs/"+id); code != http.StatusNotFound {
		t.Errorf("erased job page = %d, want 404", code)
	}
	if got := e.auditRows(t, "", "erase"); !slices.Contains(got, "admin "+adminSubject) {
		t.Errorf("erase audit rows %q, want one by admin %s", got, adminSubject)
	}
	var n int
	if err := e.pool.QueryRow(bg, `SELECT count(*) FROM audit WHERE job_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d audit rows keep the erased job (%v)", n, err)
	}
}

// release of delivery_uncertain takes the optional Docs document ID; other
// actions reject one, and a too long one is rejected.
func TestReleaseDocumentID(t *testing.T) {
	e := newViewEnv(t)
	csrf := e.login(t)

	id := e.seed(t)
	e.setState(t, id, "held/delivery_uncertain")
	code, loc, _ := e.submit(t, "/admin/jobs/"+id+"/release", url.Values{csrfField: {csrf}, "document_id": {" doc-found-1 "}})
	if code != http.StatusSeeOther {
		t.Fatalf("release = %d", code)
	}
	_, page := e.page(t, loc)
	if !strings.Contains(page, "<dd>confirmed_by_admin</dd>") || !strings.Contains(page, "<code>doc-found-1</code>") {
		t.Fatalf("job page after release:\n%s", page)
	}

	for _, c := range []struct {
		state, action, doc, want string
	}{
		{"failed", "retry", "doc-x", "400 invalid-request"},
		{"held/no_registration", "release", "doc-x", "400 invalid-request"},
		{"held/delivery_uncertain", "release", strings.Repeat("d", 256), "1 to 255 bytes"},
	} {
		id := e.seed(t)
		e.setState(t, id, c.state)
		before := e.snapshot(t, id)
		code, _, body := e.submit(t, "/admin/jobs/"+id+"/"+c.action, url.Values{csrfField: {csrf}, "document_id": {c.doc}})
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s %s with document ID = %d:\n%s", c.state, c.action, code, body)
		}
		if after := e.snapshot(t, id); after != before {
			t.Errorf("%s %s: a rejected request changed the job", c.state, c.action)
		}
	}
}

// An unexpected database error is a 500 page; only its class is logged.
func TestViewInternalError(t *testing.T) {
	e := newViewEnv(t)
	id := e.seed(t)
	e.login(t)
	e.exec(t, `ALTER TABLE packages RENAME TO packages_gone`)
	code, body := e.page(t, "/admin/jobs/"+id)
	if code != http.StatusInternalServerError || !strings.Contains(body, "Internal error") {
		t.Fatalf("got %d", code)
	}
	if logs := e.logs.String(); !strings.Contains(logs, `"error_class":"sqlstate-42P01"`) || strings.Contains(logs, "packages_gone") {
		t.Fatalf("log: %s", logs)
	}
}

// The stylesheet is served without a session; pages allow only it.
func TestStyleAndHeaders(t *testing.T) {
	e := newViewEnv(t)
	resp, body := e.get(t, "/admin/static/style.css")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") || body == "" {
		t.Fatalf("style.css = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	e.login(t)
	resp, _ = e.get(t, "/admin/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "style-src 'self'") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP %q", csp)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
}

// Without a session the views redirect to the login and actions are 403.
func TestViewsNeedSession(t *testing.T) {
	e := newViewEnv(t)
	id := e.seed(t)
	e.setState(t, id, "failed")
	for _, path := range []string{"/admin/", "/admin/jobs/" + id, "/admin/jobs/" + id + "/retry", "/admin/audit", "/admin/erased"} {
		resp, err := e.noFollow().Get(e.ui.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/login" {
			t.Errorf("GET %s = %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	before := e.snapshot(t, id)
	if code, _, _ := e.submit(t, "/admin/jobs/"+id+"/retry", url.Values{csrfField: {"x"}}); code != http.StatusForbidden {
		t.Errorf("POST without session = %d", code)
	}
	if e.snapshot(t, id) != before {
		t.Error("POST without session changed the job")
	}
}
