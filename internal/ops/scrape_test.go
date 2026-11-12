package ops

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// The §12 metrics of each process (architecture.md §12, "DB gauges are
// computed by api").
var (
	apiMetrics = []string{"recap_build_info", "recap_cleanup_overdue_jobs", "recap_dependency_up",
		"recap_ingest_requests_total", "recap_jobs", "recap_jobs_failed", "recap_jobs_held",
		"recap_step_oldest_ready_age_seconds"}
	workerMetrics = []string{"recap_build_info", "recap_dependency_up", "recap_external_requests_total",
		"recap_lease_expired_total", "recap_step_duration_seconds", "recap_step_runs_total"}
)

// Label values that look like a UUID (job_id), a BBB internal meeting ID
// (record_id) or a SHA-256 (package_id) must never appear.
var idLike = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$|^[0-9a-f]{40}-[0-9]{13}$|^[0-9a-f]{64}$`)

var labelValue = regexp.MustCompile(`="((?:[^"\\]|\\.)*)"`)

// scrape GETs /metrics and returns the recap_ metric family names and every
// label value of recap_ samples.
func scrape(t *testing.T, g prometheus.Gatherer) (names, values []string) {
	t.Helper()
	code, body := get(t, New(discard, g, nil).Handler(), "/metrics")
	if code != 200 {
		t.Fatalf("/metrics = %d", code)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "recap_") {
			continue
		}
		name := line[:strings.IndexAny(line, "{ ")]
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suffix); ok && base == "recap_step_duration_seconds" {
				name = base
			}
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
		for _, m := range labelValue.FindAllStringSubmatch(line, -1) {
			values = append(values, m[1])
		}
	}
	slices.Sort(names)
	return names, values
}

func checkScrape(t *testing.T, g prometheus.Gatherer, want []string) {
	t.Helper()
	names, values := scrape(t, g)
	if !slices.Equal(names, want) {
		t.Errorf("recap_ metrics:\n got  %v\n want %v", names, want)
	}
	for _, v := range values {
		if idLike.MatchString(v) {
			t.Errorf("label value %q looks like an ID", v)
		}
	}
}

func TestScrapeAPI(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	// A job, so the gauges see an ID-bearing row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
		VALUES (repeat('a', 64), '0192f0c1-0000-7000-8000-000000000001', 'bbb-fixture-a',
			'0123456789abcdef0123456789abcdef01234567-1730000000000', 'meeting-fixture', 'committed', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (job_id, package_id, state, current_step, config_hash)
		VALUES ('0192f0c1-0000-7000-8000-000000000001', repeat('a', 64), 'received', 'validate', repeat('b', 64))`); err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewAPIRegistry()
	reg.MustRegister(metrics.NewJobsCollector(pool, queue.Steps, slog.New(slog.DiscardHandler)))
	metrics.IngestRequest("bbb-fixture-a", metrics.OK)
	checkScrape(t, reg, apiMetrics)
}

func TestScrapeWorker(t *testing.T) {
	reg := metrics.NewWorkerRegistry()
	queue.New(nil, queue.Config{Handlers: map[string]queue.Handler{
		"validate": func(context.Context, queue.Run) queue.Result { return queue.Result{} },
	}})
	metrics.InitExternal("whisperx-test", metrics.KindASR, metrics.OK)
	checkScrape(t, reg, workerMetrics)
}
