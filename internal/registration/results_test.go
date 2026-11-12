package registration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// Sink profiles of these tests (metadata-interface.md §2.7, sinks.md §3.1, §4a).
var testSinks = map[string]Sink{
	"docs-test":     {DocumentURLTemplate: "https://docs.recap.test/docs/{id}/"},
	"docs-no-link":  {},
	"frontend-test": {Frontend: true},
}

const summaryText = "# Fixture summary\n\nAda Testperson will send the widget drawings.\n"

// newResultEnv is newTestEnv with the sink profiles and a results store.
func newResultEnv(t *testing.T) (*testEnv, *blob.FS) {
	t.Helper()
	e := newTestEnv(t)
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.api.sinks, e.api.results = testSinks, store
	t.Cleanup(func() {
		if strings.Contains(e.logs.String(), "widget drawings") {
			t.Error("log contains summary text")
		}
	})
	return e, store
}

// delivered registers recordID, joins a job and delivers it to profile with
// documentID ("" = NULL), deliveredAgo ago. It returns the job ID.
func delivered(t *testing.T, e *testEnv, recordID, profile, documentID string, deliveredAgo time.Duration) string {
	t.Helper()
	e.register(t, recordID, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, recordID)
	join(t, e.pool, jobID, recordID)
	deliver(t, e.pool, jobID, profile, documentID, deliveredAgo)
	return jobID
}

func deliver(t *testing.T, pool *pgxpool.Pool, jobID, profile, documentID string, deliveredAgo time.Duration) {
	t.Helper()
	atDeliver(t, pool, jobID, "delivered", "succeeded")
	exec(t, pool, `UPDATE jobs SET sink_profile = $2, current_step = 'cleanup',
		delivered_at = now() - $3::bigint * interval '1 microsecond' WHERE job_id = $1`,
		jobID, profile, deliveredAgo.Microseconds())
	exec(t, pool, `INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
		VALUES ($1, $2, 'sent', nullif($3, ''), now())`, jobID, profile, documentID)
}

func (e *testEnv) job(t *testing.T, recordID string) *jobRef {
	t.Helper()
	resp := expectReg(t, e.do(t, http.MethodGet, path(recordID), tokenFE, nil), http.StatusOK, "matched")
	if resp.Job == nil {
		t.Fatal("matched without job")
	}
	return resp.Job
}

func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// Before delivery every result field is null or false; after it outcome,
// delivered_at and document_url follow the delivery; after cleanup they
// stay (the result record).
func TestResultFields(t *testing.T) {
	e, _ := newResultEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	if j := e.job(t, rid); j.Outcome != nil || j.DeliveredAt != nil || j.DocumentURL != nil || j.SummaryAvailable {
		t.Fatalf("before delivery: %+v", j)
	}
	r := e.do(t, http.MethodGet, path(rid), tokenFE, nil)
	if !strings.Contains(string(r.body), `"schema_version":"1.1.0"`) ||
		!strings.Contains(string(r.body), `"summary_available":false`) || !strings.Contains(string(r.body), `"outcome":null`) {
		t.Fatalf("response %s", r.body)
	}

	deliver(t, e.pool, jobID, "docs-test", "doc-0001", time.Minute)
	j := e.job(t, rid)
	if str(j.Outcome) != "delivered" || j.DeliveredAt == nil || str(j.DocumentURL) != "https://docs.recap.test/docs/doc-0001/" ||
		j.SummaryAvailable {
		t.Fatalf("after delivery: outcome %s delivered_at %s url %s summary %v", str(j.Outcome), str(j.DeliveredAt),
			str(j.DocumentURL), j.SummaryAvailable)
	}
	at := *j.DeliveredAt

	// cleanup: owner gone, row kept, job cleaned/delivered.
	exec(t, e.pool, `UPDATE registrations SET owner_sub = NULL, owner_email = NULL, expires_at = NULL,
		result_expires_at = now() + interval '7 days' WHERE record_id = $1`, rid)
	exec(t, e.pool, `UPDATE jobs SET state = 'cleaned', outcome = 'delivered', cleaned_at = now() WHERE job_id = $1`, jobID)
	j = e.job(t, rid)
	if j.State != "cleaned" || str(j.Outcome) != "delivered" || str(j.DeliveredAt) != at ||
		str(j.DocumentURL) != "https://docs.recap.test/docs/doc-0001/" {
		t.Fatalf("after cleanup: %+v", j)
	}
	// DELETE of the delivered registration stays 409.
	expectProblem(t, e.do(t, http.MethodDelete, path(rid), tokenFE, nil), http.StatusConflict, "already-delivered")
}

// document_url is null for a profile without document_url_template, for a
// release without document ID, for the frontend sink and for an unknown
// profile (§2.7).
func TestDocumentURLNull(t *testing.T) {
	e, _ := newResultEnv(t)
	for _, c := range []struct{ name, profile, documentID string }{
		{"no-template", "docs-no-link", "doc-0002"},
		{"release-without-id", "docs-test", ""},
		{"frontend", "frontend-test", ""},
		{"unknown-profile", "docs-removed", "doc-0003"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rid := newRecordID()
			delivered(t, e, rid, c.profile, c.documentID, time.Minute)
			if j := e.job(t, rid); j.DocumentURL != nil || str(j.Outcome) != "delivered" {
				t.Fatalf("document_url %s outcome %s", str(j.DocumentURL), str(j.Outcome))
			}
		})
	}
	// A confirmed_by_admin delivery with an ID gets the link.
	rid := newRecordID()
	jobID := delivered(t, e, rid, "docs-test", "doc-0004", time.Minute)
	exec(t, e.pool, `UPDATE deliveries SET state = 'confirmed_by_admin' WHERE job_id = $1`, jobID)
	if j := e.job(t, rid); str(j.DocumentURL) != "https://docs.recap.test/docs/doc-0004/" {
		t.Fatalf("release with ID: %s", str(j.DocumentURL))
	}
}

// GET …/summary returns the frontend summary to the registering front end
// until DELETE …/summary; every other case is 404 summary-not-available.
func TestSummary(t *testing.T) {
	e, store := newResultEnv(t)
	rid := newRecordID()
	sumPath := path(rid) + "/summary"

	// No registration.
	expectProblem(t, e.do(t, http.MethodGet, sumPath, tokenFE, nil), http.StatusNotFound, "summary-not-available")
	expectProblem(t, e.do(t, http.MethodDelete, sumPath, tokenFE, nil), http.StatusNotFound, "summary-not-available")

	jobID := delivered(t, e, rid, "frontend-test", "", time.Minute)
	// Delivered, but the summary is not (yet) stored.
	expectProblem(t, e.do(t, http.MethodGet, sumPath, tokenFE, nil), http.StatusNotFound, "summary-not-available")
	if err := store.PutSummary(bg, jobID, []byte(summaryText)); err != nil {
		t.Fatal(err)
	}
	if j := e.job(t, rid); !j.SummaryAvailable || j.DocumentURL != nil {
		t.Fatalf("frontend delivery: %+v", j)
	}

	r := e.do(t, http.MethodGet, sumPath, tokenFE, nil)
	expectStatus(t, r, http.StatusOK)
	if ct := r.header.Get("Content-Type"); ct != "text/markdown; charset=utf-8" || string(r.body) != summaryText {
		t.Fatalf("GET summary: %q %q", ct, r.body)
	}

	// Another front end: 404 without detail, nothing deleted.
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		p := expectProblem(t, e.do(t, m, sumPath, tokenOther, nil), http.StatusNotFound, "summary-not-available")
		if p.Detail != "" {
			t.Errorf("foreign %s: detail %q", m, p.Detail)
		}
	}
	expectProblem(t, e.do(t, http.MethodGet, sumPath, "", nil), http.StatusUnauthorized, "unauthorized")
	expectStatus(t, e.do(t, http.MethodGet, sumPath, tokenFE, nil), http.StatusOK)

	// Pickup: 204, gone, idempotent, the record stays.
	expectStatus(t, e.do(t, http.MethodDelete, sumPath, tokenFE, nil), http.StatusNoContent)
	expectProblem(t, e.do(t, http.MethodGet, sumPath, tokenFE, nil), http.StatusNotFound, "summary-not-available")
	expectStatus(t, e.do(t, http.MethodDelete, sumPath, tokenFE, nil), http.StatusNoContent)
	if j := e.job(t, rid); j.SummaryAvailable || str(j.Outcome) != "delivered" {
		t.Fatalf("after pickup: %+v", j)
	}
	if !strings.Contains(e.logs.String(), `"msg":"summary fetched"`) || !strings.Contains(e.logs.String(), `"bytes":`) {
		t.Errorf("no fetch log line with the size: %s", e.logs)
	}
}

// The summary of a Docs delivery is never served; DELETE before delivery
// deletes nothing. (A summary past RECAP_RESULT_TTL is deleted by the
// cleanup loop together with the result record: internal/retention.)
func TestSummaryNotAvailable(t *testing.T) {
	e, store := newResultEnv(t)

	docs := newRecordID()
	jobID := delivered(t, e, docs, "docs-test", "doc-0005", time.Minute)
	if err := store.PutSummary(bg, jobID, []byte(summaryText)); err != nil { // stray: not a frontend profile
		t.Fatal(err)
	}
	expectProblem(t, e.do(t, http.MethodGet, path(docs)+"/summary", tokenFE, nil), http.StatusNotFound,
		"summary-not-available")
	expectStatus(t, e.do(t, http.MethodDelete, path(docs)+"/summary", tokenFE, nil), http.StatusNoContent)

	running := newRecordID()
	e.register(t, running, "meeting-1", ownerA)
	jobID = seedJob(t, e.pool, running)
	join(t, e.pool, jobID, running)
	if err := store.PutSummary(bg, jobID, []byte(summaryText)); err != nil { // deliver is writing it
		t.Fatal(err)
	}
	expectProblem(t, e.do(t, http.MethodGet, path(running)+"/summary", tokenFE, nil), http.StatusNotFound,
		"summary-not-available")
	expectStatus(t, e.do(t, http.MethodDelete, path(running)+"/summary", tokenFE, nil), http.StatusNoContent)
	rc, err := store.GetSummary(bg, jobID)
	if err != nil {
		t.Fatalf("DELETE before delivery removed the summary: %v", err)
	}
	_ = rc.Close()
}

// Without a results store (a backend that keeps none) nothing is available.
func TestSummaryWithoutStore(t *testing.T) {
	e := newTestEnv(t)
	e.api.sinks = testSinks
	rid := newRecordID()
	delivered(t, e, rid, "frontend-test", "", time.Minute)
	if j := e.job(t, rid); j.SummaryAvailable {
		t.Error("summary available without a store")
	}
	expectProblem(t, e.do(t, http.MethodGet, path(rid)+"/summary", tokenFE, nil), http.StatusNotFound,
		"summary-not-available")
	expectStatus(t, e.do(t, http.MethodDelete, path(rid)+"/summary", tokenFE, nil), http.StatusNoContent)
}

// …/summary counts against the same per-front-end limit as the
// registration endpoints (§2.6, §2.7).
func TestSummaryRateLimited(t *testing.T) {
	e, _ := newResultEnv(t)
	e.api.limiter = newLimiter(2, time.Minute)
	rid := newRecordID()
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusNotFound, "not-found")
	expectProblem(t, e.do(t, http.MethodGet, path(rid)+"/summary", tokenFE, nil), http.StatusNotFound,
		"summary-not-available")
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		r := e.do(t, m, path(rid)+"/summary", tokenFE, nil)
		expectProblem(t, r, http.StatusTooManyRequests, "rate-limited")
		if r.header.Get("Retry-After") == "" {
			t.Fatal("no Retry-After")
		}
	}
	expectProblem(t, e.do(t, http.MethodGet, path(rid), tokenFE, nil), http.StatusTooManyRequests, "rate-limited")
	expectProblem(t, e.do(t, http.MethodGet, path(rid)+"/summary", tokenOther, nil), http.StatusNotFound,
		"summary-not-available")
}

// DELETE …/summary of a job delivered to another sink than frontend is 204
// and does nothing: no blob delete and no "summary pickup confirmed" line.
func TestSummaryDeleteOtherSink(t *testing.T) {
	e, store := newResultEnv(t)
	rid := newRecordID()
	jobID := delivered(t, e, rid, "docs-test", "doc-0006", time.Minute)
	if err := store.PutSummary(bg, jobID, []byte(summaryText)); err != nil { // a marker only this test writes
		t.Fatal(err)
	}
	expectStatus(t, e.do(t, http.MethodDelete, path(rid)+"/summary", tokenFE, nil), http.StatusNoContent)
	rc, err := store.GetSummary(bg, jobID)
	if err != nil {
		t.Fatalf("DELETE on a Docs delivery ran a blob delete: %v", err)
	}
	_ = rc.Close()
	if strings.Contains(e.logs.String(), "summary pickup confirmed") {
		t.Errorf("pickup logged for a Docs delivery: %s", e.logs)
	}
}
