// Package cleanup implements the cleanup step (docs/reference/privacy.md §5.1,
// §5.2; architecture.md §5.2, §6.4). It deletes a job's content and its
// owner's identity and keeps only pseudonymous job metadata (IDs, states,
// times, codes and the delivery record).
//
// The engine runs cleanup after delivery (outcome delivered), for a
// cancelled job (registration DELETE, or a tombstone found by validate;
// outcome cancelled) and for an admin purge or the old job of a rerun
// (outcome purged). The engine writes the outcome; this step only deletes.
//
// Every sub-step is idempotent, so a repeated run succeeds and changes
// nothing:
//
//  1. ASR provider cleanup for every handle still recorded. No step_items
//     row records a handle today: the whisperx-family dialect is
//     synchronous, and transcribe calls the adapter's Cleanup right after
//     each request (providers.md §1, §2.4). So there is nothing to do, and
//     the output says asr_cleanups 0.
//  2. Blob prefix jobs/<job_id>/ (package, step outputs, summary.md,
//     transcript.vtt).
//  3. The registration of the job, reached only through
//     jobs.registration_id (one registration has one active job; a job
//     that a rerun replaced has none). A tombstone (status cancelled) stays,
//     with its owner fields cleared, until the cleanup loop removes it after
//     30 days. The registration of a job that ends delivered stays as the
//     result record (metadata-interface.md §2.7, privacy.md S9) when
//     ResultTTL is above 0: owner_sub, owner_email and expires_at are
//     cleared and result_expires_at is delivered_at + ResultTTL; the
//     cleanup loop deletes it then. Any other registration (ResultTTL 0, a
//     purged job) is deleted.
//     3a. The frontend sink's summary results/<job_id>/ (S10) stays for the
//     front end when the job ends delivered and ResultTTL is above 0; else
//     (cancel, purge, the old job of a rerun) it is deleted.
//  4. Verification: the prefix is no longer listed and the job's
//     registration, if any and unless a rerun took it over, holds no
//     owner_sub or owner_email. Else the
//     step is transient and runs again.
//  5. The package_files rows of the job's package, whose paths hold BBB
//     user IDs (privacy.md §5.2). The package row keeps file_count and
//     total_bytes. A rerun pair shares the package, so the rows stay while
//     another job of the package is not cleaned; the last job's cleanup
//     deletes them. This runs in the engine's completion transaction
//     (Result.Commit), which holds the job row: a rerun locks that row
//     too, so it either comes first (its job keeps the rows) or finds the
//     job cleaned and is refused. The package row is locked, so the
//     cleanups of a rerun pair see each other's outcome. Verification:
//     no row is left unless another job still needs them, else the
//     commit fails and the step runs again.
package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Step is the step name.
const Step = "cleanup"

// What happened to the job's registration (Output.Registration).
const (
	RegistrationNone      = "none"      // the job references no registration (any more)
	RegistrationDeleted   = "deleted"   // deleted with the owner identity
	RegistrationTombstone = "tombstone" // kept as a tombstone, IDs only
	RegistrationResult    = "result"    // kept as the result record, IDs and times only
)

// Output is stored in steps.output: counts and codes only.
type Output struct {
	// ASRCleanups is the number of provider cleanup calls (step 1).
	ASRCleanups  int    `json:"asr_cleanups"`
	Registration string `json:"registration"`
}

// Deps are what the cleanup handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	// Results holds the frontend sink's summaries; nil when the backend
	// keeps none (then there are none to delete).
	Results blob.Results
	// ResultTTL is RECAP_RESULT_TTL; 0 deletes the registration here.
	ResultTTL time.Duration
	Log       *slog.Logger
}

// errNotClean is a failed verification (privacy.md §5.2).
var errNotClean = errors.New("cleanup: verification failed")

// Handler returns the cleanup step handler. Every error is transient: the
// engine retries, and after the last attempt the job fails with
// retries_exhausted (architecture.md §5.1).
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		out, err := d.Run(ctx, run.JobID)
		if err != nil {
			code := "cleanup"
			if errors.Is(err, errNotClean) {
				code = "cleanup_not_verified"
			}
			d.Log.Warn("cleanup: step error", "job_id", run.JobID, "error_code", code, "err", err)
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: code}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "cleanup"}
		}
		d.Log.Info("cleanup: done", "job_id", run.JobID, "registration", out.Registration,
			"asr_cleanups", out.ASRCleanups)
		jobID := run.JobID
		return queue.Result{Outcome: queue.OK, Output: b, Commit: func(ctx context.Context, tx pgx.Tx) error {
			return packageFiles(ctx, tx, jobID)
		}}
	}
}

// Run deletes the job's content and owner data and verifies the result.
// It is safe to repeat.
func (d Deps) Run(ctx context.Context, jobID string) (Output, error) {
	out := Output{}
	if err := d.Blobs.DeletePrefix(ctx, jobID); err != nil {
		return out, fmt.Errorf("delete blob prefix: %w", err)
	}
	delivered, err := d.endsDelivered(ctx, jobID)
	if err != nil {
		return out, fmt.Errorf("job: %w", err)
	}
	keep := delivered && d.ResultTTL > 0
	if d.Results != nil && !keep {
		if err := d.Results.DeleteResults(ctx, jobID); err != nil {
			return out, fmt.Errorf("delete results: %w", err)
		}
	}
	reg, err := d.registration(ctx, jobID, keep)
	if err != nil {
		return out, fmt.Errorf("registration: %w", err)
	}
	out.Registration = reg
	return out, d.verify(ctx, jobID)
}

// registration applies step 3 in a transaction of its own, before the
// engine's completion: the completion holds the step and job rows, and the
// lock order is registrations before jobs (architecture.md §6.2).
// endsDelivered reports whether the job was delivered and is not stopping:
// the engine then writes the outcome delivered when cleanup succeeds.
func (d Deps) endsDelivered(ctx context.Context, jobID string) (bool, error) {
	var delivered bool
	err := d.Pool.QueryRow(ctx, `
		SELECT delivered_at IS NOT NULL AND cancel_requested_at IS NULL AND purge_requested_at IS NULL
		FROM jobs WHERE job_id = $1`, jobID).Scan(&delivered)
	return delivered, err
}

func (d Deps) registration(ctx context.Context, jobID string, keep bool) (string, error) {
	result := RegistrationNone
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		// Lock the registration the job references, then read the job
		// again: a rerun may have moved the reference to its new job
		// meanwhile (it locks the registration first, too). Deleting the
		// row sets jobs.registration_id to NULL (ON DELETE SET NULL),
		// which locks this job's row after the registration's.
		var regID int64
		var status string
		err := tx.QueryRow(ctx, `
			SELECT registration_id, status FROM registrations
			WHERE registration_id = (SELECT registration_id FROM jobs WHERE job_id = $1)
			FOR UPDATE`, jobID).Scan(&regID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var active bool
		err = tx.QueryRow(ctx, `
			SELECT registration_id IS NOT DISTINCT FROM $2
				AND NOT EXISTS (SELECT 1 FROM jobs WHERE rerun_of = $1)
			FROM jobs WHERE job_id = $1`, jobID, regID).Scan(&active)
		if err != nil {
			return err
		}
		switch {
		case !active:
			// Replaced by a rerun: the registration belongs to the new job.
			return nil
		case status == "cancelled":
			// A cancelled job always meets a tombstone here: registration
			// DELETE tombstones in the same transaction as its cancel, and
			// validate cancels only on an existing tombstone.
			result = RegistrationTombstone
			_, err = tx.Exec(ctx, `
				UPDATE registrations SET owner_sub = NULL, owner_email = NULL, expires_at = NULL, updated_at = now()
				WHERE registration_id = $1
					AND (owner_sub IS NOT NULL OR owner_email IS NOT NULL OR expires_at IS NOT NULL)`, regID)
			return err
		case keep:
			result = RegistrationResult
			_, err = tx.Exec(ctx, `
				UPDATE registrations SET owner_sub = NULL, owner_email = NULL, expires_at = NULL,
					result_expires_at = coalesce(result_expires_at,
						(SELECT delivered_at FROM jobs WHERE job_id = $2) + $3 * interval '1 microsecond')
				WHERE registration_id = $1 AND (owner_sub IS NOT NULL OR owner_email IS NOT NULL
					OR expires_at IS NOT NULL OR result_expires_at IS NULL)`, regID, jobID, d.ResultTTL.Microseconds())
			return err
		default:
			result = RegistrationDeleted
			_, err = tx.Exec(ctx, `DELETE FROM registrations WHERE registration_id = $1`, regID)
			return err
		}
	})
	return result, err
}

// verify checks step 4: the prefix is gone and no owner data of the job
// remains in Postgres. A registration that a rerun took over is the new
// job's, not this one's.
func (d Deps) verify(ctx context.Context, jobID string) error {
	jobs, err := d.Blobs.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("list blob prefixes: %w", err)
	}
	if slices.ContainsFunc(jobs, func(j blob.Job) bool { return j.ID == jobID }) {
		return fmt.Errorf("%w: blob prefix still listed", errNotClean)
	}
	var owner bool
	if err := d.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs j JOIN registrations r USING (registration_id)
			WHERE j.job_id = $1 AND (r.owner_sub IS NOT NULL OR r.owner_email IS NOT NULL)
				AND NOT EXISTS (SELECT 1 FROM jobs c WHERE c.rerun_of = j.job_id))`,
		jobID).Scan(&owner); err != nil {
		return err
	}
	if owner {
		return fmt.Errorf("%w: owner data still stored", errNotClean)
	}
	return nil
}

// packageFiles applies step 5 inside the completion transaction, after the
// engine locked the job row (lock order jobs, then packages). It deletes the
// rows of the job's package unless another job of it is not cleaned, then
// verifies that none is left in that case.
func packageFiles(ctx context.Context, tx pgx.Tx, jobID string) error {
	var packageID string
	if err := tx.QueryRow(ctx, `
		SELECT package_id FROM packages
		WHERE package_id = (SELECT package_id FROM jobs WHERE job_id = $1)
		FOR UPDATE`, jobID).Scan(&packageID); err != nil {
		return fmt.Errorf("lock package: %w", err)
	}
	var needed bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM jobs WHERE package_id = $1 AND job_id <> $2 AND state <> 'cleaned')`,
		packageID, jobID).Scan(&needed); err != nil {
		return err
	}
	if needed {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE packages SET file_count = (SELECT count(*) FROM package_files WHERE package_id = $1)
		WHERE package_id = $1 AND EXISTS (SELECT 1 FROM package_files WHERE package_id = $1)`,
		packageID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM package_files WHERE package_id = $1`, packageID); err != nil {
		return err
	}
	var left int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM package_files WHERE package_id = $1`, packageID).
		Scan(&left); err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("%w: %d package_files rows left", errNotClean, left)
	}
	return nil
}
