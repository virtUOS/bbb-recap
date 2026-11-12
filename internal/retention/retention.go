// Package retention is the cleanup loop of docs/reference/architecture.md §8 and
// privacy.md §5.3: once at start and then hourly in every worker, four
// sweepers delete what has outlived its retention (privacy.md §4):
//
//  1. ingest staging idle longer than RECAP_INGEST_STAGING_TTL (S0): the
//     package's blobs, package_files and packages rows;
//  2. unmatched registrations past expires_at (RECAP_REGISTRATION_TTL),
//     tombstones older than 30 days (S2) and result records past
//     result_expires_at (delivered_at + RECAP_RESULT_TTL, S9; ADR-014);
//     then the frontend sink's summaries results/<job_id>/ (S10) whose job
//     is gone, cleaned without the outcome delivered, or delivered longer
//     than RECAP_RESULT_TTL ago;
//  3. blob prefixes older than one day (age from the UUIDv7 time of the
//     job_id, docs/reference/README.md "HTTP") whose job has no row or is
//     cleaned, unless the prefix belongs to a package still in staging;
//  4. cleaned jobs older than RECAP_JOB_METADATA_RETENTION (J1, J4, S7):
//     the job with its steps, step_items and deliveries (ON DELETE CASCADE),
//     its audit rows; then, in one statement, every committed packages row
//     (with its package_files) that no job references any more.
//
// Every delete is guarded by the same predicate it was selected with, so
// several workers may run the loop at once. Logs carry counts only.
package retention

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// Fixed constants (architecture.md §8, privacy.md §4); not configuration.
const (
	Interval           = time.Hour
	TombstoneRetention = 30 * 24 * time.Hour
	OrphanAge          = 24 * time.Hour
)

// batch bounds the rows one transaction of sweeper 4 deletes.
const batch = 100

// Loop runs the sweepers.
type Loop struct {
	Pool              *pgxpool.Pool
	Blobs             blob.Store
	Log               *slog.Logger
	StagingTTL        time.Duration // RECAP_INGEST_STAGING_TTL
	MetadataRetention time.Duration // RECAP_JOB_METADATA_RETENTION
	ResultTTL         time.Duration // RECAP_RESULT_TTL
	// Results holds the frontend sink's summaries; nil when the backend
	// keeps none.
	Results blob.Results
}

// Counts is what one pass deleted.
type Counts struct {
	Staging       int // packages in staging
	Registrations int // unmatched registrations
	Tombstones    int
	Results       int // result records (S9)
	Summaries     int // frontend summaries (S10)
	Orphans       int // blob prefixes
	Jobs          int // cleaned jobs with their metadata
	Packages      int // committed packages without a job
}

// Run sweeps at once and then every Interval until ctx ends. It returns
// promptly when ctx ends: every database and blob call runs under ctx.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		l.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep runs the four sweepers once and logs the counts. A sweeper that
// fails is logged and the next one runs; the next pass tries again.
func (l *Loop) Sweep(ctx context.Context) Counts {
	var c Counts
	var err error
	if c.Staging, err = l.staging(ctx); err != nil {
		l.failed(ctx, "staging", err)
	}
	if c.Registrations, c.Tombstones, err = l.registrations(ctx); err != nil {
		l.failed(ctx, "registrations", err)
	}
	if c.Results, err = l.results(ctx); err != nil {
		l.failed(ctx, "results", err)
	}
	if c.Summaries, err = l.summaries(ctx); err != nil {
		l.failed(ctx, "summaries", err)
	}
	if c.Orphans, err = l.orphans(ctx); err != nil {
		l.failed(ctx, "orphans", err)
	}
	if c.Jobs, err = l.jobs(ctx); err != nil {
		l.failed(ctx, "jobs", err)
	}
	if c.Packages, err = l.packages(ctx); err != nil {
		l.failed(ctx, "packages", err)
	}
	if ctx.Err() == nil {
		l.Log.Info("cleanup loop: pass done", "staging", c.Staging, "registrations", c.Registrations,
			"tombstones", c.Tombstones, "results", c.Results, "summaries", c.Summaries, "orphan_prefixes", c.Orphans, "jobs", c.Jobs,
			"packages", c.Packages)
	}
	return c
}

// failed logs a sweeper error, unless the loop is stopping.
func (l *Loop) failed(ctx context.Context, sweeper string, err error) {
	if ctx.Err() == nil {
		l.Log.Warn("cleanup loop: sweeper failed", "sweeper", sweeper, "err", err)
	}
}

// staging deletes uncommitted packages idle longer than StagingTTL, one per
// transaction: the row is locked (SKIP LOCKED: another worker has it), the
// blobs are deleted, then the rows. An upload that writes a blob after the
// delete finds its row gone; its prefix is an orphan for sweeper 3.
func (l *Loop) staging(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		done := false
		err := pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
			var packageID, jobID string
			err := tx.QueryRow(ctx, `
				SELECT package_id, job_id::text FROM packages
				WHERE state <> 'committed' AND last_activity_at < now() - $1 * interval '1 microsecond'
				ORDER BY last_activity_at
				LIMIT 1
				FOR UPDATE SKIP LOCKED`, l.StagingTTL.Microseconds()).Scan(&packageID, &jobID)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			if err := l.Blobs.DeletePrefix(ctx, jobID); err != nil {
				return err
			}
			// package_files go with the row (ON DELETE CASCADE).
			_, err = tx.Exec(ctx, `DELETE FROM packages WHERE package_id = $1`, packageID)
			return err
		})
		if err != nil {
			return n, err
		}
		if done {
			return n, nil
		}
		n++
	}
	return n, ctx.Err()
}

// registrations deletes unmatched registrations past expires_at and
// tombstones older than TombstoneRetention. A registration is unmatched when
// no job references it and no job of its record_id is still live (one that
// is not cleaned may still join it in validate). A held or failed job keeps
// its registration until an admin acts (privacy.md §4, A9).
func (l *Loop) registrations(ctx context.Context) (expired, tombstones int, err error) {
	tag, err := l.Pool.Exec(ctx, `
		DELETE FROM registrations r
		WHERE r.status = 'registered' AND r.expires_at < now()
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.registration_id = r.registration_id)
			AND NOT EXISTS (
				SELECT 1 FROM jobs j JOIN packages p USING (package_id)
				WHERE p.record_id = r.record_id AND j.state <> 'cleaned')`)
	if err != nil {
		return 0, 0, err
	}
	expired = int(tag.RowsAffected())
	tag, err = l.Pool.Exec(ctx, `
		DELETE FROM registrations
		WHERE status = 'cancelled' AND cancelled_at < now() - $1 * interval '1 microsecond'`,
		TombstoneRetention.Microseconds())
	if err != nil {
		return expired, 0, err
	}
	return expired, int(tag.RowsAffected()), nil
}

// results deletes result records past result_expires_at (S9). The row
// holds IDs and times only; its job keeps registration_id NULL (ON DELETE
// SET NULL), as after a cleanup with RECAP_RESULT_TTL 0.
func (l *Loop) results(ctx context.Context) (int, error) {
	tag, err := l.Pool.Exec(ctx, `
		DELETE FROM registrations WHERE result_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// summaries deletes the frontend summaries (S10) that may not stay: the
// job has no row (erased, or a restore), was cleaned without the outcome
// delivered, or was delivered longer than ResultTTL ago. A job still on its
// way (not cleaned, not delivered yet) keeps its summary: deliver may be
// writing it. The check runs right before each delete. It runs after
// results(), so a front end never sees the summary gone while its result
// record is still there. The front end's DELETE …/summary removes the
// others earlier.
func (l *Loop) summaries(ctx context.Context) (int, error) {
	if l.Results == nil {
		return 0, nil
	}
	jobs, err := l.Results.ListResults(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		var keep bool
		if err := l.Pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM jobs WHERE job_id = $1::uuid
					AND (state <> 'cleaned' OR outcome = 'delivered')
					AND (delivered_at IS NULL OR delivered_at >= now() - $2 * interval '1 microsecond'))`,
			j.ID, l.ResultTTL.Microseconds()).Scan(&keep); err != nil {
			return n, err
		}
		if keep {
			continue
		}
		if err := l.Results.DeleteResults(ctx, j.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// orphans deletes blob prefixes older than OrphanAge whose job has no row or
// is cleaned (failed ingest transactions, restores, privacy.md §5.4). A
// prefix whose package is still in staging is no orphan. The check runs
// right before each delete; a job_id is never reused, so a prefix found
// orphaned cannot gain a live job afterwards.
func (l *Loop) orphans(ctx context.Context) (int, error) {
	jobs, err := l.Blobs.ListJobs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if !blob.ValidJobID(j.ID) || time.Since(Created(j)) < OrphanAge {
			continue
		}
		var live bool
		if err := l.Pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM jobs WHERE job_id = $1::uuid AND state <> 'cleaned')
				OR EXISTS (SELECT 1 FROM packages WHERE job_id = $1::uuid AND state <> 'committed')`,
			j.ID).Scan(&live); err != nil {
			return n, err
		}
		if live {
			continue
		}
		if err := l.Blobs.DeletePrefix(ctx, j.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Created returns when a job prefix was created: the UUIDv7 time of its
// job_id (RFC 9562 §5.7). An ID of another version has no time; then the
// newest object time stands in.
func Created(j blob.Job) time.Time {
	if len(j.ID) != 36 || j.ID[14] != '7' {
		return j.Modified
	}
	b, err := hex.DecodeString(j.ID[0:8] + j.ID[9:13])
	if err != nil {
		return j.Modified
	}
	var ts [8]byte
	copy(ts[2:], b)
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ts[:])))
}

// jobs deletes cleaned jobs whose cleaned_at is older than
// MetadataRetention, in batches. Rows locked by another worker are skipped.
func (l *Loop) jobs(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		deleted := 0
		err := pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT job_id::text FROM jobs
				WHERE state = 'cleaned' AND cleaned_at < now() - $1 * interval '1 microsecond'
				ORDER BY cleaned_at
				LIMIT $2
				FOR UPDATE SKIP LOCKED`, l.MetadataRetention.Microseconds(), batch)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil || len(ids) == 0 {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM audit WHERE job_id = ANY($1::uuid[])`, ids); err != nil {
				return err
			}
			// steps, step_items and deliveries go with the job (ON DELETE
			// CASCADE); a rerun's rerun_of becomes NULL.
			// Their packages go in packages(), after the last batch.
			if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE job_id = ANY($1::uuid[])`, ids); err != nil {
				return err
			}
			deleted = len(ids)
			return nil
		})
		if err != nil {
			return n, err
		}
		n += deleted
		if deleted < batch {
			return n, nil
		}
	}
	return n, ctx.Err()
}

// packages deletes committed packages that no job references any more,
// after sweeper 4 removed their last job. One statement over all of them,
// so a package whose jobs (a rerun pair) were deleted by two workers at
// once is not left behind. Safe: the ingest commit writes the committed
// package and its job in one transaction, and a rerun adds a job only to a
// package whose job is not cleaned yet, so a committed package without a
// job is always a leftover.
func (l *Loop) packages(ctx context.Context) (int, error) {
	tag, err := l.Pool.Exec(ctx, `
		DELETE FROM packages p
		WHERE p.state = 'committed'
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.package_id = p.package_id)`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
