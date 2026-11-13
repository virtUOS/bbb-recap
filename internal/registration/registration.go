// Package registration serves the registration API of
// docs/reference/metadata-interface.md §2: PUT, GET and DELETE
// /api/v1/registrations/{record_id} for front ends, with the result fields
// of §2.7 (ADR-014), and GET and DELETE …/{record_id}/summary for the
// frontend sink's summary.
//
// A registration holds personal data (owner sub and email). It is never
// logged and never returned; logs and responses carry IDs only
// (privacy.md §6). The queue effects of a request, the release of held jobs
// on PUT and the cancellation of jobs on DELETE, happen in the request's
// own transaction (architecture.md §5.1).
package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/httpapi"
	"github.com/virtUOS/bbb-recap/internal/schemas"
)

// Constants of metadata-interface.md §2.1 and §2.6.
const (
	maxBody       = 8 << 10 // bytes
	rateLimit     = 600     // requests per front end and rateWindow
	rateWindow    = time.Minute
	schemaVersion = "1.2.0" // of the responses (§2.3: 1.1.0 adds the result fields, 1.2.0 the record_id pattern)
)

// Sink is what the API needs to know of a sink profile to show a result
// (§2.7).
type Sink struct {
	// Frontend: the profile is of type frontend (sinks.md §4a), so the
	// summary waits for pickup.
	Frontend bool
	// DocumentURLTemplate is the profile's document_url_template with {id}
	// (sinks.md §3.1); empty: no link.
	DocumentURLTemplate string
}

// Config configures the API.
type Config struct {
	Pool            *pgxpool.Pool
	RegistrationTTL time.Duration // RECAP_REGISTRATION_TTL
	Sinks           map[string]Sink
	// Results holds the frontend sink's summaries; nil when the blob
	// backend keeps none (then no summary is ever available).
	Results blob.Results
	Log     *slog.Logger
}

// API serves the registration endpoints.
type API struct {
	pool    *pgxpool.Pool
	ttl     time.Duration // RECAP_REGISTRATION_TTL
	sinks   map[string]Sink
	results blob.Results
	log     *slog.Logger
	limiter *limiter
}

// New returns the registration API.
func New(c Config) *API {
	return &API{pool: c.Pool, ttl: c.RegistrationTTL, sinks: c.Sinks, results: c.Results,
		log: c.Log, limiter: newLimiter(rateLimit, rateWindow)}
}

// Mount adds the endpoints to mux. Only frontend credentials pass, and every
// request counts against the front end's rate limit (§2.6, §2.7).
func (a *API) Mount(mux *http.ServeMux, creds *httpapi.Credentials) {
	const path = "/api/v1/registrations/{record_id}"
	mux.Handle("PUT "+path, creds.Require(httpapi.KindFrontend, a.limited(a.put)))
	mux.Handle("GET "+path, creds.Require(httpapi.KindFrontend, a.limited(a.get)))
	mux.Handle("DELETE "+path, creds.Require(httpapi.KindFrontend, a.limited(a.delete)))
	mux.Handle("GET "+path+"/summary", creds.Require(httpapi.KindFrontend, a.limited(a.getSummary)))
	mux.Handle("DELETE "+path+"/summary", creds.Require(httpapi.KindFrontend, a.limited(a.deleteSummary)))
}

// limited applies the per-front-end rate limit (§2.6) before h.
func (a *API) limited(h func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, _ := httpapi.ClientFrom(r.Context())
		if wait, ok := a.limiter.allow(client.ID, time.Now()); !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			httpapi.WriteProblem(w, http.StatusTooManyRequests, "rate-limited", "")
			return
		}
		h(w, r, client.ID)
	})
}

// request is the PUT body (schemas/registration.schema.json).
// SchemaVersion is any, so a wrong type there is left to the schema check.
type request struct {
	SchemaVersion any    `json:"schema_version"`
	MeetingID     string `json:"meeting_id"`
	RecordID      string `json:"record_id"`
	Owner         struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	} `json:"owner"`
}

var semverRE = regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+$`)

// decode checks a PUT body in this order: JSON, the schema major
// (so 2.0.0 is 422, not a schema violation), the schema, then path = body
// record_id. On failure it writes the problem and returns false. Validation
// errors may quote the body, so they are never logged.
func decode(w http.ResponseWriter, body []byte, recordID string) (request, bool) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "body is not a valid JSON object")
		return req, false
	}
	if v, ok := req.SchemaVersion.(string); ok {
		if m := semverRE.FindStringSubmatch(v); m != nil && m[1] != "1" {
			httpapi.WriteProblem(w, http.StatusUnprocessableEntity, "unsupported-schema-version",
				"schema_version major must be 1")
			return req, false
		}
	}
	if err := schemas.Validate(schemas.Registration, body); err != nil {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request",
			"body does not match the registration schema")
		return req, false
	}
	if req.RecordID != recordID {
		httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "record_id differs from the path")
		return req, false
	}
	return req, true
}

// problem is an RFC 9457 answer chosen inside a transaction. Returning it
// rolls the transaction back.
type problem struct {
	status int
	slug   string
	detail string
}

func (p *problem) Error() string { return p.slug }

var (
	errNotFound         = &problem{http.StatusNotFound, "not-found", ""}
	errConflict         = &problem{http.StatusConflict, "conflict", ""} // no details for a foreign front end (§2.1)
	errImmutable        = &problem{http.StatusConflict, "immutable-field", "meeting_id cannot change"}
	errLocked           = &problem{http.StatusConflict, "registration-locked", "delivery has started"}
	errCancelled        = &problem{http.StatusConflict, "registration-cancelled", "a job exists for this record"}
	errAlreadyDelivered = &problem{http.StatusConflict, "already-delivered", "delivery has started"}
)

// fail writes err as a problem, or logs its class and answers 500
// internal (README "HTTP").
func (a *API) fail(w http.ResponseWriter, r *http.Request, recordID string, err error) {
	var p *problem
	if errors.As(err, &p) {
		httpapi.WriteProblem(w, p.status, p.slug, p.detail)
		return
	}
	a.log.ErrorContext(r.Context(), "registration request failed",
		"method", r.Method, "record_id", recordID, "error_class", errorClass(err))
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

func (a *API) put(w http.ResponseWriter, r *http.Request, frontendID string) {
	recordID := r.PathValue("record_id")
	body, ok := httpapi.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	req, ok := decode(w, body, recordID)
	if !ok {
		return
	}
	var status int
	var resp response
	var released []string
	err := pgx.BeginFunc(r.Context(), a.pool, func(tx pgx.Tx) error {
		var err error
		status, err = a.store(r.Context(), tx, frontendID, req)
		if err != nil {
			return err
		}
		if released, err = releaseHeld(r.Context(), tx, frontendID, recordID); err != nil {
			return err
		}
		resp, _, err = a.load(r.Context(), tx, recordID)
		return err
	})
	if err != nil {
		a.fail(w, r, recordID, err)
		return
	}
	if err := a.summaryAvailable(r.Context(), &resp); err != nil {
		a.fail(w, r, recordID, err)
		return
	}
	a.log.InfoContext(r.Context(), "registration stored",
		"record_id", recordID, "frontend_id", frontendID, "http_status", status)
	for _, id := range released {
		a.log.InfoContext(r.Context(), "held job released by registration", "record_id", recordID, "job_id", id)
	}
	writeJSON(w, status, resp)
}

// store applies the PUT matrix (§2.4) and returns 201 or 200.
func (a *API) store(ctx context.Context, tx pgx.Tx, frontendID string, req request) (int, error) {
	reg, found, err := lockRegistration(ctx, tx, req.RecordID)
	if err != nil {
		return 0, err
	}
	if !found {
		inserted, err := insertRegistration(ctx, tx, frontendID, req, a.ttl)
		if err != nil || inserted {
			return http.StatusCreated, err
		}
		// A concurrent PUT inserted it first; its row is visible now.
		if reg, found, err = lockRegistration(ctx, tx, req.RecordID); err != nil {
			return 0, err
		}
		if !found {
			return 0, errors.New("registration vanished after a conflicting insert")
		}
	}
	switch {
	case reg.frontendID != frontendID:
		return 0, errConflict
	case reg.meetingID != req.MeetingID:
		return 0, errImmutable
	case reg.status == statusCancelled:
		exists, err := anyJob(ctx, tx, reg.id, req.RecordID)
		if err != nil {
			return 0, err
		}
		if exists {
			return 0, errCancelled
		}
		return http.StatusOK, reactivate(ctx, tx, reg.id, req, a.ttl)
	case reg.ownerSub == req.Owner.Sub && reg.ownerEmail == req.Owner.Email:
		return http.StatusOK, nil // identical: nothing changes, not even updated_at
	}
	locked, err := deliveryStarted(ctx, tx, reg.id, req.RecordID)
	if err != nil {
		return 0, err
	}
	if locked {
		return 0, errLocked
	}
	return http.StatusOK, replaceOwner(ctx, tx, reg.id, req, a.ttl)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, frontendID string) {
	recordID := r.PathValue("record_id")
	resp, found, err := a.load(r.Context(), a.pool, recordID)
	if err == nil && found && resp.FrontendID == frontendID {
		err = a.summaryAvailable(r.Context(), &resp)
	}
	switch {
	case err != nil:
		a.fail(w, r, recordID, err)
	case !found || resp.FrontendID != frontendID:
		a.fail(w, r, recordID, errNotFound)
	default:
		writeJSON(w, http.StatusOK, resp)
	}
}

// delete applies the DELETE matrix (§2.5).
func (a *API) delete(w http.ResponseWriter, r *http.Request, frontendID string) {
	recordID := r.PathValue("record_id")
	status := http.StatusNoContent
	var cancelled []string
	err := pgx.BeginFunc(r.Context(), a.pool, func(tx pgx.Tx) error {
		reg, found, err := lockRegistration(r.Context(), tx, recordID)
		if err != nil {
			return err
		}
		if !found || reg.frontendID != frontendID {
			return errNotFound
		}
		if reg.status == statusCancelled {
			return nil
		}
		if cancelled, err = cancelJobs(r.Context(), tx, frontendID, reg.id, recordID); err != nil {
			return err
		}
		if len(cancelled) > 0 {
			status = http.StatusAccepted
		}
		return tombstone(r.Context(), tx, reg.id)
	})
	if err != nil {
		a.fail(w, r, recordID, err)
		return
	}
	a.log.InfoContext(r.Context(), "registration cancelled",
		"record_id", recordID, "frontend_id", frontendID, "http_status", status)
	for _, id := range cancelled {
		a.log.InfoContext(r.Context(), "job cancelled by registration", "record_id", recordID, "job_id", id)
	}
	w.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
