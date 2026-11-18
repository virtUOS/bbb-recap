package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
)

// The describe migration (architecture.md §5.2) gives every existing job a
// describe row: pending before summarize, succeeded at or after it and for
// delivered and cleaned jobs.
func TestDescribeMigration(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Open(t, dbtest.Empty(t))
	names, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	// The schema before the describe step, recorded in the ledger as
	// Migrate would.
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (version integer PRIMARY KEY,
		sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range names { // sorted by Glob
		base := filepath.Base(name)
		if base == "0003_describe_step.sql" {
			found = true
			break
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		v, _ := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		sum := sha256.Sum256(b)
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, sha256) VALUES ($1, $2)`,
			v, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("no migration 0003_describe_step.sql")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id,
		state, total_bytes) VALUES (repeat('a', 64), '01900000-0000-7000-8000-000000000001', 'bbb-fixture-a', 'rec-1',
		'fixture-meeting-01', 'committed', 0)`); err != nil {
		t.Fatal(err)
	}
	jobs := []struct {
		id, state, step, want string
	}{
		{"01900000-0000-7000-8000-000000000001", "received", "validate", "pending"},
		{"01900000-0000-7000-8000-000000000002", "running", "assemble", "pending"},
		{"01900000-0000-7000-8000-000000000003", "held", "normalize", "pending"},
		{"01900000-0000-7000-8000-000000000004", "failed", "transcribe", "pending"},
		{"01900000-0000-7000-8000-000000000005", "running", "summarize", "succeeded"},
		{"01900000-0000-7000-8000-000000000006", "failed", "render", "succeeded"},
		{"01900000-0000-7000-8000-000000000007", "held", "deliver", "succeeded"},
		{"01900000-0000-7000-8000-000000000008", "delivered", "cleanup", "succeeded"},
		{"01900000-0000-7000-8000-000000000009", "cleaned", "cleanup", "succeeded"},
	}
	for _, j := range jobs {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (job_id, package_id, state, current_step, config_hash)
			VALUES ($1, repeat('a', 64), $2, $3, 'h')`, j.id, j.state, j.step); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM steps WHERE job_id = $1 AND name = 'describe'`,
			j.id).Scan(&state); err != nil || state != j.want {
			t.Errorf("job %s at %s: describe %q, %v; want %s", j.state, j.step, state, err, j.want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET current_step = 'describe' WHERE job_id = $1`, jobs[1].id); err != nil {
		t.Errorf("current_step describe rejected: %v", err)
	}
}
