package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// These tests need TEST_DATABASE_URL (internal/db/dbtest); without it they skip.

const (
	tokenAdmin = "TEST-NOT-A-SECRET-admin"
	tokenFE    = "TEST-NOT-A-SECRET-fe"
)

var bg = context.Background()

func setup(t *testing.T) (*pgxpool.Pool, *http.ServeMux) {
	t.Helper()
	pool, mux, _ := setupLogs(t)
	return pool, mux
}

// setupLogs also returns the captured JSON log.
func setupLogs(t *testing.T) (*pgxpool.Pool, *http.ServeMux, *bytes.Buffer) {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	for name, tok := range map[string]string{"admin.test-admin": tokenAdmin, "frontend.fe-test": tokenFE} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	creds, err := httpapi.LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	mux := http.NewServeMux()
	New(Config{Pool: pool, Log: slog.New(slog.NewJSONHandler(logs, nil))}).Mount(mux, creds)
	return pool, mux, logs
}

func get(mux *http.ServeMux, jobID, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/jobs/"+jobID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func seedJob(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := queue.NewJobID()
	err := pgx.BeginFunc(bg, pool, func(tx pgx.Tx) error {
		pkg := randomHex(32)
		if _, err := tx.Exec(bg, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, 'bbb-test-a', 'record-test-1', 'meeting-test-1', 'committed', 0)`, pkg, id); err != nil {
			return err
		}
		return queue.CreateJob(bg, tx, queue.NewJob{JobID: id, PackageID: pkg, ConfigHash: randomHex(32)})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var bodyKeys = []string{"job_id", "record_id", "meeting_id", "bbb_server_id", "state", "held_reason",
	"failure_reason", "outcome", "detail", "delivery", "created_at", "updated_at"}

// decode checks the exact key set of architecture.md §4 and returns the body.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string{}, bodyKeys...)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	return m
}

func TestJobBodyNulls(t *testing.T) {
	pool, mux := setup(t)
	id := seedJob(t, pool)
	m := decode(t, get(mux, id, tokenAdmin))
	for k, want := range map[string]any{
		"job_id": id, "record_id": "record-test-1", "meeting_id": "meeting-test-1", "bbb_server_id": "bbb-test-a",
		"state": "received", "held_reason": nil, "failure_reason": nil, "outcome": nil, "detail": nil, "delivery": nil,
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if s, _ := m[k].(string); !strings.HasSuffix(s, "Z") || len(s) != len("2026-10-29T10:00:00Z") {
			t.Errorf("%s = %v, want RFC 3339 UTC", k, m[k])
		}
	}
}

func TestJobBodyValues(t *testing.T) {
	pool, mux := setup(t)
	id := seedJob(t, pool)
	if _, err := pool.Exec(bg, `UPDATE jobs SET state = 'held', held_reason = 'no_registration',
		detail = 'server-not-allowed' WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	m := decode(t, get(mux, id, tokenAdmin))
	if m["state"] != "held" || m["held_reason"] != "no_registration" || m["detail"] != "server-not-allowed" {
		t.Fatalf("body %v", m)
	}

	if _, err := pool.Exec(bg, `UPDATE jobs SET state = 'cleaned', held_reason = NULL, detail = NULL,
		outcome = 'delivered' WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(bg, `INSERT INTO deliveries (job_id, sink_profile, state, document_id)
		VALUES ($1, 'docs-test', 'sent', 'doc-test-1')`, id); err != nil {
		t.Fatal(err)
	}
	m = decode(t, get(mux, id, tokenAdmin))
	d, ok := m["delivery"].(map[string]any)
	if !ok || len(d) != 2 || d["state"] != "sent" || d["document_id"] != "doc-test-1" || m["outcome"] != "delivered" {
		t.Fatalf("body %v", m)
	}

	if _, err := pool.Exec(bg, `UPDATE deliveries SET state = 'sending', document_id = NULL WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	m = decode(t, get(mux, id, tokenAdmin))
	d, _ = m["delivery"].(map[string]any)
	if v, ok := d["document_id"]; !ok || v != nil || d["state"] != "sending" {
		t.Fatalf("delivery %v, want document_id null", d)
	}
}

func TestJobNotFoundAndAuth(t *testing.T) {
	pool, mux := setup(t)
	id := seedJob(t, pool)
	for _, c := range []struct {
		name, id, token, slug string
		status                int
	}{
		{"unknown", queue.NewJobID(), tokenAdmin, "not-found", 404},
		{"malformed", "not-a-uuid", tokenAdmin, "not-found", 404},
		{"no-credential", id, "", "unauthorized", 401},
		{"frontend-credential", id, tokenFE, "unauthorized", 401},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := get(mux, c.id, c.token)
			var p httpapi.Problem
			_ = json.Unmarshal(rec.Body.Bytes(), &p)
			if rec.Code != c.status || p.Type != httpapi.ProblemTypePrefix+c.slug ||
				rec.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// An unexpected database error answers 500 internal; only its class is logged.
func TestJobInternalError(t *testing.T) {
	pool, mux, logs := setupLogs(t)
	id := seedJob(t, pool)
	if _, err := pool.Exec(bg, `ALTER TABLE packages RENAME TO packages_gone`); err != nil {
		t.Fatal(err)
	}
	rec := get(mux, id, tokenAdmin)
	var p httpapi.Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusInternalServerError || p.Type != httpapi.ProblemTypePrefix+"internal" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), `"error_class":"sqlstate-42P01"`) || strings.Contains(logs.String(), "packages") {
		t.Fatalf("log: %s", logs)
	}
}
