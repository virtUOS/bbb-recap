package metrics_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/virtUOS/bbb-recap/internal/admin"
	"github.com/virtUOS/bbb-recap/internal/db"
	"github.com/virtUOS/bbb-recap/internal/db/dbtest"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

var ctx = context.Background()

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// seedJob creates a committed package and its job (received, at validate,
// every step pending and due now), then runs update on it.
func seedJob(t *testing.T, pool *pgxpool.Pool, update string) string {
	t.Helper()
	id := queue.NewJobID()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		pkg := randomHex(32)
		if _, err := tx.Exec(ctx, `
			INSERT INTO packages (package_id, job_id, bbb_server_id, record_id, meeting_id, state, total_bytes)
			VALUES ($1, $2, 'bbb-fixture-a', $3, 'meeting-fixture', 'committed', 0)`, pkg, id, randomHex(20)); err != nil {
			return err
		}
		if err := queue.CreateJob(ctx, tx, queue.NewJob{JobID: id, PackageID: pkg, ConfigHash: randomHex(32)}); err != nil {
			return err
		}
		if update != "" {
			_, err := tx.Exec(ctx, update, id)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

// jobsRegistry returns a registry with only the DB gauges, and the
// collector's clock.
func jobsRegistry(pool *pgxpool.Pool) (*prometheus.Registry, *time.Time) {
	c := metrics.NewJobsCollector(pool, queue.Steps, slog.New(slog.DiscardHandler))
	now := time.Now()
	metrics.SetJobsClock(c, func() time.Time { return now })
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	return reg, &now
}

func TestJobsCollector(t *testing.T) {
	pool := dbtest.New(t)
	// validate due 10 minutes ago.
	seedJob(t, pool, `UPDATE steps SET run_after = now() - interval '10 minutes' WHERE job_id = $1`)
	// validate not due yet: not ready.
	seedJob(t, pool, `UPDATE steps SET run_after = now() + interval '1 hour' WHERE job_id = $1`)
	seedJob(t, pool, `UPDATE jobs SET state = 'held', held_reason = 'delivery_uncertain', current_step = 'deliver'
		WHERE job_id = $1`)
	seedJob(t, pool, `UPDATE jobs SET state = 'failed', failure_reason = 'retries_exhausted' WHERE job_id = $1`)
	// Delivered; cleanup due 2 h ago: ready and overdue.
	seedJob(t, pool, `WITH j AS (UPDATE jobs SET state = 'delivered', current_step = 'cleanup' WHERE job_id = $1)
		UPDATE steps SET run_after = now() - interval '2 hours' WHERE job_id = $1 AND name = 'cleanup'`)
	// Delivered; cleanup due 30 min ago: not overdue yet.
	seedJob(t, pool, `WITH j AS (UPDATE jobs SET state = 'delivered', current_step = 'cleanup' WHERE job_id = $1)
		UPDATE steps SET run_after = now() - interval '30 minutes' WHERE job_id = $1 AND name = 'cleanup'`)

	reg, now := jobsRegistry(pool)
	v := func(name string, labels ...string) float64 { return metricstest.Value(t, reg, name, labels...) }
	for _, c := range []struct {
		name   string
		labels []string
		want   float64
	}{
		{"recap_jobs", []string{"state", "received"}, 2},
		{"recap_jobs", []string{"state", "held"}, 1},
		{"recap_jobs", []string{"state", "failed"}, 1},
		{"recap_jobs", []string{"state", "delivered"}, 2},
		{"recap_jobs", []string{"state", "cleaned"}, 0},
		{"recap_jobs_held", []string{"reason", "delivery_uncertain"}, 1},
		{"recap_jobs_held", []string{"reason", "no_registration"}, 0},
		{"recap_jobs_failed", []string{"reason", "retries_exhausted"}, 1},
		{"recap_jobs_failed", []string{"reason", "config_error"}, 0},
		{"recap_cleanup_overdue_jobs", nil, 1},
		{"recap_step_oldest_ready_age_seconds", []string{"step", "transcribe"}, 0},
	} {
		if got := v(c.name, c.labels...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if age := v("recap_step_oldest_ready_age_seconds", "step", "validate"); age < 590 || age > 700 {
		t.Errorf("validate age = %v, want about 600", age)
	}
	if age := v("recap_step_oldest_ready_age_seconds", "step", "cleanup"); age < 7190 || age > 7300 {
		t.Errorf("cleanup age = %v, want about 7200", age)
	}

	// Cached for 15 s.
	seedJob(t, pool, "")
	*now = now.Add(metrics.JobsCacheTime - time.Second)
	if got := v("recap_jobs", "state", "received"); got != 2 {
		t.Errorf("within 15 s: received = %v, want the cached 2", got)
	}
	*now = now.Add(time.Second)
	if got := v("recap_jobs", "state", "received"); got != 3 {
		t.Errorf("after 15 s: received = %v, want 3", got)
	}
}

// The job gauges report exactly the values of the shared enum lists, the same
// lists the admin filters accept.
func TestJobsSeriesMatchEnums(t *testing.T) {
	reg, _ := jobsRegistry(dbtest.New(t))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				got[f.GetName()] = append(got[f.GetName()], l.GetValue())
			}
		}
	}
	for name, want := range map[string][]string{
		"recap_jobs":        db.JobStates,
		"recap_jobs_held":   db.HeldReasons,
		"recap_jobs_failed": db.FailureReasons,
	} {
		if !sameSet(got[name], want) {
			t.Errorf("%s: label values %v, want %v", name, got[name], want)
		}
	}
	if !slices.Equal(admin.JobStates, db.JobStates) || !slices.Equal(admin.HeldReasons, db.HeldReasons) {
		t.Errorf("admin filters %v, %v differ from %v, %v", admin.JobStates, admin.HeldReasons, db.JobStates, db.HeldReasons)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b))
	return slices.Equal(a, b)
}
