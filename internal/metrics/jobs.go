package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// JobsCacheTime is how long the DB gauges are kept between queries
// (architecture.md §12: computed by api on scrape, cached 15 s).
const JobsCacheTime = 15 * time.Second

// CleanupOverdueAfter: a job is cleanup-overdue when its cleanup step is
// pending more than this after its run_after (docs/reference/README.md "HTTP").
const CleanupOverdueAfter = time.Hour

// The enum values of docs/reference/README.md "States and enums". Every value is
// reported, 0 when no job has it, so a scrape always lists every series.
var (
	jobStates   = []string{"received", "running", "held", "failed", "delivered", "cleaned"}
	heldReasons = []string{"invalid_metadata", "unknown_type", "unknown_sink", "no_registration",
		"owner_unresolved", "delivery_uncertain"}
	failureReasons = []string{"unsupported_audio_bridge", "events_mismatch", "manifest_metadata_mismatch",
		"retries_exhausted", "all_tracks_failed", "config_error", "rejected_by_provider", "rejected_by_sink"}
)

var (
	jobsDesc = prometheus.NewDesc("recap_jobs",
		"Jobs by state.", []string{"state"}, nil)
	jobsHeldDesc = prometheus.NewDesc("recap_jobs_held",
		"Held jobs by held reason.", []string{"reason"}, nil)
	jobsFailedDesc = prometheus.NewDesc("recap_jobs_failed",
		"Failed jobs by failure reason.", []string{"reason"}, nil)
	oldestReadyDesc = prometheus.NewDesc("recap_step_oldest_ready_age_seconds",
		"Age of the oldest runnable, unclaimed step since its run_after, by step; 0 when none.", []string{"step"}, nil)
	cleanupOverdueDesc = prometheus.NewDesc("recap_cleanup_overdue_jobs",
		"Jobs whose cleanup step is pending more than 1 h after its run_after.", nil, nil)
)

// JobsCollector computes the DB gauges of §12 on scrape and caches them for
// JobsCacheTime. On a database error the scrape has no DB gauges;
// recap_dependency_up shows why.
type JobsCollector struct {
	pool  *pgxpool.Pool
	steps []string
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	at      time.Time
	metrics []prometheus.Metric
}

// NewJobsCollector returns the DB gauges collector of the api process. steps
// are the step names (queue.Steps).
func NewJobsCollector(pool *pgxpool.Pool, steps []string, log *slog.Logger) *JobsCollector {
	return &JobsCollector{pool: pool, steps: steps, log: log, now: time.Now}
}

// Describe implements prometheus.Collector.
func (c *JobsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{jobsDesc, jobsHeldDesc, jobsFailedDesc, oldestReadyDesc, cleanupOverdueDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *JobsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metrics == nil || c.now().Sub(c.at) >= JobsCacheTime {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m, err := c.query(ctx)
		if err != nil {
			c.log.Debug("metrics: cannot read the job gauges", "err", err)
			c.metrics = nil
			return
		}
		c.metrics, c.at = m, c.now()
	}
	for _, m := range c.metrics {
		ch <- m
	}
}

// query reads all DB gauges.
func (c *JobsCollector) query(ctx context.Context) ([]prometheus.Metric, error) {
	states, held, failed := zeros(jobStates), zeros(heldReasons), zeros(failureReasons)
	rows, err := c.pool.Query(ctx, `
		SELECT state, coalesce(held_reason, ''), coalesce(failure_reason, ''), count(*)::float8
		FROM jobs GROUP BY 1, 2, 3`)
	if err != nil {
		return nil, err
	}
	var state, heldReason, failureReason string
	var n float64
	_, err = pgx.ForEachRow(rows, []any{&state, &heldReason, &failureReason, &n}, func() error {
		states[state] += n
		if state == "held" && heldReason != "" {
			held[heldReason] += n
		}
		if state == "failed" && failureReason != "" {
			failed[failureReason] += n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Runnable and unclaimed: the claim predicate of the queue
	// (architecture.md §6.2).
	ages := zeros(c.steps)
	rows, err = c.pool.Query(ctx, `
		SELECT s.name, extract(epoch FROM now() - min(s.run_after))::float8
		FROM steps s JOIN jobs j ON j.job_id = s.job_id
		WHERE s.state IN ('pending', 'retrying') AND s.run_after <= now() AND s.name = j.current_step
			AND (j.state IN ('received', 'running') OR (j.state = 'delivered' AND s.name = 'cleanup'))
		GROUP BY s.name`)
	if err != nil {
		return nil, err
	}
	var step string
	var age float64
	if _, err := pgx.ForEachRow(rows, []any{&step, &age}, func() error {
		ages[step] = age
		return nil
	}); err != nil {
		return nil, err
	}

	var overdue float64
	if err := c.pool.QueryRow(ctx, `
		SELECT count(*)::float8 FROM steps s JOIN jobs j ON j.job_id = s.job_id
		WHERE s.name = 'cleanup' AND j.current_step = 'cleanup' AND s.state IN ('pending', 'retrying')
			AND s.run_after < now() - $1 * interval '1 millisecond'`,
		CleanupOverdueAfter.Milliseconds()).Scan(&overdue); err != nil {
		return nil, err
	}

	var out []prometheus.Metric
	out = gauges(out, jobsDesc, states)
	out = gauges(out, jobsHeldDesc, held)
	out = gauges(out, jobsFailedDesc, failed)
	out = gauges(out, oldestReadyDesc, ages)
	out = append(out, prometheus.MustNewConstMetric(cleanupOverdueDesc, prometheus.GaugeValue, overdue))
	return out, nil
}

func zeros(keys []string) map[string]float64 {
	m := make(map[string]float64, len(keys))
	for _, k := range keys {
		m[k] = 0
	}
	return m
}

func gauges(out []prometheus.Metric, d *prometheus.Desc, values map[string]float64) []prometheus.Metric {
	for label, v := range values {
		out = append(out, prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, label))
	}
	return out
}
