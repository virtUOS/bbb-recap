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
	// that is not held as delivery_uncertain, to release a failed sink, or
	// to release several sending sinks at once.
	ErrDocumentIDNotAllowed = errors.New("queue: document_id only applies to one sink in sending")
	// ErrSinkNotInJob: the sink named in an action is not a sink of the job
	// (sinks.md §1.8: 400 invalid-request).
	ErrSinkNotInJob = errors.New("queue: sink is not a sink of the job")
)

// lockedJob is a job row read under FOR UPDATE.
type lockedJob struct {
	state      string
	heldReason string
	step       string // current_step
	stepState  string // state of current_step
	stopping   bool   // cancel or purge requested
}

// uncertain reports whether the job is held as delivery_uncertain.
func (j lockedJob) uncertain() bool { return j.state == "held" && j.heldReason == "delivery_uncertain" }

// failedInDeliver reports whether the job failed in its deliver step.
func (j lockedJob) failedInDeliver() bool { return j.state == "failed" && j.step == "deliver" }

// ownerUnresolved reports whether the job is held as owner_unresolved in
// deliver.
func (j lockedJob) ownerUnresolved() bool {
	return j.state == "held" && j.heldReason == "owner_unresolved" && j.step == "deliver"
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

// lockSink locks the job and finds the named sink among its sinks:
// ErrSinkNotInJob if it is not one.
func lockSink(ctx context.Context, tx pgx.Tx, jobID, name string) (lockedJob, []SinkStatus, SinkStatus, error) {
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return j, nil, SinkStatus{}, err
	}
	sinks, err := SinkStatuses(ctx, tx, jobID)
	if err != nil {
		return j, nil, SinkStatus{}, err
	}
	for _, s := range sinks {
		if s.Profile == name {
			return j, sinks, s, nil
		}
	}
	return j, nil, SinkStatus{}, ErrSinkNotInJob
}

// Retry runs the current step of a failed job, or of a job held as
// delivery_uncertain, again: step pending with its attempts reset, job
// running; earlier outputs are kept. In deliver, every failed sink and every
// sink in sending becomes pending first: its step item or its deliveries
// row in sending is deleted, so deliver sends once more (sinks.md §1.4 item
// 4, §1.7, §1.8). With sink (not empty), only that sink is reset; it must be
// failed or in sending (else ErrInvalidState), and the job state then
// follows from the sink statuses (Settle).
func Retry(ctx context.Context, tx pgx.Tx, by Actor, jobID, sink string, retention time.Duration) error {
	if sink != "" {
		return retrySink(ctx, tx, by, jobID, sink, retention)
	}
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if j.state != "failed" && !j.uncertain() {
		return ErrInvalidState
	}
	if j.step == "deliver" {
		if _, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE job_id = $1 AND state = 'sending'`,
			jobID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM step_items WHERE job_id = $1 AND step = 'deliver'`,
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

// retrySink is Retry of one sink (sinks.md §1.8).
func retrySink(ctx context.Context, tx pgx.Tx, by Actor, jobID, name string, retention time.Duration) error {
	j, _, s, err := lockSink(ctx, tx, jobID, name)
	if err != nil {
		return err
	}
	if !j.uncertain() && !j.failedInDeliver() {
		return ErrInvalidState
	}
	switch s.Status {
	case SinkSending:
		if _, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE job_id = $1 AND sink_profile = $2 AND state = 'sending'`,
			jobID, name); err != nil {
			return err
		}
	case SinkFailed:
		if _, err := tx.Exec(ctx, `DELETE FROM step_items WHERE job_id = $1 AND step = 'deliver' AND item = $2`,
			jobID, name); err != nil {
			return err
		}
	default:
		return ErrInvalidState
	}
	state, err := Settle(ctx, tx, jobID, retention)
	if err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "retry", jobID, state)
}

// Release lifts the hold of a held job (sink empty). delivery_uncertain:
// the admin found the documents, so every deliveries row in sending becomes
// confirmed_by_admin, with documentID (empty = none) only when exactly one
// sink is in sending; the job state then follows from the sink statuses
// (Settle): with every sink done the job is delivered and cleanup
// scheduled after retention (RECAP_RETENTION_AFTER_DELIVERY). Any other
// reason: the held step runs again, as ReleaseHeld.
//
// With sink, the job is held as delivery_uncertain or owner_unresolved, or
// failed in deliver, and the sink decides (sinks.md §1.8): in sending it
// becomes confirmed_by_admin (with the optional documentID); failed, in a
// failed job, or pending, in a job held owner_unresolved, it becomes
// skipped_by_admin, unless every other sink is skipped already. Any other
// status is ErrInvalidState.
func Release(ctx context.Context, tx pgx.Tx, by Actor, jobID, sink, documentID string, retention time.Duration) error {
	if sink != "" {
		return releaseSink(ctx, tx, by, jobID, sink, documentID, retention)
	}
	j, err := lockJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if j.state != "held" {
		return ErrInvalidState
	}
	if !j.uncertain() {
		if documentID != "" {
			return ErrDocumentIDNotAllowed
		}
		if _, err := releaseHeld(ctx, tx, jobID, []string{j.heldReason}); err != nil {
			return err
		}
		return auditAction(ctx, tx, by, "release", jobID, "running")
	}

	sinks, err := SinkStatuses(ctx, tx, jobID)
	if err != nil {
		return err
	}
	var sending []string
	for _, s := range sinks {
		if s.Status == SinkSending {
			sending = append(sending, s.Profile)
		}
	}
	switch {
	case len(sending) > 1 && documentID != "":
		return ErrDocumentIDNotAllowed
	case len(sending) == 0 && len(sinks) == 1:
		// No record although the job is held as delivery_uncertain: a new
		// record is written for the job's one sink.
		sending = []string{sinks[0].Profile}
	}
	for _, name := range sending {
		if err := confirm(ctx, tx, jobID, name, documentID); err != nil {
			return err
		}
	}
	state, err := Settle(ctx, tx, jobID, retention)
	if err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "release", jobID, state)
}

// releaseSink is Release of one sink (sinks.md §1.8).
func releaseSink(ctx context.Context, tx pgx.Tx, by Actor, jobID, name, documentID string,
	retention time.Duration) error {
	j, sinks, s, err := lockSink(ctx, tx, jobID, name)
	if err != nil {
		return err
	}
	if !j.uncertain() && !j.failedInDeliver() && !j.ownerUnresolved() {
		return ErrInvalidState
	}
	switch {
	case s.Status == SinkSending && !j.ownerUnresolved():
		if err := confirm(ctx, tx, jobID, name, documentID); err != nil {
			return err
		}
	case s.Status == SinkFailed && j.failedInDeliver(), s.Status == SinkPending && j.ownerUnresolved():
		// Skip: a failed sink of a failed job, or the pending sink of a job
		// held owner_unresolved whose owner stays locked (sinks.md §1.8).
		if documentID != "" {
			return ErrDocumentIDNotAllowed
		}
		others := false
		for _, o := range sinks {
			if o.Profile != name && o.Status != SinkSkippedByAdmin {
				others = true
			}
		}
		if !others {
			return ErrInvalidState // nothing would be delivered: purge instead
		}
		if _, err := tx.Exec(ctx, `DELETE FROM step_items WHERE job_id = $1 AND step = 'deliver' AND item = $2`,
			jobID, name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries (job_id, sink_profile, state) VALUES ($1, $2, 'skipped_by_admin')`,
			jobID, name); err != nil {
			return err
		}
	default:
		return ErrInvalidState
	}
	state, err := Settle(ctx, tx, jobID, retention)
	if err != nil {
		return err
	}
	return auditAction(ctx, tx, by, "release", jobID, state)
}

// confirm marks a sink's record confirmed_by_admin with the optional
// documentID, writing the record if there is none.
func confirm(ctx context.Context, tx pgx.Tx, jobID, sink, documentID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO deliveries (job_id, sink_profile, state, document_id) VALUES ($1, $2, 'confirmed_by_admin', $3)
		ON CONFLICT (job_id, sink_profile) DO UPDATE
		SET state = 'confirmed_by_admin', document_id = coalesce($3, deliveries.document_id)`,
		jobID, sink, nullIfEmpty(documentID))
	return err
}

// Settle sets the state of a locked job whose deliver step is not claimed
// from its sink statuses, after an admin action and without calling a sink
// (sinks.md §1.8): every sink done → delivered, deliver succeeded and
// cleanup scheduled after retention; a sink failed → failed with the failure
// reason of the first failed sink; a sink in sending → held
// delivery_uncertain; else the deliver step runs again with its attempts
// reset (running). It returns the new state.
func Settle(ctx context.Context, tx pgx.Tx, jobID string, retention time.Duration) (string, error) {
	sinks, err := SinkStatuses(ctx, tx, jobID)
	if err != nil {
		return "", err
	}
	var failed *SinkStatus
	sending, pending := false, false
	for i, s := range sinks {
		switch s.Status {
		case SinkFailed:
			if failed == nil {
				failed = &sinks[i]
			}
		case SinkSending:
			sending = true
		case SinkPending:
			pending = true
		}
	}
	switch {
	case failed != nil:
		reason := "retries_exhausted"
		if failed.FailureReason != nil {
			reason = *failed.FailureReason
		}
		if _, err := tx.Exec(ctx, `
			UPDATE steps SET state = 'failed', owner = NULL, lease_expires_at = NULL
			WHERE job_id = $1 AND name = 'deliver'`, jobID); err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `
			UPDATE jobs SET state = 'failed', held_reason = NULL, failure_reason = $2, detail = NULL,
				current_step = 'deliver', updated_at = now()
			WHERE job_id = $1`, jobID, reason)
		return "failed", err
	case sending:
		if _, err := tx.Exec(ctx, `
			UPDATE steps SET state = 'pending', owner = NULL, lease_expires_at = NULL
			WHERE job_id = $1 AND name = 'deliver'`, jobID); err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `
			UPDATE jobs SET state = 'held', held_reason = 'delivery_uncertain', failure_reason = NULL, detail = NULL,
				current_step = 'deliver', updated_at = now()
			WHERE job_id = $1`, jobID)
		return "held", err
	case pending:
		if err := resetStep(ctx, tx, jobID, "deliver"); err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `
			UPDATE jobs SET state = 'running', held_reason = NULL, failure_reason = NULL, detail = NULL,
				current_step = 'deliver', updated_at = now()
			WHERE job_id = $1`, jobID)
		return "running", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET state = 'succeeded', owner = NULL, lease_expires_at = NULL
		WHERE job_id = $1 AND name = 'deliver'`, jobID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET state = 'delivered', held_reason = NULL, failure_reason = NULL, detail = NULL,
			delivered_at = now(), current_step = 'cleanup', updated_at = now()
		WHERE job_id = $1`, jobID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET run_after = now() + $2 * interval '1 millisecond'
		WHERE job_id = $1 AND name = 'cleanup'`, jobID, retention.Milliseconds()); err != nil {
		return "", err
	}
	return "delivered", nil
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
