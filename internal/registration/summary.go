package registration

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// errNoSummary is every 404 of …/summary (§2.6, §2.7): no registration, a
// foreign one, nothing delivered, another sink, picked up or expired. No
// detail, so a foreign front end learns nothing.
var errNoSummary = &problem{http.StatusNotFound, "summary-not-available", ""}

// getSummary serves GET …/{record_id}/summary: the frontend sink's
// summary.md of the registration's job while it waits for pickup. The body
// is personal data: it is never logged; the log has IDs and the size.
func (a *API) getSummary(w http.ResponseWriter, r *http.Request, frontendID string) {
	recordID := r.PathValue("record_id")
	resp, found, err := a.load(r.Context(), a.pool, recordID)
	switch {
	case err != nil:
		a.fail(w, r, recordID, err)
		return
	case !found || resp.FrontendID != frontendID || resp.Job == nil || !resp.Job.summaryDue || a.results == nil:
		a.fail(w, r, recordID, errNoSummary)
		return
	}
	rc, err := a.results.GetSummary(r.Context(), resp.Job.JobID)
	if errors.Is(err, blob.ErrNotFound) {
		a.fail(w, r, recordID, errNoSummary)
		return
	}
	if err != nil {
		a.fail(w, r, recordID, err)
		return
	}
	defer func() { _ = rc.Close() }()
	// At most max_document_bytes (sinks.md §2.4), so it is read whole: a
	// read error still becomes a problem answer, not a cut-off body.
	md, err := io.ReadAll(rc)
	if err != nil {
		a.fail(w, r, recordID, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(md)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(md)
	a.log.InfoContext(r.Context(), "summary fetched",
		"record_id", recordID, "frontend_id", frontendID, "job_id", resp.Job.JobID, "bytes", len(md))
}

// deleteSummary serves DELETE …/{record_id}/summary: the front end confirms
// the pickup and the summary is deleted (§2.7). 204 also when nothing was
// there; the result record stays until its TTL. Only the summary of a job
// delivered to a frontend profile is deleted, so a confirmation sent too
// early cannot remove one that deliver is writing, and a job delivered to
// another sink has nothing to pick up (no delete, no pickup log line).
func (a *API) deleteSummary(w http.ResponseWriter, r *http.Request, frontendID string) {
	recordID := r.PathValue("record_id")
	resp, found, err := a.load(r.Context(), a.pool, recordID)
	switch {
	case err != nil:
		a.fail(w, r, recordID, err)
		return
	case !found || resp.FrontendID != frontendID:
		a.fail(w, r, recordID, errNoSummary)
		return
	}
	if resp.Job != nil && resp.Job.summaryDue && a.results != nil {
		if err := a.results.DeleteResults(r.Context(), resp.Job.JobID); err != nil {
			a.fail(w, r, recordID, err)
			return
		}
		a.log.InfoContext(r.Context(), "summary pickup confirmed",
			"record_id", recordID, "frontend_id", frontendID, "job_id", resp.Job.JobID)
	}
	w.WriteHeader(http.StatusNoContent)
}
