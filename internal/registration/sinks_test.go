package registration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// result_url and job.deliveries (metadata-interface.md §2.3, §2.4, §2.7;
// ADR-016).

const testResultURL = "https://pilos.recap.test/rooms/abc-def-123/summaries"

// bodyURL is body with schema_version 1.1.0 and result_url (nil: none).
func bodyURL(t *testing.T, recordID string, owner [2]string, resultURL *string) []byte {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(body(t, recordID, "meeting-1", owner), &m); err != nil {
		t.Fatal(err)
	}
	m["schema_version"] = "1.1.0"
	if resultURL != nil {
		m["result_url"] = *resultURL
	}
	b, _ := json.Marshal(m)
	return b
}

func TestResultURLStoredAndReplaced(t *testing.T) {
	e := newTestEnv(t)
	rid := newRecordID()
	u := testResultURL
	resp := expectReg(t, e.put(t, tokenFE, rid, bodyURL(t, rid, ownerA, &u)), http.StatusCreated, statusRegistered)
	if str(resp.ResultURL) != testResultURL {
		t.Fatalf("result_url %s", str(resp.ResultURL))
	}
	// Identical body: nothing changes.
	before := resp.UpdatedAt
	resp = expectReg(t, e.put(t, tokenFE, rid, bodyURL(t, rid, ownerA, &u)), http.StatusOK, statusRegistered)
	if resp.UpdatedAt != before || str(resp.ResultURL) != testResultURL {
		t.Fatalf("identical PUT changed the registration: %+v", resp)
	}
	// Same owner, other result_url: replaced; without one: removed.
	other := testResultURL + "-other"
	if resp = expectReg(t, e.put(t, tokenFE, rid, bodyURL(t, rid, ownerA, &other)), http.StatusOK, statusRegistered); str(resp.ResultURL) != other {
		t.Fatalf("result_url %s, want %s", str(resp.ResultURL), other)
	}
	if resp = expectReg(t, e.put(t, tokenFE, rid, bodyURL(t, rid, ownerA, nil)), http.StatusOK, statusRegistered); resp.ResultURL != nil {
		t.Fatalf("result_url %s after a PUT without it, want null", str(resp.ResultURL))
	}

	// Locked like the owner once deliver started (§2.4).
	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	atDeliver(t, e.pool, jobID, "running", "claimed")
	expectProblem(t, e.put(t, tokenFE, rid, bodyURL(t, rid, ownerA, &u)), http.StatusConflict, "registration-locked")

	// A tombstone keeps no result_url.
	rid2 := newRecordID()
	expectReg(t, e.put(t, tokenFE, rid2, bodyURL(t, rid2, ownerA, &u)), http.StatusCreated, statusRegistered)
	expectStatus(t, e.do(t, http.MethodDelete, path(rid2), tokenFE, nil), http.StatusNoContent)
	var stored *string
	if err := e.pool.QueryRow(bg, `SELECT result_url FROM registrations WHERE record_id = $1`, rid2).Scan(&stored); err != nil || stored != nil {
		t.Errorf("tombstone result_url %v (%v), want NULL", stored, err)
	}
}

// deliveries: one entry per sink in delivery order with its status, link
// and pickup; the top-level fields come from it, also before the job is
// delivered (§2.7).
func TestDeliveriesEntries(t *testing.T) {
	e, store := newResultEnv(t)
	rid := newRecordID()
	e.register(t, rid, "meeting-1", ownerA)
	jobID := seedJob(t, e.pool, rid)
	join(t, e.pool, jobID, rid)
	if j := e.job(t, rid); j.Deliveries == nil || len(j.Deliveries) != 0 {
		t.Fatalf("deliveries before validate %+v, want []", j.Deliveries)
	}

	atDeliver(t, e.pool, jobID, "running", "pending")
	exec(t, e.pool, `UPDATE jobs SET sink_profile = 'frontend-test,docs-test,docs-no-link' WHERE job_id = $1`, jobID)
	exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state, sent_at) VALUES ($1, 'frontend-test', 'sent', now())`, jobID)
	exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state, document_id) VALUES ($1, 'docs-test', 'sent', 'doc-7')`, jobID)
	exec(t, e.pool, `INSERT INTO step_items (job_id, step, item, state, failure_reason) VALUES ($1, 'deliver', 'docs-no-link', 'failed', 'rejected_by_sink')`, jobID)
	if err := store.PutSummary(bg, jobID, []byte(summaryText)); err != nil {
		t.Fatal(err)
	}

	j := e.job(t, rid)
	want := []sinkEntry{
		{SinkProfile: "frontend-test", State: "sent", SummaryAvailable: true},
		{SinkProfile: "docs-test", State: "sent", DocumentURL: ptr("https://docs.recap.test/docs/doc-7/")},
		{SinkProfile: "docs-no-link", State: "failed"},
	}
	if len(j.Deliveries) != len(want) {
		t.Fatalf("deliveries %+v", j.Deliveries)
	}
	for i, w := range want {
		g := j.Deliveries[i]
		if g.SinkProfile != w.SinkProfile || g.State != w.State || str(g.DocumentURL) != str(w.DocumentURL) ||
			g.SummaryAvailable != w.SummaryAvailable {
			t.Errorf("deliveries[%d] = %+v, want %+v", i, g, w)
		}
	}
	if str(j.DocumentURL) != "https://docs.recap.test/docs/doc-7/" || !j.SummaryAvailable || j.Outcome != nil {
		t.Errorf("top level %+v, want the Docs link, summary_available, no outcome yet", j)
	}
	// The summary is served as soon as the frontend sink is sent.
	expectStatus(t, e.do(t, http.MethodGet, path(rid)+"/summary", tokenFE, nil), http.StatusOK)
}

func ptr(s string) *string { return &s }

// Only a delivery record of a sink that uses the owner identity locks the
// owner (§2.4, sinks.md §1.8 rule 3): a job held owner_unresolved after its
// frontend sink was sent is resumed by a PUT; with two Docs sinks, once one
// has delivered, the owner stays locked.
func TestOwnerLockBySinkType(t *testing.T) {
	e, _ := newResultEnv(t)
	held := func(t *testing.T, list string, sent string) (string, string) {
		t.Helper()
		rid := newRecordID()
		e.register(t, rid, "meeting-1", ownerA)
		jobID := seedJob(t, e.pool, rid)
		join(t, e.pool, jobID, rid)
		atDeliver(t, e.pool, jobID, "held", "pending")
		hold(t, e.pool, jobID, "owner_unresolved")
		exec(t, e.pool, `UPDATE jobs SET sink_profile = $2 WHERE job_id = $1`, jobID, list)
		exec(t, e.pool, `INSERT INTO deliveries (job_id, sink_profile, state, sent_at) VALUES ($1, $2, 'sent', now())`,
			jobID, sent)
		return rid, jobID
	}
	t.Run("docs-and-frontend", func(t *testing.T) {
		rid, jobID := held(t, "docs-test,frontend-test", "frontend-test")
		expectReg(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB)), http.StatusOK, "matched")
		if row := getRow(t, e.pool, rid); row.sub != ownerB[0] {
			t.Fatal("owner not replaced")
		}
		var state string
		if err := e.pool.QueryRow(bg, `SELECT state FROM jobs WHERE job_id = $1`, jobID).Scan(&state); err != nil || state != "running" {
			t.Fatalf("job state %q (%v), want running (resumed)", state, err)
		}
	})
	t.Run("two-docs-one-sent", func(t *testing.T) {
		rid, _ := held(t, "docs-test,docs-no-link", "docs-test")
		expectProblem(t, e.put(t, tokenFE, rid, body(t, rid, "meeting-1", ownerB)), http.StatusConflict, "registration-locked")
		if row := getRow(t, e.pool, rid); row.sub != ownerA[0] {
			t.Fatal("owner replaced although locked")
		}
	})
}
