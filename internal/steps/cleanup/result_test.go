package cleanup

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

const ttl = 7 * 24 * time.Hour

// resultDeps is newDeps on the filesystem backend with RECAP_RESULT_TTL 7d
// and the results store (ADR-014).
func resultDeps(t *testing.T) Deps {
	t.Helper()
	store := newFS(t)
	d := newDeps(t, store)
	d.Results, d.ResultTTL = store, ttl
	return d
}

func hasSummary(t *testing.T, d Deps, jobID string) bool {
	t.Helper()
	rc, err := d.Results.GetSummary(ctx, jobID)
	if errors.Is(err, blob.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	return true
}

// With RECAP_RESULT_TTL above 0, a delivered job's registration stays as the
// result record: IDs, no owner, result_expires_at = delivered_at + TTL
// (metadata-interface.md §2.7, privacy.md §5.1 step 3). The frontend
// summary outside the job prefix stays too. Nothing else changes from
// CT-PRIV-01: the prefix is gone and no owner value is left.
func TestResultRecordKept(t *testing.T) {
	d := resultDeps(t)
	jobID, regID := deliveredJob(t, d)
	if err := d.Results.PutSummary(ctx, jobID, []byte("# Summary\nfor pickup\n")); err != nil {
		t.Fatal(err)
	}

	r := run(t, d, jobID)
	var out Output
	if err := json.Unmarshal(r.Output, &out); err != nil || out.Registration != RegistrationResult {
		t.Fatalf("output %s (err %v), want registration %s", r.Output, err, RegistrationResult)
	}
	if tr := ownerTraces(t, d.Pool); len(tr) != 0 {
		t.Errorf("owner data still in Postgres: %v", tr)
	}
	if listed(t, d.Blobs, jobID) {
		t.Error("blob prefix still listed")
	}
	if !hasSummary(t, d, jobID) {
		t.Error("the frontend summary was deleted at cleanup")
	}
	var status, recordID string
	var expires *time.Time
	var resultExpires, delivered time.Time
	var ref *int64
	if err := d.Pool.QueryRow(ctx, `
		SELECT r.status, r.record_id, r.expires_at, r.result_expires_at, j.delivered_at, j.registration_id
		FROM registrations r JOIN jobs j ON j.job_id = $2 WHERE r.registration_id = $1`, regID, jobID).
		Scan(&status, &recordID, &expires, &resultExpires, &delivered, &ref); err != nil {
		t.Fatalf("result record: %v", err)
	}
	if status != "registered" || recordID != f01Record || expires != nil || ref == nil || *ref != regID ||
		!resultExpires.Equal(delivered.Add(ttl)) {
		t.Errorf("result record: status %s record %s expires %v result_expires %s (delivered %s) job ref %v",
			status, recordID, expires, resultExpires, delivered, ref)
	}

	// A repeated cleanup changes no row.
	before := snapshot(t, d.Pool)
	run(t, d, jobID)
	after := snapshot(t, d.Pool)
	for table, rows := range before {
		if !slices.Equal(rows, after[table]) {
			t.Errorf("repeat changed table %s", table)
		}
	}
}

// A purged job (admin purge, or the old job of a rerun) ends without the
// outcome delivered: its frontend summary is deleted and, as with
// RECAP_RESULT_TTL 0, its registration too (§2.5).
func TestPurgedJobNoResult(t *testing.T) {
	d := resultDeps(t)
	jobID, regID := deliveredJob(t, d)
	if err := d.Results.PutSummary(ctx, jobID, []byte("# Summary\n")); err != nil {
		t.Fatal(err)
	}
	exec(t, d.Pool, `UPDATE jobs SET purge_requested_at = now() WHERE job_id = $1`, jobID)
	r := run(t, d, jobID)
	var out Output
	if err := json.Unmarshal(r.Output, &out); err != nil || out.Registration != RegistrationDeleted {
		t.Fatalf("output %s (err %v)", r.Output, err)
	}
	if hasSummary(t, d, jobID) {
		t.Error("summary of a purged job kept")
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM registrations WHERE registration_id = $1`, regID).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("registration rows %d (err %v), want 0", n, err)
	}
}

// RECAP_RESULT_TTL=0: the registration goes at cleanup, as before ADR-014
// (§2.7), and a stray summary with it.
func TestResultTTLZero(t *testing.T) {
	d := resultDeps(t)
	d.ResultTTL = 0
	jobID, regID := deliveredJob(t, d)
	if err := d.Results.PutSummary(ctx, jobID, []byte("# Summary\n")); err != nil {
		t.Fatal(err)
	}
	r := run(t, d, jobID)
	var out Output
	if err := json.Unmarshal(r.Output, &out); err != nil || out.Registration != RegistrationDeleted {
		t.Fatalf("output %s (err %v)", r.Output, err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM registrations WHERE registration_id = $1`, regID).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("registration rows %d (err %v), want 0", n, err)
	}
	if hasSummary(t, d, jobID) {
		t.Error("summary kept with RECAP_RESULT_TTL 0")
	}
}
