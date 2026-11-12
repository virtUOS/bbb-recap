package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Erased is the answer of erase (privacy.md §5.4): what the Docs operator
// needs to delete the document, which the service has no rights on
// (ADR-007). Both are null when the job has no delivery record.
type Erased struct {
	Erased      bool    `json:"erased"`
	DocumentID  *string `json:"document_id"`
	SinkProfile *string `json:"sink_profile"`
}

// postErase serves POST /api/v1/admin/jobs/{job_id}/erase after postAction
// checked the path and the body. Only a cleaned job can be erased; an
// unknown or already erased job is 404.
func (a *API) postErase(w http.ResponseWriter, r *http.Request, jobID string) {
	client, _ := httpapi.ClientFrom(r.Context())
	answer, err := a.Erase(r.Context(), queue.Actor{Kind: "admin", ID: client.ID}, jobID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpapi.WriteProblem(w, http.StatusNotFound, "not-found", "")
		return
	case errors.Is(err, ErrInvalidState):
		httpapi.WriteProblem(w, http.StatusConflict, "invalid-job-state", "")
		return
	case err != nil:
		a.internal(w, r, "admin action failed", err, "job_id", jobID, "action", "erase")
		return
	}
	writeJSON(w, answer)
}

// Erase erases a cleaned job as by (privacy.md §5.4) and returns what the
// Docs operator needs. Errors: ErrNotFound (unknown or already erased),
// ErrInvalidState (not cleaned), or an unexpected error that the caller logs
// by its class.
func (a *API) Erase(ctx context.Context, by queue.Actor, jobID string) (Erased, error) {
	jobID, ok := normalizeJobID(jobID)
	if !ok {
		return Erased{}, ErrNotFound
	}
	var answer Erased
	err := a.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		answer, err = erase(ctx, tx, by, jobID)
		return err
	})
	if err != nil {
		return Erased{}, err
	}
	// The blob prefix is gone since cleanup; deleting it again is idempotent.
	// A failure does not undo the erasure: the cleanup loop removes prefixes
	// of unknown jobs (architecture.md §8).
	if err := a.blobs.DeletePrefix(context.WithoutCancel(ctx), jobID); err != nil {
		a.log.WarnContext(ctx, "erase: deleting the blob prefix failed; the cleanup loop removes it",
			"job_id", jobID, "error_class", errorClass(err))
	}
	// A frontend summary still waiting for pickup (privacy.md S10, ADR-014).
	// A failure is left to the cleanup loop, which deletes the summary of a
	// job without a row.
	if a.results != nil {
		if err := a.results.DeleteResults(context.WithoutCancel(ctx), jobID); err != nil {
			a.log.WarnContext(ctx, "erase: deleting the summary failed; the cleanup loop removes it",
				"job_id", jobID, "error_class", errorClass(err))
		}
	}
	// No job_id here: like the audit row, the log line does not link the
	// erasure to the subject.
	a.log.InfoContext(ctx, "admin action", "action", "erase")
	return answer, nil
}

// erase deletes the rows of a cleaned job in tx (privacy.md §5.4): the job
// with its steps, step items and delivery record (ON DELETE CASCADE), its
// audit rows and its job_id in the detail of a rerun audit row, the registration, result record (S9) or tombstone of
// its record_id, and its
// package with the package files. Rows another job still uses stay: the
// package while another job references it, the registration while another
// job references it or another job of the record is not cleaned. No row
// keeps the erased job_id: a package that stays points at its newest
// remaining job, and that job's rerun_of becomes NULL (ON DELETE SET NULL).
// Then it writes the audit row erase with job_id NULL and no detail.
//
// Lock order as everywhere: registrations, jobs, packages.
func erase(ctx context.Context, tx pgx.Tx, by queue.Actor, jobID string) (Erased, error) {
	var recordID, packageID string
	err := tx.QueryRow(ctx, `
		SELECT p.record_id, j.package_id FROM jobs j JOIN packages p USING (package_id)
		WHERE j.job_id = $1`, jobID).Scan(&recordID, &packageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Erased{}, queue.ErrNotFound
	}
	if err != nil {
		return Erased{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM registrations WHERE record_id = $1 FOR UPDATE`, recordID); err != nil {
		return Erased{}, err
	}
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM jobs WHERE job_id = $1 FOR UPDATE`, jobID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) { // erased meanwhile
		return Erased{}, queue.ErrNotFound
	}
	if err != nil {
		return Erased{}, err
	}
	if state != "cleaned" {
		return Erased{}, queue.ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM packages WHERE package_id = $1 FOR UPDATE`, packageID); err != nil {
		return Erased{}, err
	}

	answer := Erased{Erased: true}
	err = tx.QueryRow(ctx, `SELECT document_id, sink_profile FROM deliveries WHERE job_id = $1`, jobID).
		Scan(&answer.DocumentID, &answer.SinkProfile)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Erased{}, err
	}

	for _, q := range []string{
		`DELETE FROM audit WHERE job_id = $1`,
		// The rerun row of the job this one was rerun from names this job
		// in detail (queue.Rerun); every other audit detail is a code.
		`UPDATE audit SET detail = NULL WHERE action = 'rerun' AND detail = $1`,
		`DELETE FROM jobs WHERE job_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, jobID); err != nil {
			return Erased{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM registrations r
		WHERE r.record_id = $1
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.registration_id = r.registration_id)
			AND NOT EXISTS (
				SELECT 1 FROM jobs j JOIN packages p USING (package_id)
				WHERE p.record_id = r.record_id AND j.state <> 'cleaned')`, recordID); err != nil {
		return Erased{}, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM packages p
		WHERE p.package_id = $1 AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.package_id = p.package_id)`,
		packageID); err != nil {
		return Erased{}, err
	}
	// A package that stays keeps no erased job_id: its job_id, which the
	// ingest ack returns, moves to the newest remaining job of the package.
	if _, err := tx.Exec(ctx, `
		UPDATE packages SET job_id = (
			SELECT job_id FROM jobs WHERE package_id = $1 ORDER BY created_at DESC, job_id DESC LIMIT 1)
		WHERE package_id = $1 AND job_id = $2`, packageID, jobID); err != nil {
		return Erased{}, err
	}

	var id *string
	if by.ID != "" {
		id = &by.ID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit (actor_kind, actor_id, action, job_id, detail)
		VALUES ($1, $2, 'erase', NULL, NULL)`, by.Kind, id)
	return answer, err
}
