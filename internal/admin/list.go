package admin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/httpapi"
)

// Paging of the job and audit lists (architecture.md §4).
const (
	defaultLimit = 50
	maxLimit     = 200
)

// Filter values (docs/reference/README.md "States and enums", "Identifiers").
var (
	// JobStates and HeldReasons are the allowed values of the state and
	// held_reason filters; the admin web UI offers them. Read only.
	JobStates   = []string{"received", "running", "held", "failed", "delivered", "cleaned"}
	HeldReasons = []string{"invalid_metadata", "unknown_type", "unknown_sink", "no_registration",
		"owner_unresolved", "delivery_uncertain"}
	outcomes   = []string{"delivered", "cancelled", "purged"}
	serverIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	auditIDRE  = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// listQuery collects WHERE conditions and their arguments. Every value goes
// in as a query argument, never into the SQL text.
type listQuery struct {
	where []string
	args  []any
}

// add appends cond, whose %d is replaced by the placeholder numbers of vals.
func (q *listQuery) add(cond string, vals ...any) {
	nums := make([]any, len(vals))
	for i, v := range vals {
		q.args = append(q.args, v)
		nums[i] = len(q.args)
	}
	q.where = append(q.where, fmt.Sprintf(cond, nums...))
}

func (q *listQuery) sql(base, order string, limit int) string {
	s := base
	if len(q.where) > 0 {
		s += " WHERE " + strings.Join(q.where, " AND ")
	}
	return s + " ORDER BY " + order + " LIMIT " + strconv.Itoa(limit+1)
}

// params checks the query string: only the allowed names, each at most once.
// It returns the values and the page size.
func params(r *http.Request, allowed ...string) (map[string]string, int, bool) {
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, 0, false
	}
	allowed = append(allowed, "limit", "cursor")
	out := map[string]string{}
	for name, v := range vals {
		// An empty value is no valid filter, limit or cursor.
		if len(v) != 1 || v[0] == "" || !slices.Contains(allowed, name) {
			return nil, 0, false
		}
		out[name] = v[0]
	}
	limit := defaultLimit
	if s, ok := out["limit"]; ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return nil, 0, false
		}
		limit = n
	}
	return out, limit, true
}

// The cursor is opaque to clients: the sort key (time and ID) of the last
// item of the previous page.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "," + id))
}

func decodeCursor(s string, idRE *regexp.Regexp) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, ok := strings.Cut(string(b), ",")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil || !idRE.MatchString(id) {
		return time.Time{}, "", false
	}
	return t, id, true
}

func badFilter(w http.ResponseWriter) {
	httpapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", "invalid filter, limit or cursor")
}

// JobFilter selects jobs for ListJobs. An empty field does not filter.
// CreatedAfter and CreatedBefore are RFC 3339; Cursor is the next cursor of
// the previous page; Limit 0 is the default page size.
type JobFilter struct {
	State, HeldReason, Outcome, BBBServerID string
	CreatedAfter, CreatedBefore             string
	Cursor                                  string
	Limit                                   int
}

// JobPage is one page of the job list, newest first. NextCursor is empty on
// the last page.
type JobPage struct {
	Jobs       []Job
	NextCursor string
}

// ListJobs returns one page of jobs, newest first (architecture.md §4). A
// value that is not allowed is ErrInvalidFilter.
func (a *API) ListJobs(ctx context.Context, f JobFilter) (JobPage, error) {
	limit, ok := pageSize(f.Limit)
	if !ok {
		return JobPage{}, ErrInvalidFilter
	}
	var q listQuery
	for _, c := range []struct {
		value, cond string
		valid       func(string) bool
	}{
		{f.State, "j.state = $%d", func(v string) bool { return slices.Contains(JobStates, v) }},
		{f.HeldReason, "j.held_reason = $%d", func(v string) bool { return slices.Contains(HeldReasons, v) }},
		{f.Outcome, "j.outcome = $%d", func(v string) bool { return slices.Contains(outcomes, v) }},
		{f.BBBServerID, "p.bbb_server_id = $%d", serverIDRE.MatchString},
	} {
		if c.value != "" {
			if !c.valid(c.value) {
				return JobPage{}, ErrInvalidFilter
			}
			q.add(c.cond, c.value)
		}
	}
	for _, c := range []struct{ value, cond string }{
		{f.CreatedAfter, "j.created_at >= $%d"},
		{f.CreatedBefore, "j.created_at < $%d"},
	} {
		if c.value != "" {
			t, err := time.Parse(time.RFC3339, c.value)
			if err != nil {
				return JobPage{}, ErrInvalidFilter
			}
			q.add(c.cond, t)
		}
	}
	if f.Cursor != "" {
		t, id, ok := decodeCursor(f.Cursor, jobIDRE)
		if !ok {
			return JobPage{}, ErrInvalidFilter
		}
		q.add("(j.created_at, j.job_id) < ($%d, $%d::uuid)", t, id)
	}

	rows, err := a.pool.Query(ctx, q.sql(jobSelect, "j.created_at DESC, j.job_id DESC", limit), q.args...)
	if err != nil {
		return JobPage{}, err
	}
	type item struct {
		job     Job
		created time.Time
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (item, error) {
		j, created, err := scanJob(row)
		return item{j, created}, err
	})
	if err != nil {
		return JobPage{}, err
	}
	page := JobPage{Jobs: []Job{}}
	for _, it := range items[:min(len(items), limit)] {
		page.Jobs = append(page.Jobs, it.job)
	}
	if len(items) > limit {
		last := items[limit-1]
		page.NextCursor = encodeCursor(last.created, last.job.JobID)
	}
	return page, nil
}

// pageSize checks a requested page size; 0 is the default.
func pageSize(n int) (int, bool) {
	if n == 0 {
		return defaultLimit, true
	}
	return n, n >= 1 && n <= maxLimit
}

// listJobs serves GET /api/v1/admin/jobs: newest first, filtered, in pages.
func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	p, limit, ok := params(r, "state", "held_reason", "outcome", "bbb_server_id", "created_after", "created_before")
	if !ok {
		badFilter(w)
		return
	}
	page, err := a.ListJobs(r.Context(), JobFilter{
		State: p["state"], HeldReason: p["held_reason"], Outcome: p["outcome"], BBBServerID: p["bbb_server_id"],
		CreatedAfter: p["created_after"], CreatedBefore: p["created_before"], Cursor: p["cursor"], Limit: limit,
	})
	if errors.Is(err, ErrInvalidFilter) {
		badFilter(w)
		return
	}
	if err != nil {
		a.internal(w, r, "admin job list failed", err)
		return
	}
	writeJSON(w, struct {
		Jobs       []Job   `json:"jobs"`
		NextCursor *string `json:"next_cursor"`
	}{page.Jobs, nonEmpty(page.NextCursor)})
}

// nonEmpty is nil for "", so JSON has null.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AuditEntry is one audit row (architecture.md §4, §6.1).
type AuditEntry struct {
	At        string  `json:"at"`
	ActorKind string  `json:"actor_kind"`
	ActorID   *string `json:"actor_id"`
	Action    string  `json:"action"`
	JobID     *string `json:"job_id"`
	Detail    *string `json:"detail"`
}

// AuditFilter selects audit rows for ListAudit, like JobFilter.
type AuditFilter struct {
	JobID  string
	Cursor string
	Limit  int
}

// AuditPage is one page of the audit list, newest first.
type AuditPage struct {
	Entries    []AuditEntry
	NextCursor string
}

// ListAudit returns one page of audit rows, newest first, optionally for
// one job. A value that is not allowed is ErrInvalidFilter.
func (a *API) ListAudit(ctx context.Context, f AuditFilter) (AuditPage, error) {
	limit, ok := pageSize(f.Limit)
	if !ok {
		return AuditPage{}, ErrInvalidFilter
	}
	var q listQuery
	if f.JobID != "" {
		if !jobIDRE.MatchString(f.JobID) {
			return AuditPage{}, ErrInvalidFilter
		}
		q.add("job_id = $%d::uuid", f.JobID)
	}
	if f.Cursor != "" {
		t, id, ok := decodeCursor(f.Cursor, auditIDRE)
		if !ok {
			return AuditPage{}, ErrInvalidFilter
		}
		q.add("(at, audit_id) < ($%d, $%d::bigint)", t, id)
	}
	rows, err := a.pool.Query(ctx, q.sql(`
		SELECT audit_id, at, actor_kind, actor_id, action, job_id::text, detail FROM audit`,
		"at DESC, audit_id DESC", limit), q.args...)
	if err != nil {
		return AuditPage{}, err
	}
	type item struct {
		entry AuditEntry
		id    int64
		at    time.Time
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (item, error) {
		var it item
		err := row.Scan(&it.id, &it.at, &it.entry.ActorKind, &it.entry.ActorID, &it.entry.Action,
			&it.entry.JobID, &it.entry.Detail)
		it.entry.At = formatTime(it.at)
		return it, err
	})
	if err != nil {
		return AuditPage{}, err
	}
	page := AuditPage{Entries: []AuditEntry{}}
	for _, it := range items[:min(len(items), limit)] {
		page.Entries = append(page.Entries, it.entry)
	}
	if len(items) > limit {
		last := items[limit-1]
		page.NextCursor = encodeCursor(last.at, strconv.FormatInt(last.id, 10))
	}
	return page, nil
}

// listAudit serves GET /api/v1/admin/audit: newest first, optionally for one
// job, in pages.
func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	p, limit, ok := params(r, "job_id")
	if !ok {
		badFilter(w)
		return
	}
	page, err := a.ListAudit(r.Context(), AuditFilter{JobID: p["job_id"], Cursor: p["cursor"], Limit: limit})
	if errors.Is(err, ErrInvalidFilter) {
		badFilter(w)
		return
	}
	if err != nil {
		a.internal(w, r, "admin audit list failed", err)
		return
	}
	writeJSON(w, struct {
		Entries    []AuditEntry `json:"entries"`
		NextCursor *string      `json:"next_cursor"`
	}{page.Entries, nonEmpty(page.NextCursor)})
}
