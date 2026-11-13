package db_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
)

var quiet = slog.New(slog.DiscardHandler)

func TestMigrateEmptyThenNoOp(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Open(t, dbtest.Empty(t))

	if ok, err := db.SchemaCompatible(ctx, pool); err != nil || ok {
		t.Fatalf("empty DB: SchemaCompatible = %v, %v; want false, nil", ok, err)
	}
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	before := ledger(t, pool)
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	after := ledger(t, pool)
	if len(before) == 0 || !strings.HasPrefix(before[len(before)-1], strconv.Itoa(db.LatestVersion())+" ") {
		t.Errorf("ledger %v does not end at the latest version %d", before, db.LatestVersion())
	}
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("second run changed the ledger:\n%v\n%v", before, after)
	}
	if ok, err := db.SchemaCompatible(ctx, pool); err != nil || !ok {
		t.Fatalf("migrated DB: SchemaCompatible = %v, %v; want true, nil", ok, err)
	}
}

func TestMigrateParallel(t *testing.T) {
	dsn := dbtest.Empty(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		pool := dbtest.Open(t, dsn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = db.Migrate(context.Background(), pool, quiet)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
}

func TestMigrateTamperedLedger(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET sha256 = repeat('0', 64) WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, quiet); !errors.Is(err, db.ErrChecksumMismatch) {
		t.Fatalf("Migrate = %v, want ErrChecksumMismatch", err)
	}
}

func TestNewerDatabaseIsCompatible(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version, sha256) VALUES (9999, repeat('a', 64))"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("Migrate on a newer DB: %v", err)
	}
	if ok, err := db.SchemaCompatible(ctx, pool); err != nil || !ok {
		t.Fatalf("SchemaCompatible = %v, %v; want true", ok, err)
	}
}

// enums lists every enum column with the exact values of docs/reference/README.md.
var enums = []struct {
	table, column string
	values        []string
}{
	{"jobs", "state", []string{"received", "running", "held", "failed", "delivered", "cleaned"}},
	{"jobs", "held_reason", []string{"invalid_metadata", "unknown_type", "unknown_sink", "no_registration",
		"owner_unresolved", "delivery_uncertain"}},
	{"jobs", "failure_reason", []string{"unsupported_audio_bridge", "events_mismatch", "manifest_metadata_mismatch",
		"retries_exhausted", "all_tracks_failed", "config_error", "rejected_by_provider", "rejected_by_sink",
		"internal_error"}},
	{"jobs", "outcome", []string{"delivered", "cancelled", "purged"}},
	{"jobs", "current_step", steps},
	{"steps", "name", steps},
	{"steps", "state", []string{"pending", "claimed", "succeeded", "retrying", "failed"}},
	{"step_items", "state", []string{"pending", "succeeded", "gap"}},
	{"step_items", "gap_reason", []string{"asr_failed", "asr_rejected_input", "asr_invalid_response", "track_unreadable"}},
	{"packages", "state", []string{"receiving", "complete", "committed"}},
	{"package_files", "role", []string{"events", "audio-microphone", "notes", "presentation"}},
	{"deliveries", "state", []string{"sending", "sent", "confirmed_by_admin"}},
	{"registrations", "status", []string{"registered", "cancelled"}},
	{"audit", "actor_kind", []string{"system", "admin", "client"}},
	{"audit", "action", []string{"retry", "release", "rerun", "purge", "erase", "ingest_commit", "job_state"}},
}

// The shared enum lists hold exactly the values of the enum columns.
func TestEnumLists(t *testing.T) {
	for _, c := range []struct {
		column string
		got    []string
	}{
		{"state", db.JobStates},
		{"held_reason", db.HeldReasons},
		{"failure_reason", db.FailureReasons},
	} {
		found := false
		for _, e := range enums {
			if e.table == "jobs" && e.column == c.column {
				found = true
				if !slices.Equal(c.got, e.values) {
					t.Errorf("jobs.%s: list %v, want %v", c.column, c.got, e.values)
				}
			}
		}
		if !found {
			t.Errorf("jobs.%s: no entry in the enums table", c.column)
		}
	}
}

var steps = []string{"validate", "normalize", "transcribe", "assemble", "summarize", "render", "deliver", "cleanup"}

const (
	jobID     = "01900000-0000-7000-8000-000000000001"
	packageID = "0000000000000000000000000000000000000000000000000000000000000001"
)

// seed inserts one row into every table, so each enum column can be updated.
func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`INSERT INTO registrations (record_id, frontend_id, meeting_id, status)
		 VALUES ('rec-1', 'pilos-fixture', 'fixture-meeting-01', 'registered')`,
		`INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
		 VALUES ('` + packageID + `', '` + jobID + `', 'bbb-fixture-a', 'rec-1', 'fixture-meeting-01', 'receiving', 0)`,
		`INSERT INTO package_files (package_id, path, role, size, sha256)
		 VALUES ('` + packageID + `', 'events.xml', 'events', 0, repeat('a', 64))`,
		`INSERT INTO jobs (job_id, package_id, state, config_hash) VALUES ('` + jobID + `', '` + packageID + `', 'received', 'h')`,
		`INSERT INTO steps (job_id, name, state) VALUES ('` + jobID + `', 'validate', 'pending')`,
		`INSERT INTO step_items (job_id, item, state) VALUES ('` + jobID + `', 'trk-1', 'pending')`,
		`INSERT INTO deliveries (job_id, sink_profile, state) VALUES ('` + jobID + `', 'docs-main', 'sending')`,
		`INSERT INTO audit (actor_kind, action, job_id) VALUES ('system', 'ingest_commit', '` + jobID + `')`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(context.Background(), s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func TestEnumChecks(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	seed(t, pool)
	for _, e := range enums {
		update := "UPDATE " + e.table + " SET " + e.column + " = $1"
		for _, v := range e.values {
			if _, err := pool.Exec(ctx, update, v); err != nil {
				t.Errorf("%s.%s = %q rejected: %v", e.table, e.column, v, err)
			}
		}
		for _, bad := range []string{"bogus", strings.ToUpper(e.values[0]), ""} {
			_, err := pool.Exec(ctx, update, bad)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" { // check_violation
				t.Errorf("%s.%s = %q: got %v, want a check violation", e.table, e.column, bad, err)
			}
		}
	}
}

func TestRecordIDIsCaseSensitive(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	for _, id := range []string{"rec-a", "REC-A"} {
		if _, err := pool.Exec(ctx, `INSERT INTO registrations (record_id, frontend_id, meeting_id, status)
			VALUES ($1, 'pilos-fixture', 'm', 'registered')`, id); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO registrations (record_id, frontend_id, meeting_id, status)
		VALUES ('rec-a', 'pilos-fixture', 'm', 'registered')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" { // unique_violation
		t.Fatalf("duplicate record_id: got %v, want a unique violation", err)
	}
}

func TestMigrationsAreEmbedded(t *testing.T) {
	if db.LatestVersion() < 1 {
		t.Fatalf("LatestVersion = %d, want >= 1", db.LatestVersion())
	}
}

// ledger returns every (version, applied_at) row of schema_migrations,
// ordered by version, as "<version> <applied_at>".
func ledger(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT version::text || ' ' || applied_at::text FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return list
}
