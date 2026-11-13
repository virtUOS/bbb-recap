package cleanup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/virtUOS/bbb-recap/internal/adapters/s3blob"
	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

var ctx = context.Background()

// F-01's owner and meeting (testdata/meetings/F-01/registration.json).
const (
	fixtureSub   = "fixture-sub-ada-0001"
	fixtureEmail = "ada.testperson@example.org"
	f01Record    = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	f01Meeting   = "fixture-meeting-01"
)

func newDeps(t *testing.T, store blob.Store) Deps {
	t.Helper()
	return Deps{Pool: dbtest.New(t), Blobs: store, Log: slog.New(slog.DiscardHandler)}
}

func newFS(t *testing.T) *blob.FS {
	t.Helper()
	s, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newS3 returns a store on a fresh bucket of a local MinIO, or skips:
//
//	TEST_S3_ENDPOINT=http://127.0.0.1:59022 TEST_S3_ACCESS_KEY=… TEST_S3_SECRET_KEY=…
func newS3(t *testing.T) blob.Store {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	access, secret := os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY")
	if endpoint == "" || access == "" || secret == "" {
		t.Skip("skipped: TEST_S3_ENDPOINT, TEST_S3_ACCESS_KEY and TEST_S3_SECRET_KEY not set")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(access, secret, ""),
		Secure:       u.Scheme == "https",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	bucket := "recap-test-" + hex.EncodeToString(b[:])
	if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	s, err := s3blob.New(s3blob.Config{Endpoint: endpoint, Bucket: bucket, AccessKeyID: access, SecretAccessKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// putBlob stores data as a blob of the job (a step output).
func putBlob(t *testing.T, store blob.Store, jobID string, data []byte) {
	t.Helper()
	sum := sha256.Sum256(data)
	if err := store.Put(ctx, jobID, hex.EncodeToString(sum[:]), int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

// register inserts F-01's registration with the fixture owner and returns
// its ID.
func register(t *testing.T, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status, expires_at,
			cancelled_at)
		VALUES ($1, 'moodle-main', $2, $3, $4, $5, now() + interval '14 days',
			CASE WHEN $5 = 'cancelled' THEN now() END)
		RETURNING registration_id`, f01Record, f01Meeting, fixtureSub, fixtureEmail, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// deliveredJob seeds F-01 (its blobs and rows), step outputs, a transcribe
// step_items row, a deliveries row and the registration with the owner,
// and puts the job at cleanup in state delivered, as deliver leaves it.
func deliveredJob(t *testing.T, d Deps) (jobID string, regID int64) {
	t.Helper()
	jobID = validatetest.Seed(t, d.Pool, d.Blobs, validatetest.Package{})
	putBlob(t, d.Blobs, jobID, []byte("# Summary\nfixture summary.md mentioning Ada Testperson\n"))
	putBlob(t, d.Blobs, jobID, []byte("WEBVTT\n\n00:00.000 --> 00:01.000\n<v Ada Testperson>fixture\n"))
	regID = register(t, d.Pool, "registered")
	exec(t, d.Pool, `
		UPDATE jobs SET state = 'delivered', current_step = 'cleanup', registration_id = $2,
			meeting_type = 'meeting-summary', sink_profile = 'docs-main', delivered_at = now()
		WHERE job_id = $1`, jobID, regID)
	exec(t, d.Pool, `UPDATE steps SET state = 'succeeded' WHERE job_id = $1 AND name <> 'cleanup'`, jobID)
	exec(t, d.Pool, `
		INSERT INTO step_items (job_id, item, state, attempt, output)
		VALUES ($1, 'trk-1', 'succeeded', 1, '{"result_sha256":"00","profile":"whisperx-main"}')`, jobID)
	exec(t, d.Pool, `
		INSERT INTO deliveries (job_id, sink_profile, state, document_id, sent_at)
		VALUES ($1, 'docs-main', 'sent', 'fixture-doc-0001', now())`, jobID)
	return jobID, regID
}

// listed reports whether the store lists a prefix for the job.
func listed(t *testing.T, store blob.Store, jobID string) bool {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(jobs, func(j blob.Job) bool { return j.ID == jobID })
}

// ownerTraces counts rows of every table whose text form contains the
// fixture owner's sub or email (CT-ID-03, database half).
func ownerTraces(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	tables := tableNames(t, pool)
	found := map[string]int{}
	for _, table := range tables {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` t
			WHERE t::text ILIKE '%' || $1 || '%' OR t::text ILIKE '%' || $2 || '%'`,
			fixtureSub, fixtureEmail).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			found[table] = n
		}
	}
	return found
}

func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// snapshot returns every row of every table as JSON, to compare the
// database before and after a repeated cleanup.
func snapshot(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	s := map[string][]string{}
	for _, table := range tableNames(t, pool) {
		rows, err := pool.Query(ctx, `SELECT row_to_json(t)::text FROM `+pgx.Identifier{table}.Sanitize()+` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		if s[table], err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// run runs the handler and then its Commit as the engine's completion
// does: in one transaction that holds the job row.
func run(t *testing.T, d Deps, jobID string) queue.Result {
	t.Helper()
	r := Handler(d)(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: 1})
	if r.Outcome != queue.OK {
		t.Fatalf("cleanup: outcome %s %s/%s, want ok", r.Outcome, r.ErrorClass, r.ErrorCode)
	}
	if err := commit(d, jobID, r); err != nil {
		t.Fatalf("cleanup commit: %v", err)
	}
	return r
}

func commit(d Deps, jobID string, r queue.Result) error {
	if r.Commit == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID); err != nil {
			return err
		}
		return r.Commit(ctx, tx)
	})
}

// fileRows counts the package_files rows of a package.
func fileRows(t *testing.T, pool *pgxpool.Pool, packageID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM package_files WHERE package_id = $1`, packageID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// userIDTraces counts rows of every table whose text form contains one of
// F-01's BBB user IDs (w_fixada0001, w_fixben0002 in its file names).
func userIDTraces(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	found := map[string]int{}
	for _, table := range tableNames(t, pool) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` t
			WHERE t::text LIKE '%w\_fix%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			found[table] = n
		}
	}
	return found
}

// CT-PRIV-01 (privacy.md §5.2) and the database half of CT-ID-03 on the
// filesystem backend.
func TestPRIV01FS(t *testing.T) {
	store := newFS(t)
	testPRIV01(t, store)
	// Nothing of the job is left on disk, not even an empty directory.
	entries, err := os.ReadDir(filepath.Join(store.Root(), "jobs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("blob root still holds %d job directories", len(entries))
	}
}

// CT-PRIV-01 on the S3 backend (MinIO).
func TestPRIV01S3(t *testing.T) { testPRIV01(t, newS3(t)) }

func testPRIV01(t *testing.T, store blob.Store) {
	d := newDeps(t, store)
	jobID, regID := deliveredJob(t, d)
	if !listed(t, store, jobID) || len(ownerTraces(t, d.Pool)) == 0 {
		t.Fatal("fixture: blobs or owner data missing before cleanup")
	}

	r := run(t, d, jobID)
	var out Output
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out != (Output{ASRCleanups: 0, Registration: RegistrationDeleted}) {
		t.Errorf("output = %+v", out)
	}
	if r.Commit == nil || r.Cancel {
		t.Error("cleanup must delete package_files in the completion and must not cancel")
	}

	// Blob store: the prefix is gone.
	if listed(t, store, jobID) {
		t.Error("blob prefix still listed")
	}
	if _, err := store.Get(ctx, jobID, validatetest.F01(t).Files[0].SHA256); err == nil {
		t.Error("a package blob is still readable")
	}
	// Postgres: no owner sub or email in any table; the registration is
	// gone and the job no longer references it.
	if tr := ownerTraces(t, d.Pool); len(tr) != 0 {
		t.Errorf("owner data still in Postgres: %v", tr)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM registrations WHERE registration_id = $1`, regID).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("registration rows = %d (err %v), want 0", n, err)
	}
	// Job metadata keeps IDs, states, times and codes, and the delivery
	// record (J1, S7).
	var state, step, docID string
	var reg *int64
	var delivered *time.Time
	if err := d.Pool.QueryRow(ctx, `
		SELECT j.state, j.current_step, j.registration_id, j.delivered_at, d.document_id
		FROM jobs j JOIN deliveries d USING (job_id) WHERE j.job_id = $1`, jobID).
		Scan(&state, &step, &reg, &delivered, &docID); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" || step != "cleanup" || reg != nil || delivered == nil || docID != "fixture-doc-0001" {
		t.Errorf("job after cleanup: state %s step %s registration %v delivered_at %v document %s",
			state, step, reg, delivered, docID)
	}

	// A repeated cleanup succeeds and changes nothing.
	before := snapshot(t, d.Pool)
	r = run(t, d, jobID)
	if err := json.Unmarshal(r.Output, &out); err != nil || out.Registration != RegistrationNone {
		t.Errorf("repeat: output %s (err %v)", r.Output, err)
	}
	after := snapshot(t, d.Pool)
	for table, rows := range before {
		if !slices.Equal(rows, after[table]) {
			t.Errorf("repeat changed table %s:\nbefore %v\nafter  %v", table, rows, after[table])
		}
	}
}

// A cancelled job keeps the tombstone: IDs and cancelled_at only, also when
// the tombstone still held owner fields.
func TestTombstoneSurvives(t *testing.T) {
	d := newDeps(t, newFS(t))
	jobID, regID := deliveredJob(t, d)
	exec(t, d.Pool, `DELETE FROM deliveries WHERE job_id = $1`, jobID)
	exec(t, d.Pool, `UPDATE registrations SET status = 'cancelled', cancelled_at = now()
		WHERE registration_id = $1`, regID)
	exec(t, d.Pool, `UPDATE jobs SET state = 'running', delivered_at = NULL, cancel_requested_at = now()
		WHERE job_id = $1`, jobID)

	r := run(t, d, jobID)
	if !strings.Contains(string(r.Output), `"registration":"tombstone"`) {
		t.Errorf("output %s", r.Output)
	}
	var rid, mid, fid, st string
	var sub, email *string
	var expires, cancelled *time.Time
	if err := d.Pool.QueryRow(ctx, `
		SELECT record_id, meeting_id, frontend_id, status, owner_sub, owner_email, expires_at, cancelled_at
		FROM registrations WHERE registration_id = $1`, regID).
		Scan(&rid, &mid, &fid, &st, &sub, &email, &expires, &cancelled); err != nil {
		t.Fatalf("tombstone gone: %v", err)
	}
	if rid != f01Record || mid != f01Meeting || fid != "moodle-main" || st != "cancelled" ||
		sub != nil || email != nil || expires != nil || cancelled == nil {
		t.Errorf("tombstone = %s %s %s %s sub %v email %v expires %v cancelled %v",
			rid, mid, fid, st, sub, email, expires, cancelled)
	}
	if tr := ownerTraces(t, d.Pool); len(tr) != 0 {
		t.Errorf("owner data still in Postgres: %v", tr)
	}
	if listed(t, d.Blobs, jobID) {
		t.Error("blob prefix still listed")
	}
	before := snapshot(t, d.Pool)
	run(t, d, jobID)
	if after := snapshot(t, d.Pool); !slices.Equal(before["registrations"], after["registrations"]) {
		t.Errorf("repeat changed the tombstone: %v → %v", before["registrations"], after["registrations"])
	}
}

// A job that a rerun replaced leaves the registration to the new job
// (architecture.md §5.4); an admin purge of the active job deletes it.
func TestRerunKeepsRegistration(t *testing.T) {
	d := newDeps(t, newFS(t))
	oldJob, regID := deliveredJob(t, d)
	newJob := queue.NewJobID()
	exec(t, d.Pool, `
		INSERT INTO jobs (job_id, package_id, rerun_of, state, current_step, config_hash, registration_id)
		SELECT $2, package_id, $1, 'running', 'validate', 'test', $3 FROM jobs WHERE job_id = $1`,
		oldJob, newJob, regID)
	// The old job is purging. Even if it still referenced the registration
	// (finish clears such a join), the rerun child makes it inactive.
	exec(t, d.Pool, `UPDATE jobs SET purge_requested_at = now() WHERE job_id = $1`, oldJob)

	r := run(t, d, oldJob)
	if !strings.Contains(string(r.Output), `"registration":"none"`) {
		t.Errorf("output %s", r.Output)
	}
	var sub string
	if err := d.Pool.QueryRow(ctx, `SELECT owner_sub FROM registrations WHERE registration_id = $1`, regID).
		Scan(&sub); err != nil || sub != fixtureSub {
		t.Errorf("registration of the new job: sub %q, err %v", sub, err)
	}
	// The new job still needs the package's files.
	pkg := packageOf(t, d.Pool, oldJob)
	if n := fileRows(t, d.Pool, pkg); n != len(validatetest.F01(t).Files) {
		t.Errorf("package_files rows after the old job's cleanup = %d, want all", n)
	}
	exec(t, d.Pool, `UPDATE jobs SET state = 'cleaned', outcome = 'purged', cleaned_at = now() WHERE job_id = $1`,
		oldJob)

	// Purge of the new (active) job deletes the registration.
	putBlob(t, d.Blobs, newJob, []byte("copied package blob"))
	exec(t, d.Pool, `UPDATE jobs SET registration_id = NULL WHERE job_id = $1`, oldJob)
	exec(t, d.Pool, `UPDATE jobs SET purge_requested_at = now(), current_step = 'cleanup' WHERE job_id = $1`, newJob)
	r = run(t, d, newJob)
	if !strings.Contains(string(r.Output), `"registration":"deleted"`) {
		t.Errorf("output %s", r.Output)
	}
	if tr := ownerTraces(t, d.Pool); len(tr) != 0 {
		t.Errorf("owner data still in Postgres: %v", tr)
	}
	if listed(t, d.Blobs, newJob) {
		t.Error("new job's prefix still listed")
	}
	// The last job of the package deletes its files.
	if n := fileRows(t, d.Pool, pkg); n != 0 {
		t.Errorf("package_files rows after the last job's cleanup = %d, want 0", n)
	}
}

// Under the engine: a cancelled job ends cleaned/cancelled, with the
// tombstone kept; a delivered job ends cleaned/delivered.
func TestEngineOutcomes(t *testing.T) {
	d := newDeps(t, newFS(t))
	delivered, _ := deliveredJob(t, d)
	exec(t, d.Pool, `UPDATE steps SET run_after = now() WHERE job_id = $1 AND name = 'cleanup'`, delivered)

	// A second job of the same package (F-01 can be seeded once per
	// database), with a blob of its own.
	cancelled := queue.NewJobID()
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		return queue.CreateJob(ctx, tx, queue.NewJob{JobID: cancelled, ConfigHash: "test",
			PackageID: packageOf(t, d.Pool, delivered)})
	}); err != nil {
		t.Fatal(err)
	}
	putBlob(t, d.Blobs, cancelled, []byte("fixture normalized meeting"))
	tomb := validatetest.Register(t, d.Pool, "other-record", "moodle-main", "m", "cancelled")
	exec(t, d.Pool, `UPDATE jobs SET registration_id = $2 WHERE job_id = $1`, cancelled, tomb)
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		return queue.Cancel(ctx, tx, queue.Actor{Kind: "client", ID: "moodle-main"}, cancelled)
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		queue.New(d.Pool, queue.Config{Owner: "w1", Slots: 2, Log: d.Log,
			Handlers: map[string]queue.Handler{Step: Handler(d)}}).Run(runCtx)
	}()
	defer func() { stop(); <-done }()

	want := map[string]string{delivered: "cleaned/delivered", cancelled: "cleaned/cancelled"}
	deadline := time.Now().Add(15 * time.Second)
	for id, w := range want {
		for {
			var got string
			if err := d.Pool.QueryRow(ctx, `SELECT state || '/' || coalesce(outcome, '') FROM jobs WHERE job_id = $1`,
				id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got == w {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s: %s, want %s", id, got, w)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if listed(t, d.Blobs, id) {
			t.Errorf("job %s: prefix still listed", id)
		}
	}
	var status string
	if err := d.Pool.QueryRow(ctx, `SELECT status FROM registrations WHERE registration_id = $1`, tomb).
		Scan(&status); err != nil || status != "cancelled" {
		t.Errorf("tombstone after cleaned/cancelled: %q, %v", status, err)
	}
	if tr := ownerTraces(t, d.Pool); len(tr) != 0 {
		t.Errorf("owner data still in Postgres: %v", tr)
	}
	// Both jobs of the package are cleaned: its files are gone.
	if n := fileRows(t, d.Pool, packageOf(t, d.Pool, delivered)); n != 0 {
		t.Errorf("package_files rows = %d, want 0", n)
	}
}

// TestPackageFiles: cleanup of a delivered, a cancelled and a purged job deletes the
// package's package_files rows (privacy.md §5.2). The package row keeps its
// IDs, file_count and total_bytes, no BBB user ID is left, and a second run
// changes nothing.
func TestPackageFiles(t *testing.T) {
	for _, outcome := range []string{"delivered", "cancelled", "purged"} {
		t.Run(outcome, func(t *testing.T) {
			d := newDeps(t, newFS(t))
			jobID, _ := deliveredJob(t, d)
			switch outcome {
			case "cancelled":
				exec(t, d.Pool, `UPDATE jobs SET state = 'running', delivered_at = NULL, cancel_requested_at = now()
					WHERE job_id = $1`, jobID)
			case "purged":
				exec(t, d.Pool, `UPDATE jobs SET purge_requested_at = now() WHERE job_id = $1`, jobID)
			}
			pkg := packageOf(t, d.Pool, jobID)
			f01 := validatetest.F01(t)
			if fileRows(t, d.Pool, pkg) == 0 || len(userIDTraces(t, d.Pool)) == 0 {
				t.Fatal("fixture: no package_files rows or BBB user IDs before cleanup")
			}

			run(t, d, jobID)
			if n := fileRows(t, d.Pool, pkg); n != 0 {
				t.Errorf("package_files rows = %d, want 0", n)
			}
			if tr := userIDTraces(t, d.Pool); len(tr) != 0 {
				t.Errorf("BBB user IDs still in Postgres: %v", tr)
			}
			var server, record, meeting string
			var count *int
			var size int64
			if err := d.Pool.QueryRow(ctx, `
				SELECT bbb_server_id, record_id, meeting_id, file_count, total_bytes
				FROM packages WHERE package_id = $1`, pkg).
				Scan(&server, &record, &meeting, &count, &size); err != nil {
				t.Fatalf("package row gone: %v", err)
			}
			if record != f01Record || meeting != f01Meeting || server == "" ||
				count == nil || *count != len(f01.Files) || size != f01.Totals.Size {
				t.Errorf("package row: server %q record %q meeting %q file_count %v total_bytes %d",
					server, record, meeting, count, size)
			}

			before := snapshot(t, d.Pool)
			run(t, d, jobID)
			after := snapshot(t, d.Pool)
			for table, rows := range before {
				if !slices.Equal(rows, after[table]) {
					t.Errorf("repeat changed table %s:\nbefore %v\nafter  %v", table, rows, after[table])
				}
			}
		})
	}
}

// TestPackageFilesNotVerified: verification fails, and the step runs again, when a package_files
// row is left. A trigger makes the delete a no-op.
func TestPackageFilesNotVerified(t *testing.T) {
	d := newDeps(t, newFS(t))
	jobID, _ := deliveredJob(t, d)
	exec(t, d.Pool, `CREATE FUNCTION keep_files() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'`)
	exec(t, d.Pool, `CREATE TRIGGER keep_files BEFORE DELETE ON package_files
		FOR EACH ROW EXECUTE FUNCTION keep_files()`)

	r := Handler(d)(ctx, queue.Run{JobID: jobID, Step: Step, Attempt: 1})
	if r.Outcome != queue.OK || r.Commit == nil {
		t.Fatalf("outcome %s, commit set %v", r.Outcome, r.Commit != nil)
	}
	if err := commit(d, jobID, r); !errors.Is(err, errNotClean) {
		t.Errorf("commit error = %v, want %v", err, errNotClean)
	}

	// Under the engine the step is released as a transient failure.
	exec(t, d.Pool, `UPDATE steps SET run_after = now() WHERE job_id = $1 AND name = 'cleanup'`, jobID)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		queue.New(d.Pool, queue.Config{Owner: "w1", Slots: 1, Log: d.Log,
			Handlers: map[string]queue.Handler{Step: Handler(d)}}).Run(runCtx)
	}()
	defer func() { stop(); <-done }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var state, code string
		var attempt int
		if err := d.Pool.QueryRow(ctx, `
			SELECT j.state, s.attempt, coalesce(s.last_error_code, '')
			FROM jobs j JOIN steps s ON s.job_id = j.job_id AND s.name = 'cleanup'
			WHERE j.job_id = $1`, jobID).Scan(&state, &attempt, &code); err != nil {
			t.Fatal(err)
		}
		if state == "cleaned" {
			t.Fatal("job cleaned although package_files rows are left")
		}
		if attempt >= 1 && code == "step_commit" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("step: attempt %d code %q, want a step_commit retry", attempt, code)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := fileRows(t, d.Pool, packageOf(t, d.Pool, jobID)); n == 0 {
		t.Error("fixture: trigger did not keep the rows")
	}
}

func packageOf(t *testing.T, pool *pgxpool.Pool, jobID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT package_id FROM jobs WHERE job_id = $1`, jobID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
