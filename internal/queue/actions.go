package queue

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The admin actions of architecture.md §5.4. Each runs in the caller's
// transaction and writes its audit row there (action = the action name,
// detail = the job's new state, for rerun the new job_id), so a rollback
// leaves neither the change nor the audit row. A job in the wrong state is
// ErrInvalidState, an unknown one ErrNotFound; neither changes anything.

// Errors of the admin actions.
var (
	ErrInvalidState = errors.New("queue: action not allowed in the job's state")
	// ErrDocumentIDNotAllowed: a document ID was given to release a job
	// that is not held as delivery_uncertain.
	ErrDocumentIDNotAllowed = errors.New("queue: document_id only applies to delivery_uncertain")
)

// lockedJob is a job row read under FOR UPDATE.
type lockedJob struct {
	state      string
	heldReason string
	step       string // current_step
	stepState  string // state of current_step
	stopping   bool   // cancel or purge requested
}

// lockJob locks the job row and reads it, then reads its current step in a
// second statement, so the step row is current (see Cancel).
func lockJob(ctx context.Context, tx pgx.Tx, jobID string) (lockedJob, error) {
	var j lockedJob
	err := tx.QueryRow(ctx, `
		SELECT state, coalesce(held_reason, ''), current_step,
			cancel_requested_at IS NOT NULL OR purge_requested_at IS NOT NULL
		FROM jobs WHERE job_id = $1
		FOR UPDATE`, jobID).Scan(&j.state, &j.heldReason, &j.step, &j.stopping)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	err = tx.QueryRow(ctx, `SELECT state FROM steps WHERE job_id = $1 AND name = $2`, jobID, j.step).
		Scan(&j.stepState)
	return j, err
}

// Retry runs the current step of a failed job, or of a job held as
// delivery_uncertain, again: step pending with its attempts reset, job
// running; earlier outputs are kept. For delivery_uncertain the deliveries
// row in sending is deleted, so deliver sends once more (sinks.md §1.4 item
// 4, §1.7).
func Retry(ctx context.Context, tx pgx.Tx, by Actor, jobID string) error {
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	uncertain := j.state == "held" && j.heldReason == "delivery_uncertain"
	if j.state != "failed" && !uncertain {
		return ErrInvalidState
	}
	if uncertain {
		if _, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE job_id = $1 AND state = 'sending'`,
			jobID); err != nil {
			return err
		}
	}
	if err := resetStep(ctx, tx, jobID, j.step); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET state = 'running', held_reason = NULL, failure_reason = NULL, detail = NULL,
			updated_at = now()
		WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "retry", jobID, "running")
}

// Release lifts the hold of a held job. delivery_uncertain: the admin found
// the document, so the deliveries row becomes confirmed_by_admin with the
// optional documentID (empty = none), deliver succeeded, the job delivered
// and cleanup scheduled after retention (RECAP_RETENTION_AFTER_DELIVERY).
// Any other reason: the held step runs again, as ReleaseHeld.
func Release(ctx context.Context, tx pgx.Tx, by Actor, jobID, documentID string, retention time.Duration) error {
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if j.state != "held" {
		return ErrInvalidState
	}
	if j.heldReason != "delivery_uncertain" {
		if documentID != "" {
			return ErrDocumentIDNotAllowed
		}
		if _, err := releaseHeld(ctx, tx, jobID, []string{j.heldReason}); err != nil {
			return err
		}
		return auditAction(ctx, tx, by, "release", jobID, "running")
	}

	// The record is normally in sending (sinks.md §1.4); without one, a
	// new record is written from the job's sink profile.
	tag, err := tx.Exec(ctx, `
		UPDATE deliveries SET state = 'confirmed_by_admin', document_id = coalesce($2, document_id)
		WHERE job_id = $1`, jobID, nullIfEmpty(documentID))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries (job_id, sink_profile, state, document_id)
			SELECT job_id, sink_profile, 'confirmed_by_admin', $2 FROM jobs WHERE job_id = $1`,
			jobID, nullIfEmpty(documentID)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET state = 'succeeded', owner = NULL, lease_expires_at = NULL
		WHERE job_id = $1 AND name = 'deliver'`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET state = 'delivered', held_reason = NULL, detail = NULL, delivered_at = now(),
			current_step = 'cleanup', updated_at = now()
		WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET run_after = now() + $2 * interval '1 millisecond'
		WHERE job_id = $1 AND name = 'cleanup'`, jobID, retention.Milliseconds()); err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "release", jobID, "delivered")
}

// Purge deletes a held or failed job's content: the job runs cleanup only
// and ends cleaned/purged (architecture.md §5.1). Purge never calls a sink.
func Purge(ctx context.Context, tx pgx.Tx, by Actor, jobID string) error {
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if j.state != "held" && j.state != "failed" {
		return ErrInvalidState
	}
	if err := requestPurge(ctx, tx, jobID, j); err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "purge", jobID, "running")
}

// Rerun creates the job next.JobID from the package of jobID, starting at
// validate with next.ConfigHash (architecture.md §5.4). The caller has
// copied the package's blobs to the new job's prefix before. The new job
// takes over the registration: jobs.registration_id moves from the old job
// to the new one, so the registration's active job is always the one job
// that references it. The old job then goes down the purge path and ends
// cleaned/purged without touching the registration. A job that is cleaned,
// or already stopping (cancel or purge requested), is ErrInvalidState.
func Rerun(ctx context.Context, tx pgx.Tx, by Actor, jobID string, next NewJob) error {
	var recordID string
	err := tx.QueryRow(ctx, `
		SELECT p.record_id, j.package_id FROM jobs j JOIN packages p USING (package_id)
		WHERE j.job_id = $1`, jobID).Scan(&recordID, &next.PackageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Lock order: the registration before any job (architecture.md §6.2),
	// as the registration API does. validate joins a job only to the
	// registration of its record, so this is the row the jobs reference.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM registrations WHERE record_id = $1 FOR NO KEY UPDATE`,
		recordID); err != nil {
		return err
	}
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if j.state == "cleaned" || j.stopping {
		return ErrInvalidState
	}
	next.RerunOf = jobID
	if err := CreateJob(ctx, tx, next); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET registration_id = (SELECT registration_id FROM jobs WHERE job_id = $2)
		WHERE job_id = $1`, next.JobID, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET registration_id = NULL WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	if err := audit(ctx, tx, by, next.JobID, "received"); err != nil {
		return err
	}
	if err := requestPurge(ctx, tx, jobID, j); err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "rerun", jobID, next.JobID)
}

// requestPurge marks a locked job for purging. If its current step is
// claimed, the move to cleanup happens when that step ends (finish), as for
// Cancel. Otherwise the job goes to its cleanup step now, runnable at once
// with fresh attempts; a held or failed job becomes running, so it can be
// claimed (a delivered job runs cleanup as it is).
func requestPurge(ctx context.Context, tx pgx.Tx, jobID string, j lockedJob) error {
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET purge_requested_at = coalesce(purge_requested_at, now()), updated_at = now()
		WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	if j.stepState == "claimed" {
		return nil
	}
	if j.state == "held" || j.state == "failed" {
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET state = 'running', held_reason = NULL, failure_reason = NULL, detail = NULL
			WHERE job_id = $1`, jobID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET current_step = 'cleanup' WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	return resetStep(ctx, tx, jobID, "cleanup")
}

// resetStep makes a step that is not claimed runnable now with its attempts
// reset. Output and the last error are kept.
func resetStep(ctx context.Context, tx pgx.Tx, jobID, step string) error {
	_, err := tx.Exec(ctx, `
		UPDATE steps SET state = 'pending', attempt = 0, run_after = now(), owner = NULL, lease_expires_at = NULL
		WHERE job_id = $1 AND name = $2 AND state <> 'claimed'`, jobID, step)
	return err
}

// auditAction writes the audit row of an admin action (architecture.md
// §6.1); detail holds codes or IDs only.
func auditAction(ctx context.Context, tx pgx.Tx, by Actor, action, jobID, detail string) error {
	var id *string
	if by.ID != "" {
		id = &by.ID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit (actor_kind, actor_id, action, job_id, detail)
		VALUES ($1, $2, $3, $4, $5)`, by.Kind, id, action, jobID, detail)
	return err
}
