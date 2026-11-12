package ingest

import (
	"net/http"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
)

// Authenticated requests are counted by BBB server and outcome (ok or the
// problem slug); unauthenticated ones are not.
func TestIngestRequestsCounted(t *testing.T) {
	e := newEnv(t)
	reg := metrics.NewAPIRegistry()
	value := func(outcome string) float64 {
		return metricstest.Value(t, reg, "recap_ingest_requests_total",
			"bbb_server_id", "bbb-fixture-a", "outcome", outcome)
	}
	ok, unknown := value("ok"), value(problemUnknownPackage)

	p := golden(t)
	wantStatus(t, "GET unknown", e.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusNotFound)
	wantStatus(t, "manifest PUT", e.putManifest(p), http.StatusCreated)
	e2 := *e
	e2.token = "wrong"
	wantStatus(t, "unauthenticated GET", e2.do(http.MethodGet, pkgPath(p.id), nil, 0), http.StatusUnauthorized)

	if got := value("ok") - ok; got != 1 {
		t.Errorf("ok counted %v times, want 1", got)
	}
	if got := value(problemUnknownPackage) - unknown; got != 1 {
		t.Errorf("unknown-package counted %v times, want 1", got)
	}
	if got := metricstest.Value(t, reg, "recap_ingest_requests_total", "outcome", "unauthorized"); got != 0 {
		t.Errorf("unauthorized counted %v times, want 0", got)
	}
}

func TestOutcomeBounded(t *testing.T) {
	for _, c := range []struct {
		rec  outcomeRecorder
		want string
	}{
		{outcomeRecorder{status: 0}, "ok"},
		{outcomeRecorder{status: 204}, "ok"},
		{outcomeRecorder{status: 409, slug: problemPackageCommitted}, problemPackageCommitted},
		{outcomeRecorder{status: 400, slug: "something-new"}, "other"},
		{outcomeRecorder{status: 500, slug: "ok"}, "other"},
		{outcomeRecorder{status: 500}, "other"},
	} {
		if got := c.rec.outcome(); got != c.want {
			t.Errorf("status %d slug %q: outcome %q, want %q", c.rec.status, c.rec.slug, got, c.want)
		}
	}
}
