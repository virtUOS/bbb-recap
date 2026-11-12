package exporter

import (
	"net/http"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics are exactly the five series of §8.4. No record IDs as labels
// (docs/reference/README.md "Configuration, processes, metrics").
type metrics struct {
	reg        *prometheus.Registry
	recordings *prometheus.GaugeVec
	oldest     prometheus.Gauge
	deleted    *prometheus.CounterVec
	rawBytes   *prometheus.GaugeVec
	buildInfo  *prometheus.GaugeVec
}

func newMetrics() *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		recordings: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "recap_exporter_recordings", Help: "Recordings in the exporter state dir by state.",
		}, []string{"state"}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "recap_exporter_oldest_unacked_seconds",
			Help: "Age of the oldest record in ready or delivered since its first attempt; 0 when none.",
		}),
		deleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "recap_exporter_deleted_total", Help: "Raw recording dirs deleted by the exporter, by reason.",
		}, []string{"reason"}),
		rawBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "recap_exporter_raw_bytes",
			Help: "Package bytes (manifest totals) of recordings whose raw dir the exporter has not deleted, by state.",
		}, []string{"state"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "recap_exporter_build_info", Help: "Always 1; the label carries the version.",
		}, []string{"version"}),
	}
	m.reg.MustRegister(m.recordings, m.oldest, m.deleted, m.rawBytes, m.buildInfo)
	m.buildInfo.WithLabelValues(Version()).Set(1)
	for _, r := range []string{ReasonAfterAck, ReasonFailureExpiry, ReasonManual} {
		m.deleted.WithLabelValues(r)
	}
	m.set(nil, time.Now())
	return m
}

// recordInfo is what the gauges need of one record.
type recordInfo struct {
	state string
	since time.Time // first_attempt_at
	size  int64     // manifest totals.size, 0 without a manifest
}

// set recomputes the gauges from all records.
func (m *metrics) set(records []recordInfo, now time.Time) {
	count := map[string]float64{}
	bytes := map[string]float64{}
	var oldest time.Duration
	for _, r := range records {
		count[r.state]++
		if r.state != StateDeleted {
			bytes[r.state] += float64(r.size)
		}
		if (r.state == StateReady || r.state == StateDelivered) && !r.since.IsZero() {
			oldest = max(oldest, now.Sub(r.since))
		}
	}
	for _, s := range States {
		m.recordings.WithLabelValues(s).Set(count[s])
		if s != StateDeleted {
			m.rawBytes.WithLabelValues(s).Set(bytes[s])
		}
	}
	m.oldest.Set(oldest.Seconds())
}

// handler serves /metrics, /healthz and /readyz (§8.4). Readiness: config
// valid (the daemon runs only with a valid config), lock held (likewise) and
// the state dir writable. Nothing calls the service.
func (m *metrics) handler(stateDir string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := syscall.Access(stateDir, 2 /* W_OK */); err != nil {
			http.Error(w, "state dir not writable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
