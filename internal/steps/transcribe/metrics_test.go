package transcribe_test

import (
	"testing"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/metrics/metricstest"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Every ASR request is counted with the profile and ok or the adapter error
// class; whisperx-family makes no cleanup request, so asr_cleanup stays 0.
func TestExternalRequestsCounted(t *testing.T) {
	e := newEnv(t, "4")
	reg := metrics.NewWorkerRegistry()
	value := func(kind, outcome string) float64 {
		return metricstest.Value(t, reg, "recap_external_requests_total",
			"profile", "whisperx-test", "kind", kind, "outcome", outcome)
	}
	ok, retryable := value("asr", "ok"), value("asr", "retryable")
	cleanup := metricstest.Value(t, reg, "recap_external_requests_total", "kind", "asr_cleanup")

	jobID, _ := e.job(t)
	e.fake.failing("trk-1", 503, nil, `{}`)
	expectOutcome(t, e.step(t, jobID, 1), queue.Transient, "", "retryable")

	if got := value("asr", "ok") - ok; got != 3 {
		t.Errorf("asr ok counted %v, want 3", got)
	}
	if got := value("asr", "retryable") - retryable; got != 1 {
		t.Errorf("asr retryable counted %v, want 1", got)
	}
	if got := metricstest.Value(t, reg, "recap_external_requests_total", "kind", "asr_cleanup") - cleanup; got != 0 {
		t.Errorf("asr_cleanup counted %v, want 0", got)
	}
}
