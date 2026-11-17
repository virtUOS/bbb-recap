package queue

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sink statuses of a job with several sinks (docs/reference/sinks.md §1.8,
// ADR-016). sent, confirmed_by_admin and skipped_by_admin are done.
const (
	SinkPending          = "pending"
	SinkSending          = "sending"
	SinkSent             = "sent"
	SinkConfirmedByAdmin = "confirmed_by_admin"
	SinkSkippedByAdmin   = "skipped_by_admin"
	SinkFailed           = "failed"
)

// SinkStatus is the status of one sink of a job, from its deliveries row
// (sending and the done states) or its deliver step item (failed).
type SinkStatus struct {
	Profile    string
	Status     string
	DocumentID *string
	Target     *string // Matrix room ID the event went to (sinks.md §1.3)
	SentAt     *time.Time
	// FailureReason is set only for failed: config_error, rejected_by_sink
	// or retries_exhausted.
	FailureReason *string
	// Detail is the failed sink's allow-listed code, if any.
	Detail *string
}

// Done reports whether the sink is done: sent, confirmed_by_admin or
// skipped_by_admin.
func (s SinkStatus) Done() bool {
	return s.Status == SinkSent || s.Status == SinkConfirmedByAdmin || s.Status == SinkSkippedByAdmin
}

// SinkList splits jobs.sink_profile, the job's sink list in delivery order
// (comma-separated, set by validate); nil before validate stored it.
func SinkList(v *string) []string {
	if v == nil || *v == "" {
		return nil
	}
	return strings.Split(*v, ",")
}

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// sinkStatusSelect reads the sinks of a job in delivery order with their
// record and failed item.
const sinkStatusSelect = `
	SELECT p.profile, d.state, d.document_id, d.target, d.sent_at, i.failure_reason, i.detail
	FROM jobs j
	CROSS JOIN LATERAL unnest(string_to_array(j.sink_profile, ',')) WITH ORDINALITY AS p(profile, n)
	LEFT JOIN deliveries d ON d.job_id = j.job_id AND d.sink_profile = p.profile
	LEFT JOIN step_items i ON i.job_id = j.job_id AND i.step = 'deliver' AND i.item = p.profile
		AND i.state = 'failed'
	WHERE j.job_id = $1
	ORDER BY p.n`

// SinkStatuses returns the statuses of the job's sinks in delivery order;
// empty before validate stored the list, or for an unknown job.
func SinkStatuses(ctx context.Context, q Querier, jobID string) ([]SinkStatus, error) {
	rows, err := q.Query(ctx, sinkStatusSelect, jobID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SinkStatus, error) {
		var s SinkStatus
		var state *string
		err := row.Scan(&s.Profile, &state, &s.DocumentID, &s.Target, &s.SentAt, &s.FailureReason, &s.Detail)
		switch {
		case state != nil:
			s.Status, s.FailureReason, s.Detail = *state, nil, nil
		case s.FailureReason != nil:
			s.Status = SinkFailed
		default:
			s.Status = SinkPending
		}
		return s, err
	})
}

// markExhausted marks every sink of the job that is still pending failed
// with retries_exhausted: the deliver step ran out of attempts (sinks.md
// §1.8), so an admin can retry or skip each of them. It runs in the
// completion transaction, also when a reaped lease used up the last
// attempt.
func markExhausted(ctx context.Context, tx pgx.Tx, jobID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO step_items (job_id, step, item, state, failure_reason)
		SELECT j.job_id, 'deliver', p.profile, 'failed', 'retries_exhausted'
		FROM jobs j CROSS JOIN LATERAL unnest(string_to_array(j.sink_profile, ',')) AS p(profile)
		WHERE j.job_id = $1
			AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.job_id = j.job_id AND d.sink_profile = p.profile)
		ON CONFLICT (job_id, step, item) DO NOTHING`, jobID)
	return err
}
