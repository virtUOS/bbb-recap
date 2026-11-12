package retention

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

var ctx = context.Background()

const (
	stagingTTL = 72 * time.Hour
	metadata   = 180 * 24 * time.Hour
	// margin puts a row just inside or just outside a limit.
	margin = time.Minute
)

func newLoop(t *testing.T) *Loop {
	t.Helper()
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{Pool: dbtest.New(t), Blobs: store, Log: slog.New(slog.DiscardHandler),
		StagingTTL: stagingTTL, MetadataRetention: metadata}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// jobIDAt returns a UUIDv7 whose time is at.
func jobIDAt(at time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(at.UnixMilli()))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// putBlob stores one small blob under the job's prefix.
func putBlob(t *testing.T, l *Loop, jobID string) {
	t.Helper()
	data := []byte("fixture blob of " + jobID)
	sum := sha256.Sum256(data)
	if err := l.Blobs.Put(ctx, jobID, hex.EncodeToString(sum[:]), int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func listed(t *testing.T, l *Loop, jobID string) bool {
	t.Helper()
	jobs, err := l.Blobs.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return true
		}
	}
	return false
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

var seq int

// pkg inserts a packages row with one package_files row and returns the
// package ID. idle is how long ago the last activity was.
func pkg(t *testing.T, l *Loop, jobID, state string, idle time.Duration) string {
	t.Helper()
	seq++
	sum := sha256.Sum256([]byte(fmt.Sprint("package ", seq, jobID)))
	id := hex.EncodeToString(sum[:])
	exec(t, l.Pool, `
		INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes,
			last_activity_at, committed_at)
		VALUES ($1, $2, 'bbb-main', $3, 'fixture-meeting', $4, 1, now() - $5 * interval '1 microsecond',
			CASE WHEN $4 = 'committed' THEN now() END)`,
		id, jobID, "record-"+id[:12], state, idle.Microseconds())
	exec(t, l.Pool, `
		INSERT INTO package_files (package_id, path, role, size, sha256)
		VALUES ($1, 'events.xml', 'events', 1, $2)`, id, id)
	return id
}

// job inserts a job with its steps for a package, in state; a cleaned job
// gets cleaned_at = now() - age, an audit row, a delivery and a step item.
func job(t *testing.T, l *Loop, jobID, packageID, state string, age time.Duration) {
	t.Helper()
	exec(t, l.Pool, `
		INSERT INTO jobs (job_id, package_id, state, current_step, config_hash, outcome, cleaned_at)
		VALUES ($1, $2, $3, 'cleanup', 'test',
			CASE WHEN $3 = 'cleaned' THEN 'delivered' END,
			CASE WHEN $3 = 'cleaned' THEN now() - $4 * interval '1 microsecond' END)`,
		jobID, packageID, state, age.Microseconds())
	exec(t, l.Pool, `INSERT INTO steps (job_id, name, state) VALUES ($1, 'cleanup', 'succeeded')`, jobID)
	exec(t, l.Pool, `INSERT INTO step_items (job_id, item, state) VALUES ($1, 'trk-1', 'succeeded')`, jobID)
	exec(t, l.Pool, `INSERT INTO deliveries (job_id, sink_profile, state, document_id)
		VALUES ($1, 'docs-main', 'sent', 'fixture-doc')`, jobID)
	exec(t, l.Pool, `INSERT INTO audit (actor_kind, action, job_id, detail)
		VALUES ('system', 'job_state', $1, 'cleaned/delivered')`, jobID)
}

func TestStaging(t *testing.T) {
	l := newLoop(t)
	oldJob, youngJob, committedJob := queue.NewJobID(), queue.NewJobID(), queue.NewJobID()
	old := pkg(t, l, oldJob, "receiving", stagingTTL+margin)
	young := pkg(t, l, youngJob, "complete", stagingTTL-margin)
	committed := pkg(t, l, committedJob, "committed", stagingTTL+margin)
	job(t, l, committedJob, committed, "running", 0)
	for _, id := range []string{oldJob, youngJob, committedJob} {
		putBlob(t, l, id)
	}

	c := l.Sweep(ctx)
	if c.Staging != 1 {
		t.Errorf("staging count = %d, want 1", c.Staging)
	}
	if listed(t, l, oldJob) || count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = $1`, old) != 0 ||
		count(t, l.Pool, `SELECT count(*) FROM package_files WHERE package_id = $1`, old) != 0 {
		t.Error("idle staging package not fully deleted")
	}
	for _, p := range []struct{ id, job string }{{young, youngJob}, {committed, committedJob}} {
		if !listed(t, l, p.job) || count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = $1`, p.id) != 1 {
			t.Errorf("package %s was deleted", p.id)
		}
	}
}

func TestRegistrations(t *testing.T) {
	l := newLoop(t)
	reg := func(record, status string, expiresIn, cancelledAgo time.Duration) {
		exec(t, l.Pool, `
			INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status,
				expires_at, cancelled_at)
			VALUES ($1, 'moodle-main', 'fixture-meeting',
				CASE WHEN $2 = 'registered' THEN 'fixture-sub-ada-0001' END,
				CASE WHEN $2 = 'registered' THEN 'ada.testperson@example.org' END, $2,
				CASE WHEN $2 = 'registered' THEN now() + $3 * interval '1 microsecond' END,
				CASE WHEN $2 = 'cancelled' THEN now() - $4 * interval '1 microsecond' END)`,
			record, status, expiresIn.Microseconds(), cancelledAgo.Microseconds())
	}
	reg("expired", "registered", -margin, 0)
	reg("valid", "registered", margin, 0)
	reg("expired-matched", "registered", -margin, 0)  // a held job references it
	reg("expired-unjoined", "registered", -margin, 0) // a job of its record has not run validate yet
	reg("expired-cleaned", "registered", -margin, 0)  // only a cleaned job of its record (never joined)
	reg("tomb-old", "cancelled", 0, TombstoneRetention+margin)
	reg("tomb-young", "cancelled", 0, TombstoneRetention-margin)

	held := queue.NewJobID()
	job(t, l, held, pkg(t, l, held, "committed", 0), "held", 0)
	exec(t, l.Pool, `UPDATE jobs SET registration_id = (SELECT registration_id FROM registrations
		WHERE record_id = 'expired-matched') WHERE job_id = $1`, held)
	unjoined := queue.NewJobID()
	p := pkg(t, l, unjoined, "committed", 0)
	exec(t, l.Pool, `UPDATE packages SET record_id = 'expired-unjoined' WHERE package_id = $1`, p)
	job(t, l, unjoined, p, "received", 0)
	cleaned := queue.NewJobID()
	p = pkg(t, l, cleaned, "committed", 0)
	exec(t, l.Pool, `UPDATE packages SET record_id = 'expired-cleaned' WHERE package_id = $1`, p)
	job(t, l, cleaned, p, "cleaned", 0)

	c := l.Sweep(ctx)
	if c.Registrations != 2 || c.Tombstones != 1 {
		t.Errorf("counts = %+v, want 2 registrations and 1 tombstone", c)
	}
	rows, err := l.Pool.Query(ctx, `SELECT record_id FROM registrations ORDER BY record_id`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		left = append(left, r)
	}
	want := []string{"expired-matched", "expired-unjoined", "tomb-young", "valid"}
	if fmt.Sprint(left) != fmt.Sprint(want) {
		t.Errorf("registrations left = %v, want %v", left, want)
	}
}

func TestOrphans(t *testing.T) {
	l := newLoop(t)
	now := time.Now()
	old := now.Add(-OrphanAge - margin)
	ids := map[string]string{
		"no-row":       jobIDAt(old),
		"cleaned":      jobIDAt(old),
		"running":      jobIDAt(old),
		"staging":      jobIDAt(old), // a package still in staging, no job row
		"young-no-row": jobIDAt(now.Add(-OrphanAge + margin)),
	}
	for _, id := range ids {
		putBlob(t, l, id)
	}
	job(t, l, ids["cleaned"], pkg(t, l, ids["cleaned"], "committed", 0), "cleaned", 0)
	job(t, l, ids["running"], pkg(t, l, ids["running"], "committed", 0), "running", 0)
	pkg(t, l, ids["staging"], "receiving", 0)

	c := l.Sweep(ctx)
	if c.Orphans != 2 {
		t.Errorf("orphans = %d, want 2", c.Orphans)
	}
	for name, id := range ids {
		gone := name == "no-row" || name == "cleaned"
		if listed(t, l, id) == gone {
			t.Errorf("%s: listed = %v, want %v", name, !gone, !gone)
		}
	}
}

func TestCreated(t *testing.T) {
	at := time.UnixMilli(1761732000123)
	if got := Created(blob.Job{ID: jobIDAt(at)}); !got.Equal(at) {
		t.Errorf("Created(v7) = %v, want %v", got, at)
	}
	mod := time.Unix(1700000000, 0)
	v4 := "0190d6a2-3c4d-4e5f-8a6b-7c8d9e0f1a2b"
	if got := Created(blob.Job{ID: v4, Modified: mod}); !got.Equal(mod) {
		t.Errorf("Created(v4) = %v, want the modification time", got)
	}
}

func TestJobs(t *testing.T) {
	l := newLoop(t)
	oldJob, youngJob, rerunOld, rerunNew := queue.NewJobID(), queue.NewJobID(), queue.NewJobID(), queue.NewJobID()
	oldPkg := pkg(t, l, oldJob, "committed", 0)
	job(t, l, oldJob, oldPkg, "cleaned", metadata+margin)
	youngPkg := pkg(t, l, youngJob, "committed", 0)
	job(t, l, youngJob, youngPkg, "cleaned", metadata-margin)
	// A rerun shares the package: the old job goes, the package stays.
	shared := pkg(t, l, rerunOld, "committed", 0)
	job(t, l, rerunOld, shared, "cleaned", metadata+margin)
	job(t, l, rerunNew, shared, "running", 0)
	exec(t, l.Pool, `UPDATE jobs SET rerun_of = $1 WHERE job_id = $2`, rerunOld, rerunNew)

	c := l.Sweep(ctx)
	if c.Jobs != 2 {
		t.Errorf("jobs = %d, want 2", c.Jobs)
	}
	for _, id := range []string{oldJob, rerunOld} {
		for _, table := range []string{"jobs", "steps", "step_items", "deliveries", "audit"} {
			if n := count(t, l.Pool, `SELECT count(*) FROM `+table+` WHERE job_id = $1`, id); n != 0 {
				t.Errorf("job %s: %d rows left in %s", id, n, table)
			}
		}
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = $1`, oldPkg); n != 0 {
		t.Error("package of the deleted job is left")
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM package_files WHERE package_id = $1`, oldPkg); n != 0 {
		t.Error("package_files of the deleted job are left")
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = ANY($1)`,
		[]string{youngPkg, shared}); n != 2 {
		t.Errorf("packages kept = %d, want 2", n)
	}
	for _, id := range []string{youngJob, rerunNew} {
		if n := count(t, l.Pool, `SELECT count(*) FROM jobs WHERE job_id = $1`, id); n != 1 {
			t.Errorf("job %s was deleted", id)
		}
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM jobs WHERE job_id = $1 AND rerun_of IS NULL`, rerunNew); n != 1 {
		t.Error("rerun_of of the new job should be NULL after the old job is gone")
	}
}

// Two loops at once (two workers) delete everything exactly once and fail
// nowhere.
func TestConcurrentLoops(t *testing.T) {
	l := newLoop(t)
	var log bytes.Buffer
	var mu sync.Mutex
	l.Log = slog.New(slog.NewJSONHandler(&lockedWriter{w: &log, mu: &mu}, nil))
	const n = 20
	old := time.Now().Add(-OrphanAge - margin)
	for i := range n {
		staged := queue.NewJobID()
		pkg(t, l, staged, "receiving", stagingTTL+margin)
		putBlob(t, l, staged)
		orphan := jobIDAt(old)
		putBlob(t, l, orphan)
		cleaned := queue.NewJobID()
		job(t, l, cleaned, pkg(t, l, cleaned, "committed", 0), "cleaned", metadata+margin)
		exec(t, l.Pool, `
			INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status, expires_at)
			VALUES ($1, 'moodle-main', 'm', 'fixture-sub-ada-0001', 'ada.testperson@example.org', 'registered',
				now() - interval '1 minute')`, fmt.Sprint("expired-", i))
		exec(t, l.Pool, `
			INSERT INTO registrations (record_id, frontend_id, meeting_id, status, cancelled_at)
			VALUES ($1, 'moodle-main', 'm', 'cancelled', now() - interval '31 days')`, fmt.Sprint("tomb-", i))
	}

	var wg sync.WaitGroup
	counts := make([]Counts, 2)
	for i := range counts {
		wg.Go(func() { counts[i] = l.Sweep(ctx) })
	}
	wg.Wait()
	total := Counts{}
	for _, c := range counts {
		total.Staging += c.Staging
		total.Registrations += c.Registrations
		total.Tombstones += c.Tombstones
		total.Orphans += c.Orphans
		total.Jobs += c.Jobs
	}
	// Orphans may be counted by both loops (deleting a gone prefix is
	// success); everything else exactly once.
	if total.Staging != n || total.Registrations != n || total.Tombstones != n || total.Jobs != n ||
		total.Orphans < n {
		t.Errorf("total counts = %+v", total)
	}
	for _, table := range []string{"packages", "package_files", "jobs", "registrations", "audit", "steps"} {
		if c := count(t, l.Pool, `SELECT count(*) FROM `+table); c != 0 {
			t.Errorf("%s: %d rows left", table, c)
		}
	}
	if jobs, err := l.Blobs.ListJobs(ctx); err != nil || len(jobs) != 0 {
		t.Errorf("prefixes left: %v (err %v)", jobs, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bytes.Contains(log.Bytes(), []byte(`"level":"WARN"`)) {
		t.Errorf("a sweeper failed:\n%s", log.String())
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// Run sweeps at once and returns promptly when its context ends.
func TestRunStops(t *testing.T) {
	l := newLoop(t)
	staged := queue.NewJobID()
	pkg(t, l, staged, "receiving", stagingTTL+margin)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { l.Run(runCtx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for count(t, l.Pool, `SELECT count(*) FROM packages`) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first pass did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1 s after its context ended")
	}
}

// A committed package whose jobs are all gone is deleted in a later pass,
// e.g. after two workers deleted both jobs of a rerun pair at the same
// time and each still saw the other's job. A committed package
// with a job and a staging package are kept.
func TestLeftoverPackage(t *testing.T) {
	l := newLoop(t)
	leftover := pkg(t, l, queue.NewJobID(), "committed", 0) // its jobs are gone
	withJob := queue.NewJobID()
	kept := pkg(t, l, withJob, "committed", 0)
	job(t, l, withJob, kept, "cleaned", metadata-margin)
	staging := pkg(t, l, queue.NewJobID(), "receiving", 0)

	c := l.Sweep(ctx)
	if c.Packages != 1 {
		t.Errorf("packages = %d, want 1", c.Packages)
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = $1`, leftover); n != 0 {
		t.Error("committed package without a job is left")
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM package_files WHERE package_id = $1`, leftover); n != 0 {
		t.Error("its package_files are left")
	}
	if n := count(t, l.Pool, `SELECT count(*) FROM packages WHERE package_id = ANY($1)`,
		[]string{kept, staging}); n != 2 {
		t.Errorf("packages kept = %d, want 2", n)
	}
}

// Both jobs of a rerun pair expire at once and are deleted by two loops
// running at the same time: the shared package goes too.
func TestRerunPairConcurrent(t *testing.T) {
	for range 5 {
		l := newLoop(t)
		first, second := queue.NewJobID(), queue.NewJobID()
		shared := pkg(t, l, first, "committed", 0)
		job(t, l, first, shared, "cleaned", metadata+margin)
		job(t, l, second, shared, "cleaned", metadata+margin)
		exec(t, l.Pool, `UPDATE jobs SET rerun_of = $1 WHERE job_id = $2`, first, second)

		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { l.Sweep(ctx) })
		}
		wg.Wait()
		if n := count(t, l.Pool, `SELECT count(*) FROM jobs`) + count(t, l.Pool, `SELECT count(*) FROM packages`); n != 0 {
			t.Fatalf("%d jobs or packages rows left", n)
		}
	}
}
