package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/manifest"
)

const (
	f01Record    = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	testdataRoot = "../../testdata"
	configHash   = "0000000000000000000000000000000000000000000000000000000000000000"
)

func testdata(parts ...string) string {
	return filepath.Join(append([]string{testdataRoot}, parts...)...)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func token(t *testing.T, client string) string {
	t.Helper()
	return strings.TrimSuffix(string(readFile(t, testdata("secrets", "clients", client))), "\n")
}

// pkg is a fixture package: manifest bytes, package_id and the file bytes
// by hash.
type pkg struct {
	data  []byte
	id    string
	m     manifest.Manifest
	files map[string][]byte
}

// f01 loads F-01's golden manifest and raw files. mutate, if set, changes
// the decoded manifest, which is then re-canonicalized.
func f01(t *testing.T, manifestPath string, mutate func(map[string]any)) pkg {
	t.Helper()
	data := readFile(t, manifestPath)
	if mutate != nil {
		var v map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		mutate(v)
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if data, err = manifest.Canonicalize(raw); err != nil {
			t.Fatal(err)
		}
	}
	p := pkg{data: data, id: manifest.PackageID(data), files: map[string][]byte{}}
	if err := json.Unmarshal(data, &p.m); err != nil {
		t.Fatal(err)
	}
	raw := testdata("meetings", "F-01", "raw", f01Record)
	for _, f := range p.m.Files {
		b, err := os.ReadFile(filepath.Join(raw, filepath.FromSlash(f.Path)))
		if errors.Is(err, os.ErrNotExist) {
			continue // duplicate-hash case: the copy is not in the raw dir; checked below
		}
		if err != nil {
			t.Fatal(err)
		}
		p.files[f.SHA256] = b
	}
	for _, f := range p.m.Files {
		if p.files[f.SHA256] == nil {
			t.Fatalf("fixture file with hash %s not found", f.SHA256)
		}
	}
	return p
}

func golden(t *testing.T) pkg {
	return f01(t, testdata("meetings", "F-01", "expected", "manifest.json"), nil)
}

// env is a service on a fresh database and blob directory.
type env struct {
	t     *testing.T
	svc   *Service
	pool  *pgxpool.Pool
	blobs *blob.FS
	url   string
	logs  *syncBuffer
	token string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newEnv(t *testing.T) *env {
	return newEnvWith(t, nil)
}

// newEnvWith wraps the blob store with wrap, if set.
func newEnvWith(t *testing.T, wrap func(blob.Store) blob.Store) *env {
	t.Helper()
	pool := dbtest.New(t)
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := httpapi.LoadCredentials(testdata("secrets", "clients"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	var store blob.Store = fs
	if wrap != nil {
		store = wrap(fs)
	}
	svc := New(Config{
		Pool:       pool,
		Blobs:      store,
		Creds:      creds,
		MetaPrefix: "recap",
		ConfigHash: configHash,
		Log:        slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	mux := http.NewServeMux()
	svc.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		// Logs carry IDs only: no manifest paths (they hold BBB user IDs), no tokens.
		for _, bad := range []string{"microphone-", "FIXTURE-NOT-A-SECRET", "w_fix"} {
			if strings.Contains(logs.String(), bad) {
				t.Errorf("log contains %q:\n%s", bad, logs.String())
			}
		}
	})
	return &env{t: t, svc: svc, pool: pool, blobs: fs, url: srv.URL, logs: logs,
		token: token(t, "bbb_server.bbb-fixture-a")}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	return v
}

func (r response) problem(t *testing.T) string {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	return strings.TrimPrefix(r.json(t)["type"].(string), httpapi.ProblemTypePrefix)
}

// do sends a request with the env's token, unless headers set Authorization.
// A nil body sends no body; body length -1 forces chunked encoding.
func (e *env) do(method, path string, body io.Reader, length int64, headers ...string) response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+e.token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, b}
}

func pkgPath(id string) string { return "/api/v1/ingest/packages/" + id }

func (e *env) putManifest(p pkg) response {
	e.t.Helper()
	return e.do(http.MethodPut, pkgPath(p.id), bytes.NewReader(p.data), int64(len(p.data)),
		"Content-Type", "application/json")
}

func (e *env) putFile(p pkg, hash string, data []byte, headers ...string) response {
	e.t.Helper()
	return e.do(http.MethodPut, pkgPath(p.id)+"/files/"+hash, bytes.NewReader(data), int64(len(data)), headers...)
}

func (e *env) commit(p pkg) response {
	e.t.Helper()
	return e.do(http.MethodPost, pkgPath(p.id)+"/commit", strings.NewReader("{}"), 2,
		"Content-Type", "application/json")
}

func (e *env) uploadAll(p pkg) {
	e.t.Helper()
	for hash, data := range p.files {
		if r := e.putFile(p, hash, data); r.status != http.StatusNoContent {
			e.t.Fatalf("upload %s: status %d %s", hash, r.status, r.body)
		}
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func wantStatus(t *testing.T, what string, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d; body %s", what, r.status, status, r.body)
	}
}

func distinctSize(p pkg) int64 {
	var n int64
	for _, b := range p.files {
		n += int64(len(b))
	}
	return n
}

// Idempotency table, rows 1 and 3 (architecture.md §4.1): new package; commit
// or GET of a committed package gives the identical ack, also for a cleaned
// job, and no new job.
func TestNewPackageAndCommit(t *testing.T) {
	e := newEnv(t)
	p := golden(t)

	r := e.putManifest(p)
	wantStatus(t, "manifest PUT", r, http.StatusCreated)
	st := r.json(t)
	if st["state"] != "receiving" || len(st["missing"].([]any)) != len(p.files) ||
		st["received_size"] != 0.0 || st["expected_size"] != float64(distinctSize(p)) {
		t.Fatalf("status = %s", r.body)
	}
	if r.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", r.header.Get("Content-Type"))
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("%d jobs before commit, want 0", n)
	}

	// The manifest is stored in the job prefix under its own hash.
	var jobID string
	if err := e.pool.QueryRow(context.Background(), `SELECT job_id::text FROM packages WHERE package_id = $1`,
		p.id).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	rc, err := e.blobs.Get(context.Background(), jobID, p.id)
	if err != nil {
		t.Fatalf("manifest blob: %v", err)
	}
	stored, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(stored, p.data) {
		t.Fatalf("manifest blob differs (err %v)", err)
	}

	e.uploadAll(p)
	r = e.putManifest(p)
	wantStatus(t, "manifest re-check", r, http.StatusOK)
	if st := r.json(t); st["state"] != "complete" || len(st["missing"].([]any)) != 0 ||
		st["received_size"] != st["expected_size"] {
		t.Fatalf("status = %s", r.body)
	}

	r = e.commit(p)
	wantStatus(t, "commit", r, http.StatusOK)
	ack := r.json(t)
	if ack["state"] != "committed" || ack["job_id"] != jobID || ack["record_id"] != p.m.RecordID ||
		ack["file_count"] != float64(p.m.Totals.FileCount) || ack["size"] != float64(p.m.Totals.Size) ||
		ack["package_id"] != p.id || ack["committed_at"] == "" {
		t.Fatalf("ack = %s", r.body)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE job_id = $1 AND state = 'received' AND current_step = 'validate'
		AND config_hash = $2 AND package_id = $3`, jobID, configHash, p.id); n != 1 {
		t.Fatalf("job rows = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM steps WHERE job_id = $1 AND state = 'pending'`, jobID); n != 8 {
		t.Fatalf("steps = %d, want 8", n)
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE job_id = $1 AND action = 'ingest_commit'
		AND actor_kind = 'client' AND actor_id = 'bbb_server.bbb-fixture-a'`, jobID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	// Repeated commit and GET: the identical ack.
	again := e.commit(p)
	wantStatus(t, "repeated commit", again, http.StatusOK)
	if !bytes.Equal(again.body, r.body) {
		t.Fatalf("repeated ack %s, first %s", again.body, r.body)
	}
	g := e.do(http.MethodGet, pkgPath(p.id), nil, 0)
	wantStatus(t, "GET", g, http.StatusOK)
	gs := g.json(t)
	for k, v := range ack {
		if gs[k] != v {
			t.Errorf("GET %s = %v, ack %v", k, gs[k], v)
		}
	}
	m := e.putManifest(p)
	wantStatus(t, "manifest PUT after commit", m, http.StatusOK)
	if m.json(t)["committed_at"] != ack["committed_at"] {
		t.Errorf("manifest PUT after commit: %s", m.body)
	}

	// Even a cleaned job gives the same ack and no new job.
	if _, err := e.pool.Exec(context.Background(), `UPDATE jobs SET state = 'cleaned', outcome = 'delivered'`); err != nil {
		t.Fatal(err)
	}
	again = e.commit(p)
	if again.status != http.StatusOK || !bytes.Equal(again.body, r.body) {
		t.Fatalf("commit of cleaned job: %d %s", again.status, again.body)
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 1 {
		t.Fatalf("%d jobs, want 1", n)
	}

	// Uploads after the commit are refused.
	for hash, data := range p.files {
		if r := e.putFile(p, hash, data); r.status != http.StatusConflict || r.problem(t) != "package-committed" {
			t.Fatalf("upload after commit: %d %s", r.status, r.body)
		}
		break
	}

	// After cleanup deleted the package_files rows (privacy.md
	// §5.2), GET, manifest PUT and commit still give the original ack.
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE packages SET file_count = (SELECT count(*) FROM package_files WHERE package_id = $1)
		WHERE package_id = $1`, p.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM package_files WHERE package_id = $1`, p.id); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what string
		r    response
	}{
		{"GET after cleanup", e.do(http.MethodGet, pkgPath(p.id), nil, 0)},
		{"manifest PUT after cleanup", e.putManifest(p)},
		{"commit after cleanup", e.commit(p)},
	} {
		wantStatus(t, c.what, c.r, http.StatusOK)
		got := c.r.json(t)
		for _, k := range []string{"package_id", "state", "job_id", "record_id", "file_count", "size", "committed_at"} {
			if got[k] != ack[k] {
				t.Errorf("%s: %s = %v, ack %v", c.what, k, got[k], ack[k])
			}
		}
	}
	// An upload stays 409 package-committed (bbb-exporter.md §6.2).
	for hash, data := range p.files {
		if r := e.putFile(p, hash, data); r.status != http.StatusConflict || r.problem(t) != "package-committed" {
			t.Fatalf("upload after cleanup: %d %s", r.status, r.body)
		}
		break
	}
}

// Idempotency table, row 2: same package_id, not committed → resume; a
// verified file is not stored again.
func TestResume(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)

	var first string
	for h := range p.files {
		first = h
		break
	}
	wantStatus(t, "upload", e.putFile(p, first, p.files[first]), http.StatusNoContent)

	r := e.putManifest(p)
	wantStatus(t, "manifest PUT again", r, http.StatusOK)
	st := r.json(t)
	if st["state"] != "receiving" || len(st["missing"].([]any)) != len(p.files)-1 ||
		st["received_size"] != float64(len(p.files[first])) {
		t.Fatalf("status = %s", r.body)
	}
	for _, h := range st["missing"].([]any) {
		if h == first {
			t.Fatalf("missing lists the uploaded file")
		}
	}
	// Already verified: 200, the body is not read.
	wantStatus(t, "repeat upload", e.putFile(p, first, p.files[first]), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM packages`); n != 1 {
		t.Fatalf("%d packages, want 1", n)
	}

	r = e.commit(p)
	wantStatus(t, "early commit", r, http.StatusConflict)
	if r.problem(t) != "package-incomplete" || len(r.json(t)["missing"].([]any)) != len(p.files)-1 {
		t.Fatalf("early commit: %s", r.body)
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("%d jobs after a refused commit", n)
	}
}

// Idempotency table, row 4: another package_id for the same record → 409,
// before and after the commit (V-I06).
func TestRecordAlreadyIngested(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	other := f01(t, testdata("meetings", "F-01", "expected", "manifest.json"), func(m map[string]any) {
		m["created_at"] = "2025-10-29T11:10:00Z"
	})
	wantStatus(t, "first", e.putManifest(p), http.StatusCreated)
	r := e.putManifest(other)
	wantStatus(t, "second", r, http.StatusConflict)
	if r.problem(t) != "record-already-ingested" {
		t.Fatalf("problem %s", r.body)
	}
	e.uploadAll(p)
	wantStatus(t, "commit", e.commit(p), http.StatusOK)
	wantStatus(t, "after commit", e.putManifest(other), http.StatusConflict)
	if n := e.count(`SELECT count(*) FROM packages`); n != 1 {
		t.Fatalf("%d packages, want 1", n)
	}
}

// V-I07: not opted in → 422 and nothing stored (no row, no blob).
func TestNotOptedInStoresNothing(t *testing.T) {
	e := newEnv(t)
	p := f01(t, testdata("ingest", "exchanges", "bodies", "M-12.not-opted-in.manifest.json"), nil)
	r := e.putManifest(p)
	wantStatus(t, "manifest PUT", r, http.StatusUnprocessableEntity)
	if r.problem(t) != "not-opted-in" {
		t.Fatalf("problem %s", r.body)
	}
	if n := e.count(`SELECT count(*) FROM packages`) + e.count(`SELECT count(*) FROM package_files`); n != 0 {
		t.Fatalf("%d rows stored", n)
	}
	jobs, err := e.blobs.ListJobs(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatalf("blob jobs %v (err %v), want none", jobs, err)
	}
	wantStatus(t, "GET", e.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusNotFound)
}

// The ack is never sent before the job commit: a failure inside the commit
// transaction gives no ack, no jobs row, no audit row and no committed
// package. The next commit works.
func TestCommitFailureSendsNoAck(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	e.uploadAll(p)

	e.svc.beforeCommit = func(ctx context.Context, tx pgx.Tx) error {
		// The rows are written inside the transaction at this point.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil || n != 1 {
			t.Errorf("jobs inside the transaction: %d (err %v)", n, err)
		}
		return errors.New("injected failure")
	}
	r := e.commit(p)
	wantStatus(t, "failing commit", r, http.StatusServiceUnavailable)
	if r.problem(t) != "unavailable" || r.header.Get("Retry-After") != "30" {
		t.Fatalf("failing commit: %v %s", r.header, r.body)
	}
	if _, has := r.json(t)["job_id"]; has {
		t.Fatalf("failing commit sent an ack: %s", r.body)
	}
	if n := e.count(`SELECT count(*) FROM jobs`) + e.count(`SELECT count(*) FROM steps`) +
		e.count(`SELECT count(*) FROM audit`); n != 0 {
		t.Fatalf("%d job, step or audit rows after the failed commit", n)
	}
	if n := e.count(`SELECT count(*) FROM packages WHERE state = 'committed' OR committed_at IS NOT NULL`); n != 0 {
		t.Fatalf("package committed after the failed commit")
	}

	e.svc.beforeCommit = nil
	wantStatus(t, "commit", e.commit(p), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM jobs`); n != 1 {
		t.Fatalf("%d jobs, want 1", n)
	}
}

// Concurrent manifest PUTs with the same package_id give one row.
func TestConcurrentManifestPUT(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	const n = 10
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodPut, e.url+pkgPath(p.id), bytes.NewReader(p.data))
			req.Header.Set("Authorization", "Bearer "+e.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("status %d", s)
		}
	}
	if created != 1 {
		t.Errorf("%d answers 201, want 1 (%v)", created, statuses)
	}
	if c := e.count(`SELECT count(*) FROM packages`); c != 1 {
		t.Fatalf("%d package rows, want 1", c)
	}
	if c := e.count(`SELECT count(*) FROM package_files`); c != len(p.m.Files) {
		t.Fatalf("%d package_files rows, want %d", c, len(p.m.Files))
	}
	jobs, err := e.blobs.ListJobs(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("blob job prefixes %v (err %v), want 1", jobs, err)
	}
}

// More than MaxUploads concurrent uploads → 429 rate-limited, Retry-After 30.
func TestUploadLimit(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	var hash string
	for h := range p.files {
		hash = h
		break
	}
	data := p.files[hash]

	writers := make([]*io.PipeWriter, MaxUploads)
	var wg sync.WaitGroup
	for i := range writers {
		pr, pw := io.Pipe()
		writers[i] = pw
		req, _ := http.NewRequest(http.MethodPut, e.url+pkgPath(p.id)+"/files/"+hash, pr)
		req.ContentLength = int64(len(data))
		req.Header.Set("Authorization", "Bearer "+e.token)
		wg.Go(func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		})
	}
	// Each upload has sent its headers and the first byte; wait until the
	// server holds all slots (the handler takes its slot before it reads
	// the body, and none of them can answer before its body ends).
	for _, pw := range writers {
		if _, err := pw.Write(data[:1]); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.svc.uploads) < MaxUploads {
		if time.Now().After(deadline) {
			t.Fatalf("server holds %d upload slots, want %d", len(e.svc.uploads), MaxUploads)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r := e.putFile(p, hash, data)
	wantStatus(t, "upload over the limit", r, http.StatusTooManyRequests)
	if r.problem(t) != "rate-limited" || r.header.Get("Retry-After") != "30" {
		t.Fatalf("429: %v %s", r.header, r.body)
	}
	for _, pw := range writers {
		_, _ = pw.Write(data[1:])
		_ = pw.Close()
	}
	wg.Wait()
	// The slots are free again.
	if r := e.putFile(p, hash, data); r.status != http.StatusOK && r.status != http.StatusNoContent {
		t.Fatalf("upload after the burst: %d %s", r.status, r.body)
	}
}

// A manifest body over 1 MiB → 413 package-too-large, with and without
// Content-Length.
func TestManifestTooLarge(t *testing.T) {
	e := newEnv(t)
	big := bytes.Repeat([]byte(" "), manifest.MaxManifestSize+1)
	id := manifest.PackageID(big)
	for _, length := range []int64{int64(len(big)), -1} {
		r := e.do(http.MethodPut, pkgPath(id), bytes.NewReader(big), length)
		wantStatus(t, "big manifest", r, http.StatusRequestEntityTooLarge)
		if r.problem(t) != "package-too-large" {
			t.Fatalf("problem %s", r.body)
		}
	}
	if n := e.count(`SELECT count(*) FROM packages`); n != 0 {
		t.Fatalf("%d packages", n)
	}
}

func digest(alg string, sum []byte) string {
	return alg + "=:" + base64.StdEncoding.EncodeToString(sum) + ":"
}

// Content-Digest: sha-256 must match the path hash; other algorithms are
// ignored.
func TestContentDigest(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	var hashes []string
	for h := range p.files {
		hashes = append(hashes, h)
	}

	wrong := sha256.Sum256([]byte("other"))
	r := e.putFile(p, hashes[0], p.files[hashes[0]], "Content-Digest", digest("sha-256", wrong[:]))
	wantStatus(t, "wrong sha-256 digest", r, http.StatusUnprocessableEntity)
	if r.problem(t) != "file-hash-mismatch" {
		t.Fatalf("problem %s", r.body)
	}
	r = e.putFile(p, hashes[0], p.files[hashes[0]], "Content-Digest", "sha-256=not-a-byte-sequence")
	wantStatus(t, "unreadable sha-256 digest", r, http.StatusUnprocessableEntity)

	r = e.putFile(p, hashes[0], p.files[hashes[0]], "Content-Digest", digest("sha-512", []byte("anything")))
	wantStatus(t, "sha-512 digest is ignored", r, http.StatusNoContent)

	right, _ := hex.DecodeString(hashes[1])
	r = e.putFile(p, hashes[1], p.files[hashes[1]],
		"Content-Digest", digest("sha-512", []byte("x"))+", "+digest("sha-256", right))
	wantStatus(t, "matching sha-256 digest", r, http.StatusNoContent)
}

func TestUploadErrors(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	var hash string
	for h := range p.files {
		hash = h
		break
	}
	data := p.files[hash]

	// V-I05: one flipped byte → 422; nothing stored, still missing.
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 0xff
	r := e.putFile(p, hash, bad)
	wantStatus(t, "flipped byte", r, http.StatusUnprocessableEntity)
	if r.problem(t) != "file-hash-mismatch" {
		t.Fatalf("problem %s", r.body)
	}
	// Wrong size: 422 without reading.
	wantStatus(t, "short body", e.putFile(p, hash, data[:10]), http.StatusUnprocessableEntity)
	st := e.do(http.MethodGet, pkgPath(p.id), nil, 0).json(t)
	if len(st["missing"].([]any)) != len(p.files) || st["received_size"] != 0.0 {
		t.Fatalf("after a mismatch: %v", st)
	}

	// No Content-Length (chunked) → 411.
	r = e.do(http.MethodPut, pkgPath(p.id)+"/files/"+hash, bytes.NewReader(data), -1)
	wantStatus(t, "chunked upload", r, http.StatusLengthRequired)

	other := strings.Repeat("ab", 32)
	r = e.putFile(p, other, data)
	wantStatus(t, "hash not in manifest", r, http.StatusNotFound)
	if r.problem(t) != "file-not-in-manifest" {
		t.Fatalf("problem %s", r.body)
	}
	unknown := pkg{id: other}
	wrong := sha256.Sum256([]byte("other"))
	for _, r := range []response{
		e.putFile(unknown, hash, data),
		e.putFile(unknown, hash, data, "Content-Digest", digest("sha-256", wrong[:])),
		e.commit(unknown),
		e.do(http.MethodGet, pkgPath(other), nil, 0),
	} {
		wantStatus(t, "unknown package", r, http.StatusNotFound)
		if r.problem(t) != "unknown-package" {
			t.Fatalf("problem %s", r.body)
		}
	}

	// Another server cannot see the package.
	e2 := *e
	e2.token = token(t, "bbb_server.bbb-fixture-b")
	wantStatus(t, "GET by another server", e2.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusNotFound)
	r = e2.putManifest(p)
	wantStatus(t, "manifest PUT by another server", r, http.StatusForbidden)
	if r.problem(t) != "server-mismatch" {
		t.Fatalf("problem %s", r.body)
	}
}

// A missing, unknown or wrong-kind credential is 401.
func TestUnauthorized(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	for _, tok := range []string{"", "wrong", token(t, "frontend.pilos-fixture"), token(t, "admin.fixture-admin")} {
		e2 := *e
		e2.token = tok
		wantStatus(t, "manifest PUT", e2.putManifest(p), http.StatusUnauthorized)
		wantStatus(t, "GET", e2.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusUnauthorized)
	}
	if n := e.count(`SELECT count(*) FROM packages`); n != 0 {
		t.Fatalf("%d packages", n)
	}
}

// One hash under two paths: listed and uploaded once; the ack counts both
// paths (bbb-exporter.md §6.3 step 5).
func TestDuplicateHash(t *testing.T) {
	e := newEnv(t)
	p := f01(t, testdata("ingest", "exchanges", "bodies", "F-01.duplicate-hash.manifest.json"), nil)
	r := e.putManifest(p)
	wantStatus(t, "manifest PUT", r, http.StatusCreated)
	st := r.json(t)
	if len(st["missing"].([]any)) != len(p.files) || len(p.files) != len(p.m.Files)-1 {
		t.Fatalf("missing %v for %d files", st["missing"], len(p.m.Files))
	}
	e.uploadAll(p)
	r = e.commit(p)
	wantStatus(t, "commit", r, http.StatusOK)
	ack := r.json(t)
	if ack["file_count"] != float64(len(p.m.Files)) || ack["size"] != float64(p.m.Totals.Size) {
		t.Fatalf("ack %s", r.body)
	}
	if n := e.count(`SELECT count(*) FROM package_files WHERE verified_at IS NULL`); n != 0 {
		t.Fatalf("%d unverified rows", n)
	}
}

// failingPut is a blob store whose Put fails transiently.
type failingPut struct{ blob.Store }

func (failingPut) Put(context.Context, string, string, int64, io.Reader) error {
	return blob.ErrTransient
}

// A blob store failure is 503 unavailable with Retry-After 30.
func TestBlobStoreDown(t *testing.T) {
	e := newEnvWith(t, func(s blob.Store) blob.Store { return failingPut{s} })
	p := golden(t)
	r := e.putManifest(p)
	wantStatus(t, "manifest PUT", r, http.StatusServiceUnavailable)
	if r.problem(t) != "unavailable" || r.header.Get("Retry-After") != "30" {
		t.Fatalf("503: %v %s", r.header, r.body)
	}
	if n := e.count(`SELECT count(*) FROM packages`); n != 0 {
		t.Fatalf("%d packages after a failed manifest store", n)
	}
}

func TestDigestMatches(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	h := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		header string
		want   bool
	}{
		{"", true},
		{digest("sha-256", sum[:]), true},
		{digest("SHA-256", sum[:]), true},
		{digest("sha-512", []byte("y")), true},
		{"md5=:abc=:", true},
		{digest("sha-256", []byte("y")), false},
		{"sha-256=:%%%:", false},
		{"sha-256=abc", false},
	} {
		if got := digestMatches(c.header, h); got != c.want {
			t.Errorf("digestMatches(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// Staging activity counts every request: a manifest re-check and a GET move
// last_activity_at of an uncommitted package; a committed one is left alone.
func TestActivity(t *testing.T) {
	e := newEnv(t)
	p := golden(t)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	setOld := func() {
		if _, err := e.pool.Exec(context.Background(),
			`UPDATE packages SET last_activity_at = now() - interval '1 day'`); err != nil {
			t.Fatal(err)
		}
	}
	recent := func() bool {
		return e.count(`SELECT count(*) FROM packages WHERE last_activity_at > now() - interval '1 hour'`) == 1
	}
	setOld()
	wantStatus(t, "manifest re-check", e.putManifest(p), http.StatusOK)
	if !recent() {
		t.Fatal("manifest re-check did not record activity")
	}
	setOld()
	wantStatus(t, "GET", e.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusOK)
	if !recent() {
		t.Fatal("GET did not record activity")
	}
	e.uploadAll(p)
	wantStatus(t, "commit", e.commit(p), http.StatusOK)
	setOld()
	wantStatus(t, "GET after commit", e.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusOK)
	if recent() {
		t.Fatal("GET after commit changed last_activity_at")
	}
}

// badKeyPut is a blob store whose Put fails with an error a retry cannot fix.
type badKeyPut struct{ blob.Store }

func (badKeyPut) Put(context.Context, string, string, int64, io.Reader) error {
	return blob.ErrInvalidKey
}

// An unexpected server error is 500 internal, without Retry-After.
func TestInternalError(t *testing.T) {
	e := newEnvWith(t, func(s blob.Store) blob.Store { return badKeyPut{s} })
	r := e.putManifest(golden(t))
	wantStatus(t, "manifest PUT", r, http.StatusInternalServerError)
	if r.problem(t) != "internal" || r.header.Get("Retry-After") != "" {
		t.Fatalf("500: %v %s", r.header, r.body)
	}
}

func TestIsInternal(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{blob.ErrTransient, false},
		{errors.New("connection refused"), false},
		{context.DeadlineExceeded, false},
		{&pgconn.PgError{Code: "57P01"}, false}, // admin shutdown
		{&pgconn.PgError{Code: "40001"}, false}, // serialization failure
		{&pgconn.PgError{Code: "23505"}, true},  // unique violation
		{&pgconn.PgError{Code: "42P01"}, true},  // undefined table
		{&pgconn.PgError{Code: "22001"}, true},  // string too long
		{blob.ErrInvalidKey, true},
	} {
		if got := isInternal(c.err); got != c.want {
			t.Errorf("isInternal(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
