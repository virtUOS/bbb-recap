// Package admin serves the admin API of docs/reference/architecture.md §4 and
// §5.4: the job list and one job, the actions retry, release, rerun, purge
// and erase, and the audit list. No answer carries personal data (no owner, no
// meeting name, no title). The same logic is available as Go functions
// (ListJobs, Job, JobDetail, ListAudit, Action, Erase) for the admin web UI
// (internal/adminui); the HTTP handlers call them too.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Config configures the admin API.
type Config struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store // rerun copies the package's blobs; erase deletes the prefix
	// Results holds the frontend sink's summaries (erase deletes the job's);
	// nil when the blob backend keeps none.
	Results blob.Results
	// ConfigHash is pinned by a job that rerun creates (architecture.md §5.2).
	ConfigHash string
	// RetentionAfterDelivery schedules cleanup after release of a
	// delivery_uncertain job (deployment.md §5.3).
	RetentionAfterDelivery time.Duration
	Log                    *slog.Logger
}

// API serves the admin endpoints.
type API struct {
	pool       *pgxpool.Pool
	blobs      blob.Store
	results    blob.Results
	configHash string
	retention  time.Duration
	log        *slog.Logger
}

// New returns the admin API.
func New(c Config) *API {
	return &API{pool: c.Pool, blobs: c.Blobs, results: c.Results, configHash: c.ConfigHash, retention: c.RetentionAfterDelivery, log: c.Log}
}

// Mount adds the endpoints to mux. Only admin credentials pass.
func (a *API) Mount(mux *http.ServeMux, creds *httpapi.Credentials) {
	admin := func(h http.HandlerFunc) http.Handler { return creds.Require(httpapi.KindAdmin, h) }
	mux.Handle("GET /api/v1/admin/jobs", admin(a.listJobs))
	mux.Handle("GET /api/v1/admin/jobs/{job_id}", admin(a.getJob))
	mux.Handle("POST /api/v1/admin/jobs/{job_id}/{action}", admin(a.postAction))
	mux.Handle("GET /api/v1/admin/audit", admin(a.listAudit))
}

// Job is the admin job body (architecture.md §4). Values that do not apply
// are null.
type Job struct {
	JobID         string    `json:"job_id"`
	RecordID      string    `json:"record_id"`
	MeetingID     string    `json:"meeting_id"`
	BBBServerID   string    `json:"bbb_server_id"`
	State         string    `json:"state"`
	HeldReason    *string   `json:"held_reason"`
	FailureReason *string   `json:"failure_reason"`
	Outcome       *string   `json:"outcome"`
	Detail        *string   `json:"detail"`
	Delivery      *Delivery `json:"delivery"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
}

// Delivery is the deliveries row of a job.
type Delivery struct {
	State      string  `json:"state"`
	DocumentID *string `json:"document_id"`
}

// jobIDRE is a UUID in canonical form, so a malformed path never reaches the
// database as a uuid cast error.
var jobIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Errors of the Go functions that the HTTP handlers and the admin web UI
// call. The handlers map them to problems.
var (
	ErrNotFound             = queue.ErrNotFound             // unknown or malformed job ID: 404
	ErrInvalidState         = queue.ErrInvalidState         // action not allowed in the job's state: 409
	ErrDocumentIDNotAllowed = queue.ErrDocumentIDNotAllowed // 400
	ErrInvalidDocumentID    = errors.New("admin: document_id must be 1 to 255 bytes")
	ErrInvalidFilter        = errors.New("admin: invalid filter, limit or cursor")
	ErrUnknownAction        = errors.New("admin: unknown action")
)

// normalizeJobID lowercases a job ID (UUIDs are case-insensitive; stored
// lowercase, as blob keys need) and reports whether it is a UUID.
func normalizeJobID(id string) (string, bool) {
	id = strings.ToLower(id)
	return id, jobIDRE.MatchString(id)
}

// Job returns the body of one job; ErrNotFound if it does not exist.
func (a *API) Job(ctx context.Context, jobID string) (Job, error) {
	jobID, ok := normalizeJobID(jobID)
	if !ok {
		return Job{}, ErrNotFound
	}
	j, _, err := scanJob(a.pool.QueryRow(ctx, jobSelect+` WHERE j.job_id = $1::uuid`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	a.writeJob(w, r, r.PathValue("job_id"))
}

// writeJob answers 200 with the body of one job, 404 if it does not exist.
func (a *API) writeJob(w http.ResponseWriter, r *http.Request, jobID string) {
	j, err := a.Job(r.Context(), jobID)
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteProblem(w, http.StatusNotFound, "not-found", "")
		return
	}
	if err != nil {
		a.internal(w, r, "admin job read failed", err, "job_id", strings.ToLower(jobID))
		return
	}
	writeJSON(w, j)
}

// jobSelect reads the columns of the job body; scanJob scans them.
const jobSelect = `
	SELECT j.job_id::text, p.record_id, p.meeting_id, p.bbb_server_id, j.state,
		j.held_reason, j.failure_reason, j.outcome, j.detail, d.state, d.document_id,
		j.created_at, j.updated_at
	FROM jobs j
	JOIN packages p ON p.package_id = j.package_id
	LEFT JOIN deliveries d ON d.job_id = j.job_id`

// scanJob scans one row of jobSelect. It also returns created_at at full
// precision, for the list cursor.
func scanJob(row pgx.Row) (Job, time.Time, error) {
	var (
		j          Job
		created    time.Time
		updated    time.Time
		deliverySt *string
		documentID *string
	)
	err := row.Scan(&j.JobID, &j.RecordID, &j.MeetingID, &j.BBBServerID, &j.State,
		&j.HeldReason, &j.FailureReason, &j.Outcome, &j.Detail, &deliverySt, &documentID,
		&created, &updated)
	if err != nil {
		return j, created, err
	}
	if deliverySt != nil {
		j.Delivery = &Delivery{State: *deliverySt, DocumentID: documentID}
	}
	j.CreatedAt = formatTime(created)
	j.UpdatedAt = formatTime(updated)
	return j, created, nil
}

// formatTime renders a timestamp as RFC 3339 in UTC, to the second.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// internal logs an unexpected error by its class only and answers 500.
func (a *API) internal(w http.ResponseWriter, r *http.Request, msg string, err error, attrs ...any) {
	a.log.ErrorContext(r.Context(), msg, append(attrs, "error_class", errorClass(err))...)
	httpapi.WriteProblem(w, http.StatusInternalServerError, "internal", "")
}

// errorClass names an error without its text, which may echo values: the
// SQLSTATE of a Postgres error, else the Go type (privacy.md §6).
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return "sqlstate-" + pgErr.Code
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline-exceeded"
	}
	return fmt.Sprintf("%T", err)
}
