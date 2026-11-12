// Package validatetest seeds a committed package and its job for the
// database tests of the validate and normalize steps. Fixture data only.
package validatetest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// F01Dir and F01Manifest are F-01's raw directory and golden manifest,
// relative to internal/steps/<step>/.
const (
	F01Dir      = "../../../testdata/meetings/F-01/raw/536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	F01Manifest = "../../../testdata/meetings/F-01/expected/manifest.json"
)

// Package is a package to seed.
type Package struct {
	Dir         string             // files at their manifest paths
	Manifest    *manifest.Manifest // nil: F-01's golden manifest
	BBBServerID string             // overrides the manifest's (the credential's server)
}

// F01 returns F-01's golden manifest.
func F01(t testing.TB) *manifest.Manifest {
	t.Helper()
	b, err := os.ReadFile(F01Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// Seed stores the package's files and manifest as blobs of a new job, writes
// the packages, package_files and jobs rows (job received at validate) and
// returns the job ID.
func Seed(t testing.TB, pool *pgxpool.Pool, store blob.Store, p Package) string {
	t.Helper()
	ctx := context.Background()
	m := p.Manifest
	if m == nil {
		m = F01(t)
	}
	if p.Dir == "" {
		p.Dir = F01Dir
	}
	server := m.BBBServerID
	if p.BBBServerID != "" {
		server = p.BBBServerID
	}
	data, err := manifest.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	packageID := manifest.PackageID(data)
	jobID := queue.NewJobID()

	if err := store.Put(ctx, jobID, packageID, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(p.Dir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, jobID, f.SHA256, f.Size, bytes.NewReader(b)); err != nil {
			t.Fatalf("put %s: %v", f.Path, err)
		}
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes, committed_at)
			VALUES ($1, $2, $3, $4, $5, 'committed', $6, now())`,
			packageID, jobID, server, m.RecordID, m.MeetingID, m.Totals.Size); err != nil {
			return err
		}
		for _, f := range m.Files {
			if _, err := tx.Exec(ctx, `
				INSERT INTO package_files (package_id, path, role, size, sha256, verified_at)
				VALUES ($1, $2, $3, $4, $5, now())`, packageID, f.Path, f.Role, f.Size, f.SHA256); err != nil {
				return err
			}
		}
		return queue.CreateJob(ctx, tx, queue.NewJob{JobID: jobID, PackageID: packageID, ConfigHash: "test"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

// Register inserts a registration and returns its ID. status is registered
// or cancelled (a tombstone).
func Register(t testing.TB, pool *pgxpool.Pool, recordID, frontendID, meetingID, status string) int64 {
	t.Helper()
	var id int64
	owner, email := any("fixture-sub-ada-0001"), any("ada.testperson@example.org")
	var cancelledAt any
	if status == "cancelled" {
		owner, email, cancelledAt = nil, nil, "2025-10-29T10:00:00Z"
	}
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status, cancelled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING registration_id`,
		recordID, frontendID, meetingID, owner, email, status, cancelledAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
