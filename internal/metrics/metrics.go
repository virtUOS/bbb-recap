// Package metrics holds the Prometheus collectors of docs/reference/architecture.md
// §12 and small helpers to record them.
//
// Labels carry only configuration names (profiles, client IDs) and enum
// values of docs/reference/README.md, never job, meeting, record, document or user
// IDs (privacy.md §6). Each process serves its own registry on the ops
// listener: NewAPIRegistry or NewWorkerRegistry.
package metrics

import (
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Values of the kind label of recap_external_requests_total (§12).
const (
	KindASR        = "asr"
	KindASRCleanup = "asr_cleanup"
	KindLLM        = "llm"
	KindSink       = "sink"
)

// OK is the outcome label of a successful request.
const OK = "ok"

// Values of the dependency label of recap_dependency_up (§12).
const (
	DependencyPostgres  = "postgres"
	DependencyBlobstore = "blobstore"
)

var (
	stepRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "recap_step_runs_total",
		Help: "Step runs whose result was recorded, by step and step outcome.",
	}, []string{"step", "outcome"})

	stepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "recap_step_duration_seconds",
		Help:    "Time a step handler ran, by step.",
		Buckets: prometheus.DefBuckets,
	}, []string{"step"})

	leaseExpired = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "recap_lease_expired_total",
		Help: "Claimed steps reaped after their lease expired, by step.",
	}, []string{"step"})

	externalRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "recap_external_requests_total",
		Help: "Requests to providers and sinks, by profile, kind and outcome (ok or the adapter error class).",
	}, []string{"profile", "kind", "outcome"})

	ingestRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "recap_ingest_requests_total",
		Help: "Authenticated ingest API requests, by BBB server and outcome (ok or the problem slug).",
	}, []string{"bbb_server_id", "outcome"})

	dependencyUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "recap_dependency_up",
		Help: "1 if the last readiness check reached the dependency, else 0.",
	}, []string{"dependency"})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "recap_build_info",
		Help: "Always 1; the labels carry the version and commit of the binary.",
	}, []string{"version", "commit"})
)

func init() {
	// Down until the first readiness check says otherwise.
	dependencyUp.WithLabelValues(DependencyPostgres).Set(0)
	dependencyUp.WithLabelValues(DependencyBlobstore).Set(0)
	buildInfo.WithLabelValues(build()).Set(1)
}

// build returns the module version and the VCS revision the binary was built
// from, or "unknown".
func build() (version, commit string) {
	version, commit = "unknown", "unknown"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit
	}
	if bi.Main.Version != "" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			commit = s.Value
		}
	}
	return version, commit
}

// newRegistry returns a registry with what both processes serve: Go and
// process collectors, recap_build_info and recap_dependency_up.
func newRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		dependencyUp,
	)
	return reg
}

// NewAPIRegistry returns the registry of the api process. The DB gauges are
// added with NewJobsCollector once the database is open.
func NewAPIRegistry() *prometheus.Registry {
	reg := newRegistry()
	reg.MustRegister(ingestRequests)
	return reg
}

// NewWorkerRegistry returns the registry of the worker process.
func NewWorkerRegistry() *prometheus.Registry {
	reg := newRegistry()
	reg.MustRegister(stepRuns, stepDuration, leaseExpired, externalRequests)
	return reg
}

// InitStep makes the series of a step visible at 0 before its first run:
// the run counter for each outcome, the duration and the expired leases.
func InitStep(step string, outcomes ...string) {
	for _, o := range outcomes {
		stepRuns.WithLabelValues(step, o)
	}
	stepDuration.WithLabelValues(step)
	leaseExpired.WithLabelValues(step)
}

// StepRun counts a step run whose result was recorded with outcome (a step
// outcome of docs/reference/README.md).
func StepRun(step, outcome string) { stepRuns.WithLabelValues(step, outcome).Inc() }

// StepDuration records how long a step handler ran.
func StepDuration(step string, d time.Duration) {
	stepDuration.WithLabelValues(step).Observe(d.Seconds())
}

// LeaseExpired counts a step reaped after its lease expired.
func LeaseExpired(step string) { leaseExpired.WithLabelValues(step).Inc() }

// ExternalRequest counts one request to a provider or sink profile. kind is
// one of the Kind constants; outcome is OK or the adapter error class.
func ExternalRequest(profile, kind, outcome string) {
	externalRequests.WithLabelValues(profile, kind, outcome).Inc()
}

// InitExternal makes the series of a profile visible at 0 before its first
// request.
func InitExternal(profile, kind string, outcomes ...string) {
	for _, o := range outcomes {
		externalRequests.WithLabelValues(profile, kind, o)
	}
}

// IngestRequest counts one authenticated ingest request. bbbServerID is the
// authenticated client ID (a configured credential name); outcome is OK or
// a problem slug from a fixed list.
func IngestRequest(bbbServerID, outcome string) {
	ingestRequests.WithLabelValues(bbbServerID, outcome).Inc()
}

// SetDependencyUp records the result of a readiness check of dependency.
func SetDependencyUp(dependency string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	dependencyUp.WithLabelValues(dependency).Set(v)
}
