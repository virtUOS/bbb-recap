package queue

import (
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
)

// The engine counts recorded runs by outcome, observes their duration and
// counts reaped leases.
func TestEngineMetrics(t *testing.T) {
	pool := newDB(t)
	reg := metrics.NewWorkerRegistry()
	value := func(name string, labels ...string) float64 { return metricstest.Value(t, reg, name, labels...) }
	runs := value("recap_step_runs_total", "step", "validate", "outcome", "ok")
	durations := value("recap_step_duration_seconds", "step", "validate")
	expired := value("recap_lease_expired_total", "step", "validate")

	e := newEngine(pool, "w1", "validate")
	id := seedJob(t, pool)
	e.runStep(ctx, ctx, mustClaim(t, e, id, "validate"))

	other := seedJob(t, pool)
	mustClaim(t, e, other, "validate")
	expireLease(t, pool, other, "validate")
	e.reap(ctx)

	if got := value("recap_step_runs_total", "step", "validate", "outcome", "ok") - runs; got != 1 {
		t.Errorf("ok runs counted %v, want 1", got)
	}
	if got := value("recap_step_duration_seconds", "step", "validate") - durations; got != 1 {
		t.Errorf("durations observed %v, want 1", got)
	}
	if got := value("recap_lease_expired_total", "step", "validate") - expired; got != 1 {
		t.Errorf("expired leases counted %v, want 1", got)
	}
}

// A zero Drain means DefaultDrain.
func TestDefaultDrain(t *testing.T) {
	if e := New(nil, Config{}); e.drain != DefaultDrain {
		t.Errorf("drain = %v, want %v", e.drain, DefaultDrain)
	}
	if e := New(nil, Config{Drain: time.Second}); e.drain != time.Second {
		t.Errorf("drain = %v, want 1s", e.drain)
	}
}
