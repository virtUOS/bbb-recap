package admin

import (
	"errors"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// erase also deletes a frontend summary waiting for pickup (S10) and the
// result record of the job's registration (S9; privacy.md §5.4, ADR-014).
func TestEraseDeletesResults(t *testing.T) {
	e := newEnv(t)
	id := e.seed(t, "bbb-test-a")
	e.cleanAfterDelivery(t, id)
	// cleanup kept the registration as the result record: no owner.
	e.exec(t, `UPDATE registrations SET owner_sub = NULL, owner_email = NULL, expires_at = NULL,
		result_expires_at = now() + interval '7 days'
		WHERE registration_id = (SELECT registration_id FROM jobs WHERE job_id = $1)`, id)
	_, record := e.ids(t, id)
	results := e.blobs.(blob.Results)
	if err := results.PutSummary(bg, id, []byte("# Summary\n")); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM registrations WHERE record_id = $1`, record); n != 1 {
		t.Fatalf("seed: %d registrations", n)
	}

	decodeErased(t, e.post(id, "erase", "{}"))
	if _, err := results.GetSummary(bg, id); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("summary after erase: %v, want ErrNotFound", err)
	}
	if n := e.count(t, `SELECT count(*) FROM registrations WHERE record_id = $1`, record); n != 0 {
		t.Fatalf("result record left after erase")
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(bg, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
