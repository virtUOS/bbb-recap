package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// stateCommitted is the final package state (docs/reference/README.md "Ingest
// package status"); before it come receiving and complete.
const stateCommitted = "committed"

var (
	errUnknownPackage = errors.New("ingest: unknown package")
	errNotInManifest  = errors.New("ingest: file not in manifest")
)

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// status reads the IngestStatus of a package of the given server. An unknown
// package is pgx.ErrNoRows. Sizes count each distinct hash once; the ack
// fields carry the manifest totals (bbb-exporter.md §6.2). cleanup deletes
// the package_files rows of a committed package and keeps their count in
// packages.file_count (privacy.md §5.2), so the ack stays the same; the
// sizes and missing then read 0 and empty.
func (s *Service) status(ctx context.Context, q querier, packageID, serverID string) (*Status, error) {
	var (
		st          = Status{PackageID: packageID, Missing: []string{}}
		ack         ackFields
		committedAt *time.Time
	)
	err := q.QueryRow(ctx, `
		SELECT p.state, p.job_id::text, p.record_id, p.total_bytes, p.committed_at,
			coalesce(p.file_count, (SELECT count(*) FROM package_files f WHERE f.package_id = p.package_id))
		FROM packages p
		WHERE p.package_id = $1 AND p.bbb_server_id = $2`, packageID, serverID).
		Scan(&st.State, &ack.JobID, &ack.RecordID, &ack.Size, &committedAt, &ack.FileCount)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT sha256, max(size), bool_or(verified_at IS NOT NULL)
		FROM package_files WHERE package_id = $1
		GROUP BY sha256 ORDER BY sha256`, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var size int64
		var verified bool
		if err := rows.Scan(&hash, &size, &verified); err != nil {
			return nil, err
		}
		st.ExpectedSize += size
		if verified {
			st.ReceivedSize += size
		} else {
			st.Missing = append(st.Missing, hash)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if st.State == stateCommitted && committedAt != nil {
		ack.CommittedAt = committedAt.UTC().Format(time.RFC3339)
		st.ackFields = &ack
	}
	return &st, nil
}

// create stages a new package: in one transaction it inserts the packages
// row (receiving) and the package_files rows and stores the manifest bytes
// as a blob under the new job_id. It reports created = false, and stores
// nothing, if a package with this package_id or for this (bbb_server_id,
// record_id) already exists, also one committed concurrently.
func (s *Service) create(ctx context.Context, m *manifest.Manifest, packageID string, data []byte) (jobID string, created bool, err error) {
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	jobID = queue.NewJobID()
	tag, err := tx.Exec(ctx, `
		INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
		VALUES ($1, $2, $3, $4, $5, 'receiving', $6)
		ON CONFLICT DO NOTHING`,
		packageID, jobID, m.BBBServerID, m.RecordID, m.MeetingID, m.Totals.Size)
	if err != nil {
		return "", false, fmt.Errorf("insert package: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", false, nil
	}
	rows := make([][]any, len(m.Files))
	for i, f := range m.Files {
		rows[i] = []any{packageID, f.Path, f.Role, f.Size, f.SHA256}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"package_files"},
		[]string{"package_id", "path", "role", "size", "sha256"}, pgx.CopyFromRows(rows)); err != nil {
		return "", false, fmt.Errorf("insert package files: %w", err)
	}
	// The manifest blob is written before the rows commit, so a staged
	// package always has it. A failed commit leaves an orphan prefix, which
	// the cleanup loop removes (architecture.md §8).
	if err := s.cfg.Blobs.Put(ctx, jobID, packageID, int64(len(data)), bytes.NewReader(data)); err != nil {
		return "", false, fmt.Errorf("store manifest: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return jobID, true, nil
}

// touch records activity on an uncommitted package of the server, so the
// staging TTL counts from the last request of any kind (manifest PUT, GET,
// upload). An unknown package is no error: nothing is updated.
func (s *Service) touch(ctx context.Context, packageID, serverID string) error {
	_, err := s.cfg.Pool.Exec(ctx, `
		UPDATE packages SET last_activity_at = now()
		WHERE package_id = $1 AND bbb_server_id = $2 AND state <> 'committed'`, packageID, serverID)
	return err
}

// stagedFile is what a file upload needs to know.
type stagedFile struct {
	jobID    string
	state    string
	size     int64
	verified bool
}

// lookupFile finds a package of the server and the file with the given hash
// in it. For a committed package it does not require the file to be listed.
func (s *Service) lookupFile(ctx context.Context, packageID, serverID, hash string) (*stagedFile, error) {
	var f stagedFile
	var listed bool
	err := s.cfg.Pool.QueryRow(ctx, `
		SELECT p.job_id::text, p.state,
			EXISTS (SELECT 1 FROM package_files WHERE package_id = p.package_id AND sha256 = $3),
			coalesce((SELECT max(size) FROM package_files WHERE package_id = p.package_id AND sha256 = $3), 0),
			EXISTS (SELECT 1 FROM package_files
				WHERE package_id = p.package_id AND sha256 = $3 AND verified_at IS NOT NULL)
		FROM packages p
		WHERE p.package_id = $1 AND p.bbb_server_id = $2`, packageID, serverID, hash).
		Scan(&f.jobID, &f.state, &listed, &f.size, &f.verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errUnknownPackage
	}
	if err != nil {
		return nil, err
	}
	// The state comes first: an upload to a committed package is 409
	// package-committed (bbb-exporter.md §6.2), also once cleanup deleted
	// its package_files rows.
	if !listed && f.state != stateCommitted {
		return nil, errNotInManifest
	}
	return &f, nil
}

// markVerified marks every package_files row with the hash as verified and
// records the activity; the package becomes complete when nothing is
// missing. It reports whether this call verified the rows (false: another
// upload of the same hash did it first, or the package is committed).
func (s *Service) markVerified(ctx context.Context, packageID, hash string) (bool, error) {
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the package row first, in a statement of its own, so concurrent
	// uploads to one package take turns and the completeness check below
	// sees the rows the other upload committed.
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM packages WHERE package_id = $1 FOR UPDATE`,
		packageID).Scan(&state); err != nil {
		return false, err
	}
	if state == stateCommitted {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE package_files SET verified_at = now()
		WHERE package_id = $1 AND sha256 = $2 AND verified_at IS NULL`, packageID, hash)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE packages SET last_activity_at = now(),
			state = CASE WHEN EXISTS (SELECT 1 FROM package_files
					WHERE package_id = $1 AND verified_at IS NULL)
				THEN 'receiving' ELSE 'complete' END
		WHERE package_id = $1`, packageID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// commitPackage commits a package of the server: if every hash is verified,
// one transaction marks the package committed, creates the job with its
// steps (queue.CreateJob) and writes the ingest_commit audit row. A package
// that is committed already is left as it is. It returns the missing hashes
// if there are any, and errUnknownPackage for an unknown package.
func (s *Service) commitPackage(ctx context.Context, packageID, serverID string) (missing []string, err error) {
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state, jobID string
	err = tx.QueryRow(ctx, `
		SELECT state, job_id::text FROM packages
		WHERE package_id = $1 AND bbb_server_id = $2
		FOR UPDATE`, packageID, serverID).Scan(&state, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errUnknownPackage
	}
	if err != nil {
		return nil, err
	}
	if state == stateCommitted {
		return nil, nil
	}
	st, err := s.status(ctx, tx, packageID, serverID)
	if err != nil {
		return nil, err
	}
	if len(st.Missing) > 0 {
		return st.Missing, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE packages SET state = 'committed', committed_at = now(), last_activity_at = now()
		WHERE package_id = $1`, packageID); err != nil {
		return nil, fmt.Errorf("mark committed: %w", err)
	}
	if err := queue.CreateJob(ctx, tx, queue.NewJob{
		JobID:      jobID,
		PackageID:  packageID,
		ConfigHash: s.cfg.ConfigHash,
	}); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (actor_kind, actor_id, action, job_id, detail)
		VALUES ('client', $1, 'ingest_commit', $2, 'received')`,
		"bbb_server."+serverID, jobID); err != nil {
		return nil, fmt.Errorf("insert audit: %w", err)
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.cfg.Log.Info("package committed", "bbb_server_id", serverID, "package_id", packageID, "job_id", jobID)
	return nil, nil
}
