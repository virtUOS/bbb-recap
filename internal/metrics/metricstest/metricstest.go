// Package metricstest reads metric values from a registry in tests.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Value returns the value of the counter, gauge or histogram (its sample
// count) named name whose labels include all of labels (name, value pairs).
// A missing series is 0.
func Value(t testing.TB, g prometheus.Gatherer, name string, labels ...string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for i := 0; i+1 < len(labels); i += 2 {
				if have[labels[i]] != labels[i+1] {
					continue metrics
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}
