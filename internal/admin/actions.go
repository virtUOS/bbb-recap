package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// maxActionBody bounds the action body: {} or {"document_id": …}.
const maxActionBody = 8 << 10

// maxDocumentIDLen bounds the Docs document ID an admin passes to release.
// Docs' ID format is not pinned (sinks.md §1.3), so only the length is checked.
const maxDocumentIDLen = 255

// actionBody is the body of POST …/{action} (architecture.md §4).
type actionBody struct {
	DocumentID *string `json:"document_id"`
}

// postAction serves POST /api/v1/admin/jobs/{job_id}/{action} (architecture.md
// §5.4). It answers 200 with the job body; for rerun, the body of the new job;
// for erase, the body of postErase.
func (a *API) postAction(w http.ResponseWriter, r *http.Request) {
	jobID, action := r.PathValue("job_id"), r.PathValue("action")
	switch action {
	case "retry", "release", "rerun", "purge", "erase":
	default:
		httpapi.WriteProblem(w, http.StatusNotFound, "not-found", "")
		return
	}
	jobID, ok := normalizeJobID(jobID)
	if !ok {
		httpapi.WriteProblem(w, http.StatusNotFound, "not-found", "")
		return
	}
	documentID, ok := readActionBody(w, r)
	if !ok {
		return
	}
	if documentID != "" && action != "release" {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "document_id applies to release only")
		return
	}
	if action == "erase" {
		a.postErase(w, r, jobID)
		return
	}
	client, _ := httpapi.ClientFrom(r.Context())
	answer, err := a.Action(r.Context(), queue.Actor{Kind: "admin", ID: client.ID}, jobID, action, documentID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpapi.WriteProblem(w, http.StatusNotFound, "not-found", "")
		return
	case errors.Is(err, ErrInvalidState):
		httpapi.WriteProblem(w, http.StatusConflict, "invalid-job-state", "")
		return
	case errors.Is(err, ErrDocumentIDNotAllowed):
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "document_id applies to delivery_uncertain only")
		return
	case err != nil:
		a.internal(w, r, "admin action failed", err, "job_id", jobID, "action", action)
		return
	}
	a.writeJob(w, r, answer)
}

// Action runs retry, release, purge or rerun on a job as by, with its audit
// row (architecture.md §5.4). documentID applies to release of a
// delivery_uncertain job only; "" means none. It returns the job ID to show:
// the new job for rerun, else jobID. Errors: ErrNotFound, ErrInvalidState,
// ErrDocumentIDNotAllowed, ErrInvalidDocumentID, ErrUnknownAction, or an unexpected error that the
// caller logs by its class.
func (a *API) Action(ctx context.Context, by queue.Actor, jobID, action, documentID string) (string, error) {
	jobID, ok := normalizeJobID(jobID)
	if !ok {
		return "", ErrNotFound
	}
	if documentID != "" && action != "release" {
		return "", ErrDocumentIDNotAllowed
	}
	if len(documentID) > maxDocumentIDLen {
		return "", ErrInvalidDocumentID
	}
	answer := jobID
	var err error
	switch action {
	case "retry":
		err = a.inTx(ctx, func(tx pgx.Tx) error { return queue.Retry(ctx, tx, by, jobID) })
	case "release":
		err = a.inTx(ctx, func(tx pgx.Tx) error {
			return queue.Release(ctx, tx, by, jobID, documentID, a.retention)
		})
	case "purge":
		err = a.inTx(ctx, func(tx pgx.Tx) error { return queue.Purge(ctx, tx, by, jobID) })
	case "rerun":
		answer, err = a.rerun(ctx, by, jobID)
	default:
		return "", ErrUnknownAction
	}
	if err != nil {
		return "", err
	}
	if answer != jobID {
		a.log.InfoContext(ctx, "admin action", "job_id", jobID, "action", action, "new_job_id", answer)
	} else {
		a.log.InfoContext(ctx, "admin action", "job_id", jobID, "action", action)
	}
	return answer, nil
}

// readActionBody reads {} or {"document_id": "…"}; an empty body counts as
// {}. On a bad body it answers 400 (413 if too large) and returns false.
func readActionBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, ok := httpapi.ReadBody(w, r, maxActionBody)
	if !ok {
		return "", false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return "", true
	}
	var body actionBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "body must be {} or {\"document_id\": string}")
		return "", false
	}
	if body.DocumentID == nil {
		return "", true
	}
	if *body.DocumentID == "" || len(*body.DocumentID) > maxDocumentIDLen {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "document_id must be 1 to 255 bytes")
		return "", false
	}
	return *body.DocumentID, true
}

func (a *API) inTx(ctx context.Context, f func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, a.pool, f)
}

// rerun copies the package's blobs (every package file and the manifest,
// whose hash is the package_id) to a new job_id server-side, then creates
// the job in one transaction (queue.Rerun). The copy happens before the
// transaction; if that fails, the new prefix is deleted here, or later by
// the cleanup loop as an orphan (architecture.md §8). It returns the new
// job_id.
func (a *API) rerun(ctx context.Context, by queue.Actor, jobID string) (string, error) {
	var (
		state     string
		stopping  bool
		packageID string
	)
	err := a.pool.QueryRow(ctx, `
		SELECT state, cancel_requested_at IS NOT NULL OR purge_requested_at IS NOT NULL, package_id
		FROM jobs WHERE job_id = $1::uuid`, jobID).Scan(&state, &stopping, &packageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", queue.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if state == "cleaned" || stopping { // checked again under lock in queue.Rerun
		return "", queue.ErrInvalidState
	}
	rows, err := a.pool.Query(ctx, `
		SELECT sha256 FROM package_files WHERE package_id = $1
		UNION SELECT $1`, packageID)
	if err != nil {
		return "", err
	}
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}

	newID := queue.NewJobID()
	err = a.copyBlobs(ctx, jobID, newID, hashes)
	if err == nil {
		err = a.inTx(ctx, func(tx pgx.Tx) error {
			return queue.Rerun(ctx, tx, by, jobID, queue.NewJob{JobID: newID, ConfigHash: a.configHash})
		})
	}
	if err != nil {
		if derr := a.blobs.DeletePrefix(context.WithoutCancel(ctx), newID); derr != nil {
			a.log.WarnContext(ctx, "rerun: deleting the copied blobs failed; the cleanup loop removes them",
				"job_id", jobID, "new_job_id", newID, "error_class", errorClass(derr))
		}
		return "", err
	}
	return newID, nil
}

// copyBlobs copies the blobs server-side. A missing blob means the content
// is gone (cleanup ran meanwhile): ErrInvalidState.
func (a *API) copyBlobs(ctx context.Context, from, to string, hashes []string) error {
	for _, h := range hashes {
		err := a.blobs.Copy(ctx, from, to, h)
		if errors.Is(err, blob.ErrNotFound) {
			return queue.ErrInvalidState
		}
		if err != nil {
			return err
		}
	}
	return nil
}
