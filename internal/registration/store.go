package registration

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Stored statuses. "matched" is not stored: it is derived from a job that
// references the registration (jobs.registration_id, set by validate).
const (
	statusRegistered = "registered"
	statusCancelled  = "cancelled"
)

// row is a registrations row. Owner fields are empty in a tombstone.
type row struct {
	id         int64
	frontendID string
	meetingID  string
	ownerSub   string
	ownerEmail string
	status     string
}

const selectRow = `
	SELECT registration_id, frontend_id, meeting_id, coalesce(owner_sub, ''), coalesce(owner_email, ''), status
	FROM registrations WHERE record_id = $1`

func scanRow(ctx context.Context, tx pgx.Tx, query, recordID string) (row, bool, error) {
	var r row
	err := tx.QueryRow(ctx, query, recordID).Scan(&r.id, &r.frontendID, &r.meetingID, &r.ownerSub, &r.ownerEmail, &r.status)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// lockRegistration reads the row and locks it until the transaction ends, so
// concurrent requests for one record_id run one after the other. The lock is
// FOR NO KEY UPDATE: no request changes a key column, and the weaker lock
// lets validate join a job meanwhile (its foreign key check takes FOR KEY
// SHARE on this row) instead of deadlocking with a request that waits for
// that job's lock (https://www.postgresql.org/docs/16/explicit-locking.html#LOCKING-ROWS).
func lockRegistration(ctx context.Context, tx pgx.Tx, recordID string) (row, bool, error) {
	return scanRow(ctx, tx, selectRow+" FOR NO KEY UPDATE", recordID)
}

// insertRegistration creates a registered row. It reports false, and
// changes nothing, if a row for the record_id exists already.
func insertRegistration(ctx context.Context, tx pgx.Tx, frontendID string, req request, ttl time.Duration) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO registrations (record_id, frontend_id, meeting_id, owner_sub, owner_email, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'registered', now() + $6 * interval '1 microsecond')
		ON CONFLICT (record_id) DO NOTHING`,
		req.RecordID, frontendID, req.MeetingID, req.Owner.Sub, req.Owner.Email, ttl.Microseconds())
	return tag.RowsAffected() == 1, err
}

// replaceOwner sets a new owner. expires_at counts from this PUT
// (deployment.md, RECAP_REGISTRATION_TTL).
func replaceOwner(ctx context.Context, tx pgx.Tx, id int64, req request, ttl time.Duration) error {
	_, err := tx.Exec(ctx, `
		UPDATE registrations SET owner_sub = $2, owner_email = $3, updated_at = now(),
			expires_at = now() + $4 * interval '1 microsecond'
		WHERE registration_id = $1`,
		id, req.Owner.Sub, req.Owner.Email, ttl.Microseconds())
	return err
}

// reactivate turns a tombstone without a job into a new registration on the
// same row, with new created_at and expires_at.
func reactivate(ctx context.Context, tx pgx.Tx, id int64, req request, ttl time.Duration) error {
	_, err := tx.Exec(ctx, `
		UPDATE registrations SET status = 'registered', owner_sub = $2, owner_email = $3,
			created_at = now(), updated_at = now(), expires_at = now() + $4 * interval '1 microsecond',
			cancelled_at = NULL
		WHERE registration_id = $1`,
		id, req.Owner.Sub, req.Owner.Email, ttl.Microseconds())
	return err
}

// tombstone deletes the owner identity and keeps IDs and cancelled_at
// (§2.5). The cleanup loop removes it after 30 days.
func tombstone(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE registrations SET status = 'cancelled', owner_sub = NULL, owner_email = NULL,
			expires_at = NULL, cancelled_at = now(), updated_at = now()
		WHERE registration_id = $1`, id)
	return err
}

// anyJob reports whether a job exists for the registration: joined by
// validate, or not yet joined but from a package with this record_id.
func anyJob(ctx context.Context, tx pgx.Tx, id int64, recordID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM jobs WHERE registration_id = $1)
			OR EXISTS (SELECT 1 FROM jobs JOIN packages USING (package_id) WHERE packages.record_id = $2)`,
		id, recordID).Scan(&exists)
	return exists, err
}

// deliveryStarted reports whether a job of the registration has left the
// pending deliver step or has a deliveries row (sending, sent or confirmed).
// The owner is locked from then on (§2.4, identity-mapping.md §5). The job
// rows are locked first, so a deliver claim running now is seen.
func deliveryStarted(ctx context.Context, tx pgx.Tx, id int64, recordID string) (bool, error) {
	if _, err := lockJobs(ctx, tx, recordID); err != nil {
		return false, err
	}
	var started bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs j JOIN steps s ON s.job_id = j.job_id AND s.name = 'deliver'
			WHERE j.registration_id = $1 AND s.state <> 'pending')
		OR EXISTS (
			SELECT 1 FROM jobs j JOIN deliveries d ON d.job_id = j.job_id
			WHERE j.registration_id = $1)`, id).Scan(&started)
	return started, err
}

// lockJobs locks every job row of the record, whatever its state, and
// returns the job IDs. The caller has locked the registration row first
// (architecture.md §6.2 lock order). A step boundary of such a job (claim,
// finish, hold) holds the same lock until it commits, so this waits for it,
// and the caller's following statements see its result. Filtering on a
// state here would miss a job that changes state while this waits: READ
// COMMITTED rechecks only rows that matched the snapshot
// (https://www.postgresql.org/docs/16/transaction-iso.html#XACT-READ-COMMITTED).
func lockJobs(ctx context.Context, tx pgx.Tx, recordID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT j.job_id::text FROM jobs j JOIN packages p USING (package_id)
		WHERE p.record_id = $1
		ORDER BY j.created_at, j.job_id
		FOR UPDATE OF j`, recordID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// releaseHeld resumes every job of the record held as no_registration or
// owner_unresolved (§3; architecture.md §5.1). no_registration jobs are not
// joined yet, so they are found through their package's record_id. The
// released step runs its checks again. It returns the released job IDs.
// The jobs are locked first (lockJobs), so a validate that commits a
// no_registration hold while this PUT runs is waited for and released.
func releaseHeld(ctx context.Context, tx pgx.Tx, frontendID, recordID string) ([]string, error) {
	ids, err := lockJobs(ctx, tx, recordID)
	if err != nil {
		return nil, err
	}
	by := queue.Actor{Kind: "client", ID: frontendID}
	var released []string
	for _, id := range ids {
		ok, err := queue.ReleaseHeld(ctx, tx, by, id, "no_registration", "owner_unresolved")
		if err != nil {
			return nil, err
		}
		if ok {
			released = append(released, id)
		}
	}
	return released, nil
}

// cancelJobs cancels every job of the registration (§2.5) and returns the
// IDs it cancelled. A job cleaned with outcome purged made no document and
// is skipped. If a job has started delivery, it returns errAlreadyDelivered
// and the caller's transaction rolls back. The record's jobs are locked first
// (lockJobs), so a validate that joins a job while this DELETE runs is
// waited for, and the join is seen.
func cancelJobs(ctx context.Context, tx pgx.Tx, frontendID string, id int64, recordID string) ([]string, error) {
	if _, err := lockJobs(ctx, tx, recordID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT job_id::text FROM jobs
		WHERE registration_id = $1 AND NOT (state = 'cleaned' AND outcome IS NOT DISTINCT FROM 'purged')
		ORDER BY created_at, job_id`, id)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	by := queue.Actor{Kind: "client", ID: frontendID}
	for _, jobID := range ids {
		err := queue.Cancel(ctx, tx, by, jobID)
		if errors.Is(err, queue.ErrNotCancellable) {
			return nil, errAlreadyDelivered
		}
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// response is the registration response ($defs/registration_response). It
// never contains the owner.
type response struct {
	SchemaVersion string  `json:"schema_version"`
	RecordID      string  `json:"record_id"`
	MeetingID     string  `json:"meeting_id"`
	FrontendID    string  `json:"frontend_id"`
	Status        string  `json:"status"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	ExpiresAt     *string `json:"expires_at"`
	Job           *jobRef `json:"job"`
}

type jobRef struct {
	JobID            string  `json:"job_id"`
	State            string  `json:"state"`
	HeldReason       *string `json:"held_reason"`
	Outcome          *string `json:"outcome"`
	DeliveredAt      *string `json:"delivered_at"`
	DocumentURL      *string `json:"document_url"`
	SummaryAvailable bool    `json:"summary_available"`

	// summaryDue: the job was delivered to a frontend profile, so its
	// summary waits unless picked up or deleted by the cleanup loop after
	// RECAP_RESULT_TTL (in the same pass as the result record). Set by load;
	// summaryAvailable then asks the blob store.
	summaryDue bool
}

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// load builds the response of the registration for recordID and reports
// whether it exists. job is the newest job that references the
// registration. status is cancelled for a tombstone, else matched with a job
// and registered without. The result fields (§2.7) come from the job and its
// delivery record; summary_available is left to summaryAvailable.
func (a *API) load(ctx context.Context, q querier, recordID string) (response, bool, error) {
	var (
		resp        response
		id          int64
		created     time.Time
		updated     time.Time
		expires     *time.Time
		jobID       *string
		jobState    *string
		jobReason   *string
		outcome     *string
		deliveredAt *time.Time
		profile     *string
		documentID  *string
	)
	err := q.QueryRow(ctx, `
		SELECT r.registration_id, r.record_id, r.meeting_id, r.frontend_id, r.status,
			r.created_at, r.updated_at, r.expires_at, j.job_id::text, j.state, j.held_reason,
			j.outcome, j.delivered_at, d.sink_profile, d.document_id
		FROM registrations r
		LEFT JOIN LATERAL (
			SELECT job_id, state, held_reason, outcome, delivered_at FROM jobs
			WHERE registration_id = r.registration_id
			ORDER BY created_at DESC, job_id DESC LIMIT 1) j ON true
		LEFT JOIN deliveries d ON d.job_id = j.job_id AND d.state IN ('sent', 'confirmed_by_admin')
		WHERE r.record_id = $1`, recordID).
		Scan(&id, &resp.RecordID, &resp.MeetingID, &resp.FrontendID, &resp.Status,
			&created, &updated, &expires, &jobID, &jobState, &jobReason,
			&outcome, &deliveredAt, &profile, &documentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return resp, false, nil
	}
	if err != nil {
		return resp, false, err
	}
	resp.SchemaVersion = schemaVersion
	resp.CreatedAt = formatTime(created)
	resp.UpdatedAt = formatTime(updated)
	if jobID != nil {
		resp.Job = &jobRef{JobID: *jobID, State: *jobState, HeldReason: jobReason, Outcome: outcome}
		if resp.Status == statusRegistered {
			resp.Status = "matched"
		}
		a.result(resp.Job, deliveredAt, profile, documentID)
	}
	if resp.Status == statusRegistered && expires != nil {
		s := formatTime(*expires)
		resp.ExpiresAt = &s
	}
	return resp, true, nil
}

// result sets the result fields of a job (§2.7). outcome is the job's own
// (set when it is cleaned); a delivered job not yet cleaned shows delivered.
// Nothing is shown before delivery. document_url is the profile's template
// with the document ID (sinks.md §1.3: built when shown, not stored).
func (a *API) result(j *jobRef, deliveredAt *time.Time, profile, documentID *string) {
	if j.Outcome == nil && j.State == "delivered" {
		delivered := "delivered"
		j.Outcome = &delivered
	}
	if deliveredAt == nil {
		return
	}
	at := formatTime(*deliveredAt)
	j.DeliveredAt = &at
	if j.Outcome == nil || *j.Outcome != "delivered" || profile == nil {
		return
	}
	s := a.sinks[*profile]
	if s.DocumentURLTemplate != "" && documentID != nil && *documentID != "" {
		u := strings.ReplaceAll(s.DocumentURLTemplate, "{id}", url.PathEscape(*documentID))
		j.DocumentURL = &u
	}
	j.summaryDue = s.Frontend
}

// summaryAvailable sets summary_available of resp: a summary is due and
// the blob store still has it (the front end has not picked it up).
func (a *API) summaryAvailable(ctx context.Context, resp *response) error {
	if resp.Job == nil || !resp.Job.summaryDue || a.results == nil {
		return nil
	}
	rc, err := a.results.GetSummary(ctx, resp.Job.JobID)
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Job.SummaryAvailable = true
	return rc.Close()
}

// formatTime renders a timestamp as RFC 3339 in UTC, to the second.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
