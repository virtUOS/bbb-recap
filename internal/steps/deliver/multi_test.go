package deliver

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/sink"
)

// Several sinks per job (sinks.md §1.1, §1.8; ADR-016): delivery groups,
// the wait for the front end's pickup, the links, partial failure and the
// last attempt.

const (
	hookProfile = "webhook-main"
	resultURL   = "https://pilos.recap.test/rooms/abc-def-123/summaries"
)

// fakeHook is a group 2 sender that records each request's links.
type fakeHook struct {
	mu    sync.Mutex
	links [][]sink.Link
	errs  []error
}

func (f *fakeHook) Send(_ context.Context, req sink.Request) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links = append(f.links, req.Links)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", err
	}
	return "", nil
}

func (f *fakeHook) calls() [][]sink.Link {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.links)
}

// newMultiEnv is newFrontendEnv with a docs link template and a webhook
// profile; the handler waits for pickups in the env's store.
func newMultiEnv(t *testing.T) (*env, blob.Results, *fakeHook) {
	t.Helper()
	e, results := newFrontendEnv(t)
	d := e.deps.Sinks[profile]
	d.Type, d.DocumentURLTemplate = TypeDocs, "https://docs.recap.test/docs/{id}/"
	e.deps.Sinks[profile] = d
	f := e.deps.Sinks[frontendProfile]
	f.Type = TypeFrontend
	e.deps.Sinks[frontendProfile] = f
	hook := &fakeHook{}
	e.deps.Sinks[hookProfile] = Sink{Type: TypeWebhook, Sender: hook}
	e.deps.Results = results
	return e, results, hook
}

// seedMulti seeds a job with the sink list (delivery order) and, if not
// empty, the registration's result_url.
func (e *env) seedMulti(t *testing.T, list, url string) string {
	t.Helper()
	jobID := e.seed(t, summaryMD)
	e.exec(t, `UPDATE jobs SET sink_profile = $2 WHERE job_id = $1`, jobID, list)
	if url != "" {
		e.exec(t, `UPDATE registrations SET result_url = $2
			WHERE registration_id = (SELECT registration_id FROM jobs WHERE job_id = $1)`, jobID, url)
	}
	return jobID
}

func (e *env) statuses(t *testing.T, jobID string) []string {
	t.Helper()
	sinks, err := queue.SinkStatuses(ctx, e.deps.Pool, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range sinks {
		out = append(out, s.Profile+"="+s.Status)
	}
	return out
}

func expectStatuses(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}
}

// Group 1 first; group 2 waits for the pickup of the summary, then gets the
// result_url and the Docs link (sinks.md §1.2, §1.8).
func TestGroupsAndPickup(t *testing.T) {
	e, results, hook := newMultiEnv(t)
	jobID := e.seedMulti(t, "frontend-main,docs-main,webhook-main", resultURL)

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Transient || r.ErrorClass != "wait" || r.ErrorCode != "pickup_pending" {
		t.Fatalf("first attempt %+v, want transient while the summary waits for pickup", r)
	}
	if len(hook.calls()) != 0 {
		t.Fatal("group 2 called before the pickup")
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "frontend-main=sent", "docs-main=sent", "webhook-main=pending")

	// Still not picked up: the next attempt neither sends again nor calls group 2.
	run = e.claim(t, jobID)
	r = Handler(e.deps)(ctx, run)
	e.complete(t, run, r)
	if e.docs.requests() != 1 || len(hook.calls()) != 0 || r.ErrorCode != "pickup_pending" {
		t.Fatalf("second attempt: %d Docs requests, %d webhook calls, %+v", e.docs.requests(), len(hook.calls()), r)
	}

	if err := results.DeleteResults(ctx, jobID); err != nil { // the front end's DELETE …/summary
		t.Fatal(err)
	}
	run = e.claim(t, jobID)
	r = Handler(e.deps)(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("after the pickup: %+v, want ok", r)
	}
	e.complete(t, run, r)
	calls := hook.calls()
	want := []sink.Link{{SinkProfile: frontendProfile, URL: resultURL},
		{SinkProfile: profile, URL: "https://docs.recap.test/docs/doc-1/"}}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("webhook calls %v, want one with %v", calls, want)
	}
	expectStatuses(t, e.statuses(t, jobID), "frontend-main=sent", "docs-main=sent", "webhook-main=sent")
	if e.docs.requests() != 1 {
		t.Errorf("%d Docs requests, want 1", e.docs.requests())
	}
}

// Without a result_url nothing waits for the pickup, and the links hold
// only the Docs link; group 2 waits for a group 1 sink that was not sent.
func TestGroup2WaitsForGroup1(t *testing.T) {
	e, _, hook := newMultiEnv(t)
	e.docs.queue(status(http.StatusTooManyRequests, `{"detail": "mock-docs: throttled"}`))
	jobID := e.seedMulti(t, "frontend-main,docs-main,webhook-main", "")

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Transient || r.ErrorClass != "not_sent" || len(hook.calls()) != 0 {
		t.Fatalf("first attempt %+v, %d webhook calls; want transient not_sent, none", r, len(hook.calls()))
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "frontend-main=sent", "docs-main=pending", "webhook-main=pending")

	run = e.claim(t, jobID)
	r = Handler(e.deps)(ctx, run)
	if r.Outcome != queue.OK {
		t.Fatalf("second attempt %+v, want ok", r)
	}
	e.complete(t, run, r)
	want := []sink.Link{{SinkProfile: profile, URL: "https://docs.recap.test/docs/doc-2/"}}
	if calls := hook.calls(); len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("webhook calls %v, want one with %v", calls, want)
	}
}

// A failed sink fails the job after the other group 1 sinks were called;
// group 2 is not called; the failure stays on the sink (sinks.md §1.8).
func TestPartialFailure(t *testing.T) {
	e, results, hook := newMultiEnv(t)
	e.docs.queue(status(http.StatusBadRequest, `{"content": ["mock-docs: Could not convert content"]}`))
	jobID := e.seedMulti(t, "docs-main,frontend-main,webhook-main", "")

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Permanent || r.Reason != ReasonRejectedBySink {
		t.Fatalf("%+v, want permanent rejected_by_sink", r)
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "docs-main=failed", "frontend-main=sent", "webhook-main=pending")
	if _, ok := stored(t, results, jobID); !ok {
		t.Error("the frontend sink was not delivered in the same attempt")
	}
	if len(hook.calls()) != 0 {
		t.Error("group 2 called although group 1 failed")
	}
	sinks, _ := queue.SinkStatuses(ctx, e.deps.Pool, jobID)
	if d := sinks[0]; d.FailureReason == nil || *d.FailureReason != ReasonRejectedBySink || d.Detail == nil || *d.Detail != "http_400" {
		t.Errorf("failed sink %+v, want rejected_by_sink with code http_400", d)
	}
}

// An uncertain group 2 sink holds the job; the done sinks are not sent
// again.
func TestUncertainWebhookHolds(t *testing.T) {
	e, _, hook := newMultiEnv(t)
	hook.errs = []error{&sink.Error{Class: sink.Uncertain, Code: "http_500"}}
	jobID := e.seedMulti(t, "docs-main,webhook-main", "")

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Hold || r.Reason != HeldDeliveryUncertain {
		t.Fatalf("%+v, want hold delivery_uncertain", r)
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "docs-main=sent", "webhook-main=sending")
	run = e.claim(t, jobID)
	if r = Handler(e.deps)(ctx, run); r.Outcome != queue.Hold || e.docs.requests() != 1 || len(hook.calls()) != 1 {
		t.Fatalf("second attempt %+v with %d Docs requests and %d webhook calls; want the hold, nothing sent",
			r, e.docs.requests(), len(hook.calls()))
	}
}

// A profile removed from the configuration fails only its own sink with
// config_error; the other sinks are still called, and a done sink with a
// removed profile is not looked at (sinks.md §1.8).
func TestMissingProfile(t *testing.T) {
	e, _, _ := newMultiEnv(t)
	jobID := e.seedMulti(t, "frontend-main,removed-docs,docs-main", "")

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Permanent || r.Reason != ReasonConfigError || r.ErrorCode != "unknown_sink_profile" {
		t.Fatalf("%+v, want permanent config_error unknown_sink_profile", r)
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "frontend-main=sent", "removed-docs=failed", "docs-main=sent")
	sinks, _ := queue.SinkStatuses(ctx, e.deps.Pool, jobID)
	if s := sinks[1]; s.FailureReason == nil || *s.FailureReason != ReasonConfigError {
		t.Errorf("removed sink %+v, want config_error", s)
	}

	// A sent sink whose profile is gone later does not stop the others.
	other := e.seedMulti(t, "frontend-main,docs-main", "")
	e.exec(t, `UPDATE jobs SET sink_profile = 'gone-main,docs-main' WHERE job_id = $1`, other)
	e.exec(t, `INSERT INTO deliveries (job_id, sink_profile, state, sent_at) VALUES ($1, 'gone-main', 'sent', now())`, other)
	run = e.claim(t, other)
	if r = Handler(e.deps)(ctx, run); r.Outcome != queue.OK {
		t.Fatalf("%+v, want ok", r)
	}
}

// recipient_unresolved next to a frontend sink: the job holds
// owner_unresolved with the frontend sink sent; after the resume the Docs
// sink is sent and the frontend sink is not stored again.
func TestOwnerUnresolvedWithFrontend(t *testing.T) {
	e, results, _ := newMultiEnv(t)
	e.docs.queue(status(http.StatusBadRequest, `{"email": ["mock-docs: two users"]}`))
	jobID := e.seedMulti(t, "docs-main,frontend-main", "")

	run := e.claim(t, jobID)
	r := Handler(e.deps)(ctx, run)
	if r.Outcome != queue.Hold || r.Reason != HeldOwnerUnresolved {
		t.Fatalf("%+v, want hold owner_unresolved", r)
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "docs-main=pending", "frontend-main=sent")
	if err := results.DeleteResults(ctx, jobID); err != nil { // picked up meanwhile
		t.Fatal(err)
	}
	run = e.claim(t, jobID)
	if r = Handler(e.deps)(ctx, run); r.Outcome != queue.OK {
		t.Fatalf("after the resume: %+v, want ok", r)
	}
	e.complete(t, run, r)
	expectStatuses(t, e.statuses(t, jobID), "docs-main=sent", "frontend-main=sent")
	if _, ok := stored(t, results, jobID); ok {
		t.Error("the frontend sink was stored again")
	}
}
