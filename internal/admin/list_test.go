package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type listBody struct {
	Jobs       []map[string]any `json:"jobs"`
	NextCursor *string          `json:"next_cursor"`
}

func (e *env) list(t *testing.T, query string) listBody {
	t.Helper()
	rec := e.do(http.MethodGet, "/api/v1/admin/jobs?"+query, tokenAdmin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET jobs?%s: %d %s", query, rec.Code, rec.Body)
	}
	var b listBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Jobs == nil {
		t.Fatalf("GET jobs?%s: %v %s", query, err, rec.Body)
	}
	return b
}

func ids(b listBody) []string {
	var out []string
	for _, j := range b.Jobs {
		out = append(out, j["job_id"].(string))
	}
	return out
}

func TestListFilters(t *testing.T) {
	e := newEnv(t)
	held := e.seed(t, "bbb-test-a")
	e.setState(t, held, "held/no_registration")
	failed := e.seed(t, "bbb-test-a")
	e.setState(t, failed, "failed")
	cleaned := e.seed(t, "bbb-test-b")
	e.setState(t, cleaned, "cleaned")
	e.exec(t, `UPDATE jobs SET created_at = '2026-01-01T00:00:00Z' WHERE job_id = $1`, held)

	for _, c := range []struct {
		query string
		want  []string
	}{
		{"", []string{cleaned, failed, held}},
		{"state=held", []string{held}},
		{"held_reason=no_registration", []string{held}},
		{"held_reason=delivery_uncertain", nil},
		{"outcome=delivered", []string{cleaned}},
		{"bbb_server_id=bbb-test-a", []string{failed, held}},
		{"state=failed&bbb_server_id=bbb-test-a", []string{failed}},
		{"created_before=2026-02-01T00:00:00Z", []string{held}},
		{"created_after=2026-02-01T00:00:00Z", []string{cleaned, failed}},
	} {
		t.Run(c.query, func(t *testing.T) {
			got := ids(e.list(t, c.query))
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
	for _, q := range []string{"state=paused", "held_reason=x", "outcome=lost", "bbb_server_id=BAD", "created_after=yesterday",
		"limit=0", "limit=201", "limit=x", "cursor=%%%", "cursor=" + url.QueryEscape(encodeCursor(time.Now(), "x")),
		"owner=ada", "state=held&state=failed"} {
		t.Run("bad/"+q, func(t *testing.T) {
			rec := e.do(http.MethodGet, "/api/v1/admin/jobs?"+q, tokenAdmin, "")
			if rec.Code != http.StatusBadRequest || problemType(rec) != "invalid-request" {
				t.Fatalf("got %d %s, want 400 invalid-request", rec.Code, rec.Body)
			}
		})
	}
	if rec := e.do(http.MethodGet, "/api/v1/admin/jobs", tokenFE, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("frontend credential: %d", rec.Code)
	}
}

// Two pages in a stable order, newest first; equal created_at values are
// ordered by job_id.
func TestListPaging(t *testing.T) {
	e := newEnv(t)
	var all []string
	for range 5 {
		all = append(all, e.seed(t, "bbb-test-a"))
	}
	// Two pairs share a timestamp, so the tie-break matters.
	e.exec(t, `UPDATE jobs SET created_at = '2026-03-01T00:00:00Z' WHERE job_id = ANY($1)`, all[:2])
	e.exec(t, `UPDATE jobs SET created_at = '2026-03-02T00:00:00Z' WHERE job_id = ANY($1)`, all[2:4])
	full := ids(e.list(t, "limit=200"))
	if len(full) != 5 {
		t.Fatalf("full list %v", full)
	}
	p1 := e.list(t, "limit=3")
	if p1.NextCursor == nil {
		t.Fatal("page 1: next_cursor null")
	}
	p2 := e.list(t, "limit=3&cursor="+url.QueryEscape(*p1.NextCursor))
	if p2.NextCursor != nil {
		t.Fatalf("page 2: next_cursor %v, want null", *p2.NextCursor)
	}
	if got := append(ids(p1), ids(p2)...); strings.Join(got, ",") != strings.Join(full, ",") {
		t.Fatalf("pages %v, want %v", got, full)
	}
	if strings.Join(ids(e.list(t, "limit=3")), ",") != strings.Join(ids(p1), ",") {
		t.Fatal("page 1 is not stable")
	}
	if b := e.list(t, "limit=5"); b.NextCursor != nil {
		t.Fatal("exactly one full page: next_cursor must be null")
	}
}

type auditBody struct {
	Entries    []map[string]any `json:"entries"`
	NextCursor *string          `json:"next_cursor"`
}

func (e *env) audit(t *testing.T, query string) auditBody {
	t.Helper()
	rec := e.do(http.MethodGet, "/api/v1/admin/audit?"+query, tokenAdmin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit?%s: %d %s", query, rec.Code, rec.Body)
	}
	var b auditBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Entries == nil {
		t.Fatalf("GET audit?%s: %v %s", query, err, rec.Body)
	}
	return b
}

func TestAuditList(t *testing.T) {
	e := newEnv(t)
	a := e.seed(t, "bbb-test-a")
	e.setState(t, a, "failed")
	b := e.seed(t, "bbb-test-a")
	e.setState(t, b, "held/no_registration")
	decode(t, e.post(a, "retry", "{}"))
	decode(t, e.post(b, "release", "{}"))
	if rec := e.post(b, "purge", "{}"); rec.Code != http.StatusConflict { // b is running now
		t.Fatalf("purge of a running job: %d", rec.Code)
	}
	got := e.audit(t, "job_id="+a)
	if len(got.Entries) != 1 {
		t.Fatalf("entries %v", got.Entries)
	}
	want := map[string]any{"actor_kind": "admin", "actor_id": "test-admin", "action": "retry", "job_id": a, "detail": "running"}
	for k, v := range want {
		if got.Entries[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got.Entries[0][k], v)
		}
	}
	if at, _ := got.Entries[0]["at"].(string); len(at) != len("2026-10-29T10:00:00Z") || !strings.HasSuffix(at, "Z") {
		t.Errorf("at = %q, want RFC 3339 UTC", at)
	}
	if len(got.Entries[0]) != 6 {
		t.Errorf("entry keys %v", got.Entries[0])
	}

	// Paging over all rows, newest first.
	all := e.audit(t, "limit=200").Entries
	if len(all) != 2 || all[0]["job_id"] != b {
		t.Fatalf("all entries %v", all)
	}
	p1 := e.audit(t, "limit=1")
	p2 := e.audit(t, "limit=1&cursor="+url.QueryEscape(*p1.NextCursor))
	if p1.Entries[0]["job_id"] != b || p2.Entries[0]["job_id"] != a || p2.NextCursor != nil {
		t.Fatalf("pages %v %v", p1, p2)
	}
	for _, q := range []string{"job_id=nope", "actor_id=x", "cursor=abc"} {
		if rec := e.do(http.MethodGet, "/api/v1/admin/audit?"+q, tokenAdmin, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("audit?%s: %d, want 400", q, rec.Code)
		}
	}
}

// No admin answer carries personal data (architecture.md §4, §11): no
// owner field, sub, email or meeting name.
func TestNoPersonalData(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.setState(t, id, "held/delivery_uncertain")
	var bodies []string
	add := func(method, path, body string) {
		rec := e.do(method, path, tokenAdmin, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	add(http.MethodGet, "/api/v1/admin/jobs/"+id, "")
	add(http.MethodPost, "/api/v1/admin/jobs/"+id+"/release", `{"document_id": "doc-1"}`)
	add(http.MethodPost, "/api/v1/admin/jobs/"+id+"/rerun", "{}")
	add(http.MethodGet, "/api/v1/admin/jobs", "")
	add(http.MethodGet, "/api/v1/admin/audit", "")
	add(http.MethodGet, "/api/v1/admin/audit?job_id="+id, "")
	bodies = append(bodies, e.logs.String())
	for _, b := range bodies {
		for _, bad := range []string{`"owner`, `"sub"`, `"email"`, ownerSub, ownerEmail, "@example.org", meetingName, "title"} {
			if strings.Contains(b, bad) {
				t.Errorf("answer or log contains %q: %s", bad, b)
			}
		}
	}
}
