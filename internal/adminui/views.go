package adminui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/admin"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// The views and actions of architecture.md §11. They show IDs, states,
// codes and timestamps only, never content, names, the owner's subject or
// email (privacy.md P-5). Each action has a confirmation page (GET) whose
// form POSTs with the CSRF token. The POST calls the same Go function as
// the admin API, which writes the audit row with the logged-in subject as
// actor_id, then redirects to the job page (post/redirect/get). The
// one-line result travels in a short-lived signed cookie, so no value
// lands in a URL.

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/style.css
var styleCSS []byte

// pageSize is the number of rows per page of the job and audit lists.
const pageSize = 50

// pages are the templates by name, each with the layout.
var pages = func() map[string]*template.Template {
	m := map[string]*template.Template{}
	for _, name := range []string{"message", "jobs", "job", "confirm", "erased", "audit"} {
		m[name] = template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	return m
}()

// view is what the layout and a page template get.
type view struct {
	Title     string
	Session   *Session // nil: logged out, no header
	Views     bool     // the job and audit views are mounted
	Flash     string   // one-line result of the last action
	Message   string   // message page
	Back      string   // message page: link back
	LoggedOut bool     // message page
	Data      any      // page specific
}

// page renders the message page.
func (u *UI) page(w http.ResponseWriter, status int, v view) {
	u.render(w, status, "message", v)
}

func (u *UI) render(w http.ResponseWriter, status int, name string, v view) {
	v.Views = u.cfg.Admin != nil
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := pages[name].ExecuteTemplate(w, "layout", v); err != nil {
		u.cfg.Log.Warn("cannot render admin page", "page", name, "error_class", "template")
	}
}

func serveStyle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write(styleCSS)
}

func (u *UI) mountViews(mux *http.ServeMux) {
	p := func(h http.HandlerFunc) http.Handler { return u.Protect(h) }
	mux.Handle("GET /admin/{$}", p(u.jobs))
	mux.Handle("GET /admin/jobs/{job_id}", p(u.job))
	mux.Handle("GET /admin/jobs/{job_id}/{action}", p(u.confirm))
	mux.Handle("POST /admin/jobs/{job_id}/{action}", p(u.act))
	mux.Handle("GET /admin/erased", p(u.erased))
	mux.Handle("GET /admin/audit", p(u.audit))
}

// session returns the session Protect checked.
func session(r *http.Request) *Session {
	s, _ := SessionFrom(r.Context())
	return &s
}

// listLinks returns the links to the first page (when not on it) and the
// next page (when there is one) of a list at path with query q.
func listLinks(path string, q url.Values, cursor, next string) (first, nextURL string) {
	if cursor != "" {
		q.Del("cursor")
		first = path + "?" + q.Encode()
	}
	if next != "" {
		q.Set("cursor", next)
		nextURL = path + "?" + q.Encode()
	}
	return first, nextURL
}

// jobs is the job list: newest first, filtered by state and held reason, in
// pages.
func (u *UI) jobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := admin.JobFilter{State: q.Get("state"), HeldReason: q.Get("held_reason"), Cursor: q.Get("cursor"),
		Limit: pageSize}
	page, err := u.cfg.Admin.ListJobs(r.Context(), f)
	if err != nil {
		u.fail(w, r, err, "")
		return
	}
	keep := url.Values{}
	for name, v := range map[string]string{"state": f.State, "held_reason": f.HeldReason, "cursor": f.Cursor} {
		if v != "" {
			keep.Set(name, v)
		}
	}
	first, next := listLinks("/admin/", keep, f.Cursor, page.NextCursor)
	u.render(w, http.StatusOK, "jobs", view{Title: "Jobs", Session: session(r), Data: struct {
		Jobs                []admin.Job
		States, HeldReasons []string
		State, HeldReason   string
		First, Next         string
	}{page.Jobs, admin.JobStates, admin.HeldReasons, f.State, f.HeldReason, first, next}})
}

// job is the job page: the job body, rerun_of, the steps and the actions
// that apply in its state.
func (u *UI) job(w http.ResponseWriter, r *http.Request) {
	d, err := u.cfg.Admin.JobDetail(r.Context(), r.PathValue("job_id"))
	if err != nil {
		u.fail(w, r, err, "")
		return
	}
	u.render(w, http.StatusOK, "job", view{Title: "Job", Session: session(r), Flash: u.takeFlash(w, r, d.JobID),
		Data: jobPage{Job: d.Job, RerunOf: d.RerunOf, CurrentStep: d.CurrentStep, Steps: d.Steps,
			Actions: actionsFor(d.Job)}})
}

// jobPage is the data of the job page. It embeds admin.Job, not
// admin.Detail: an embedded field named Detail would hide the job's detail
// code in the template.
type jobPage struct {
	admin.Job
	RerunOf     *string
	CurrentStep *string
	Steps       []admin.Step
	Actions     []string
}

// actions in the order the job page offers them, with what the
// confirmation page says they do (architecture.md §5.4).
var actions = []struct{ name, effect string }{
	{"retry", "Runs the failed or held step again: attempts reset, earlier outputs kept. For delivery_uncertain: " +
		"only after you checked Docs and found no document; deliver sends again."},
	{"release", "Lifts the hold. For delivery_uncertain: you found the document in Docs; the job is marked " +
		"delivered without sending again, optionally with the document ID. Any other reason: the held step runs again."},
	{"rerun", "Creates a new job from the same package, starting at validate, and a new Docs document. " +
		"This job then ends as cleaned (purged)."},
	{"purge", "Runs cleanup only: the job's content is deleted and the job ends as cleaned (purged)."},
	{"erase", "Data-subject erasure: deletes the job's remaining rows and audit entries. This cannot be undone. " +
		"The next page shows the Docs document ID for the Docs operator."},
}

func actionEffect(name string) (string, bool) {
	for _, a := range actions {
		if a.name == name {
			return a.effect, true
		}
	}
	return "", false
}

// actionsFor lists the actions that apply to j's state (architecture.md
// §5.4). It only decides which buttons the page shows; the action itself
// checks the state under lock.
func actionsFor(j admin.Job) []string {
	held := j.State == "held"
	uncertain := held && j.HeldReason != nil && *j.HeldReason == "delivery_uncertain"
	allowed := map[string]bool{
		"retry":   j.State == "failed" || uncertain,
		"release": held,
		"rerun":   j.State != "cleaned",
		"purge":   held || j.State == "failed",
		"erase":   j.State == "cleaned",
	}
	var out []string
	for _, a := range actions {
		if allowed[a.name] {
			out = append(out, a.name)
		}
	}
	return out
}

// confirm is the confirmation page of an action. It is shown in any state;
// the POST answers a wrong state with the 409 message.
func (u *UI) confirm(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	effect, ok := actionEffect(action)
	if !ok {
		u.fail(w, r, admin.ErrNotFound, "")
		return
	}
	j, err := u.cfg.Admin.Job(r.Context(), r.PathValue("job_id"))
	if err != nil {
		u.fail(w, r, err, "")
		return
	}
	docField := action == "release" && j.State == "held" && j.HeldReason != nil && *j.HeldReason == "delivery_uncertain"
	u.render(w, http.StatusOK, "confirm", view{Title: "Confirm " + action, Session: session(r), Data: struct {
		Job             admin.Job
		Action, Effect  string
		DocumentIDField bool
	}{j, action, effect, docField}})
}

// act runs a confirmed action (Protect checked the CSRF token) and
// redirects to the job page, for rerun to the new job, for erase to the
// page with the Docs document ID.
func (u *UI) act(w http.ResponseWriter, r *http.Request) {
	jobID, action := strings.ToLower(r.PathValue("job_id")), r.PathValue("action")
	if _, ok := actionEffect(action); !ok {
		u.fail(w, r, admin.ErrNotFound, "")
		return
	}
	by := queue.Actor{Kind: "admin", ID: session(r).Subject}
	back := "/admin/jobs/" + url.PathEscape(jobID)
	if action == "erase" {
		res, err := u.cfg.Admin.Erase(r.Context(), by, jobID)
		if err != nil {
			u.fail(w, r, err, back)
			return
		}
		u.setFlash(w, flash{Action: action, JobID: jobID, DocumentID: res.DocumentID, SinkProfile: res.SinkProfile})
		http.Redirect(w, r, "/admin/erased", http.StatusSeeOther)
		return
	}
	documentID := strings.TrimSpace(r.PostFormValue("document_id"))
	shown, err := u.cfg.Admin.Action(r.Context(), by, jobID, action, documentID)
	if err != nil {
		u.fail(w, r, err, back)
		return
	}
	u.setFlash(w, flash{Action: action, JobID: shown, From: jobID})
	http.Redirect(w, r, "/admin/jobs/"+url.PathEscape(shown), http.StatusSeeOther)
}

// erased shows the result of an erase once: the Docs document ID.
func (u *UI) erased(w http.ResponseWriter, r *http.Request) {
	f, ok := u.readFlash(w, r)
	var data any
	if ok && f.Action == "erase" {
		data = f
	}
	u.render(w, http.StatusOK, "erased", view{Title: "Job erased", Session: session(r), Data: data})
}

// audit is the audit list, newest first, optionally of one job.
func (u *UI) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := admin.AuditFilter{JobID: strings.ToLower(q.Get("job_id")), Cursor: q.Get("cursor"), Limit: pageSize}
	page, err := u.cfg.Admin.ListAudit(r.Context(), f)
	if err != nil {
		u.fail(w, r, err, "")
		return
	}
	keep := url.Values{}
	for name, v := range map[string]string{"job_id": f.JobID, "cursor": f.Cursor} {
		if v != "" {
			keep.Set(name, v)
		}
	}
	first, next := listLinks("/admin/audit", keep, f.Cursor, page.NextCursor)
	u.render(w, http.StatusOK, "audit", view{Title: "Audit", Session: session(r), Data: struct {
		Entries     []admin.AuditEntry
		JobID       string
		First, Next string
	}{page.Entries, f.JobID, first, next}})
}

// fail answers an error of the admin functions with a message page and its
// status. An unexpected error is logged by its class only.
func (u *UI) fail(w http.ResponseWriter, r *http.Request, err error, back string) {
	v := view{Session: session(r), Back: back}
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, admin.ErrNotFound):
		status, v.Title, v.Message = http.StatusNotFound, "Not found", "No such job (404 not-found). It may have been erased."
	case errors.Is(err, admin.ErrInvalidState):
		status, v.Title, v.Message = http.StatusConflict, "Not possible in this state",
			"The job's state does not allow this action (409 invalid-job-state). Nothing was changed."
	case errors.Is(err, admin.ErrDocumentIDNotAllowed):
		v.Title, v.Message = "Invalid request", "A document ID applies only to release of a delivery_uncertain job (400 invalid-request)."
	case errors.Is(err, admin.ErrInvalidDocumentID):
		v.Title, v.Message = "Invalid request", "The document ID must be 1 to 255 bytes (400 invalid-request)."
	case errors.Is(err, admin.ErrInvalidFilter):
		// Back to the list the filter came from (/admin/ or /admin/audit), without the filter.
		v.Title, v.Message, v.Back = "Invalid request", "Invalid filter or page link (400 invalid-request).", r.URL.Path
	default:
		status, v.Title, v.Message = http.StatusInternalServerError, "Error", "Internal error (500)."
		u.cfg.Log.ErrorContext(r.Context(), "admin UI request failed", "path", r.Pattern, "error_class", errorClass(err))
	}
	u.page(w, status, v)
}

// errorClass names an error without its text, which may echo values
// (privacy.md §6): the SQLSTATE of a Postgres error, else the Go type.
func errorClass(err error) string {
	var pgErr interface{ SQLState() string }
	switch {
	case errors.As(err, &pgErr):
		return "sqlstate-" + pgErr.SQLState()
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline-exceeded"
	}
	return fmt.Sprintf("%T", err)
}

func (u *UI) setFlash(w http.ResponseWriter, f flash) {
	f.Expires = u.now().Add(flashTTL).Unix()
	v, err := u.sign.encode(flashCookie, f)
	if err != nil {
		return // the action is done; only its one-line result is lost
	}
	setCookie(w, flashCookie, v, int(flashTTL.Seconds()))
}

// readFlash returns the flash cookie, if valid, and clears it.
func (u *UI) readFlash(w http.ResponseWriter, r *http.Request) (flash, bool) {
	if _, err := r.Cookie(flashCookie); err != nil {
		return flash{}, false
	}
	clearCookie(w, flashCookie)
	var f flash
	err := u.sign.read(r, flashCookie, &f, func() int64 { return f.Expires }, u.now())
	return f, err == nil
}

// takeFlash returns the one-line result for the page of jobID, if the last
// action left one for it.
func (u *UI) takeFlash(w http.ResponseWriter, r *http.Request, jobID string) string {
	f, ok := u.readFlash(w, r)
	if !ok || f.JobID != jobID {
		return ""
	}
	switch f.Action {
	case "retry":
		return "Retry done: the step runs again."
	case "release":
		return "Release done: the hold is lifted."
	case "purge":
		return "Purge done: cleanup runs and the job ends as cleaned (purged)."
	case "rerun":
		return "Rerun started: this is the new job, rerun of " + f.From + "."
	}
	return ""
}
