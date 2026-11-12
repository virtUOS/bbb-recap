package sink

import (
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// The frontend sink (sinks.md §4a, ADR-014): CT-SINK-12, and the
// document_url half of CT-REG-08 (metadata-interface.md §2.7) that needs
// this package's fault-injecting mock Docs.

const (
	// frontendProfile is the frontend profile of testdata/sink/sinks-frontend.yaml.
	frontendProfile = "frontend-main"
	// frontendContractSink is the contract environment's frontend profile
	// (tools/contract-env/config/sinks.yaml).
	frontendContractSink = "frontend-contract"
)

// testFrontendSink: deliver stores the exact bytes under
// results/<job_id>/summary.md, a second deliver overwrites them, nothing goes
// over the network, the §2.4 checks reject before anything is stored. The
// tool half needs RECAP_BIN, the service half the running service.
func testFrontendSink(t *testing.T) {
	t.Run("tool", testFrontendTool)
	t.Run("service", testFrontendService)
}

// frontendEnv is a tool environment with testdata/sink/sinks-frontend.yaml
// (docs-main pointing at the mock m, which must see no request) and an fs
// blob store in a temp dir (RECAP_BLOB_BACKEND=fs, RECAP_FS_ROOT; README.md).
func frontendEnv(t *testing.T) (*mockDocs, sinkEnv, string) {
	t.Helper()
	m := newMockDocs(t)
	srv := m.startTLS(t)
	e := newSinkEnvFrom(t, "sinks-frontend.yaml", srv, srv.URL)
	root := t.TempDir()
	e.extraEnv = []string{"RECAP_BLOB_BACKEND=fs", "RECAP_FS_ROOT=" + root}
	return m, e, root
}

// storedSummary is the path of a job's frontend summary in the fs blob
// store: key results/<job_id>/summary.md (architecture.md §8: the fs backend
// has the same layout under RECAP_FS_ROOT).
func storedSummary(root, jobID string) string {
	return filepath.Join(root, "results", jobID, "summary.md")
}

// expectStored checks an ok result of the frontend sink: state sent, an empty
// document_id (null or ""; sinks.md §4a), sent_at RFC 3339 UTC, and the stored
// object equal to the request's summary_md byte for byte.
func expectStored(t *testing.T, o output, req deliveryRequest, root string) {
	t.Helper()
	if o.Outcome != "ok" || o.Error != nil || o.HeldReason != nil || o.FailureReason != nil {
		t.Fatalf("got %s, want outcome ok without error", o.raw)
	}
	r := o.Delivery
	if r == nil || r.State != "sent" || r.JobID != req.JobID || r.SinkProfile != req.SinkProfile || str(r.DocumentID) != "" {
		t.Fatalf("delivery record %s, want state sent, job_id %s, sink_profile %s, empty document_id", o.raw, req.JobID, req.SinkProfile)
	}
	if _, err := time.Parse(time.RFC3339Nano, str(r.SentAt)); err != nil || !strings.HasSuffix(str(r.SentAt), "Z") {
		t.Errorf("sent_at %q is not RFC 3339 UTC", str(r.SentAt))
	}
	got, err := os.ReadFile(storedSummary(root, req.JobID))
	if err != nil {
		t.Fatalf("stored summary: %v", err)
	}
	if string(got) != req.SummaryMD {
		t.Fatalf("stored summary differs from summary_md (%d vs %d bytes)", len(got), len(req.SummaryMD))
	}
	for _, s := range []string{req.Title, req.SummaryMD} {
		if strings.Contains(string(o.raw), s) {
			t.Errorf("tool output contains request content %q", s)
		}
	}
}

func expectNotStored(t *testing.T, root, jobID string) {
	t.Helper()
	if _, err := os.Stat(storedSummary(root, jobID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("results/%s/summary.md exists after a rejection (stat: %v)", jobID, err)
	}
}

func testFrontendTool(t *testing.T) {
	m, e, root := frontendEnv(t)
	mark := stderrLen()
	req := baseRequest(t)
	req.SinkProfile = frontendProfile

	t.Run("stores-exact-bytes", func(t *testing.T) {
		expectStored(t, deliverOnce(t, e, req), req, root)
	})
	t.Run("second-deliver-overwrites", func(t *testing.T) {
		// Even with the record of an interrupted attempt (sending) the
		// frontend sink has no uncertain case (§4a).
		next := req.withContent(string(readFile(t, testdataPath(t, "sink", "allowed.md"))))
		o := e.run(t, toolInput{Request: next, Delivery: &record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: "sending"}})
		expectStored(t, o, next, root)
	})
	t.Run("oversized-rejected", func(t *testing.T) {
		big := req.SummaryMD + strings.Repeat("Filler paragraph about the widget.\n\n", 2097152/36+1)
		r := req.withContent(big[:2097153]) // max_document_bytes 2097152 + 1
		r.JobID = "019a2f8c-3b40-7000-8000-00000000000a"
		expectRejectedWithoutCall(t, deliverOnce(t, e, r))
		expectNotStored(t, root, r.JobID)
	})
	t.Run("forbidden-construct-rejected", func(t *testing.T) {
		r := req.withContent(req.SummaryMD + "\n" + string(readFile(t, testdataPath(t, "sink", "forbidden", "raw-html-block.md"))))
		r.JobID = "019a2f8c-3b40-7000-8000-00000000000b"
		expectRejectedWithoutCall(t, deliverOnce(t, e, r))
		expectNotStored(t, root, r.JobID)
	})
	t.Run("no-network-call", func(t *testing.T) {
		expectRequests(t, m, 0)
	})
	t.Run("no-content-in-logs", func(t *testing.T) {
		logs := stderrSince(mark)
		expectLogLines(t, "tool stderr", logs)
		expectNoCanary(t, "tool stderr", logs, append(append([]string{req.Title}, contentCanaries...), identityCanaries...))
	})
}

// regResult holds the registration response fields the service half reads
// (metadata-interface.md §2.3, §2.7).
type regResult struct {
	Job *struct {
		JobID            string  `json:"job_id"`
		Outcome          *string `json:"outcome"`
		DeliveredAt      *string `json:"delivered_at"`
		DocumentURL      *string `json:"document_url"`
		SummaryAvailable *bool   `json:"summary_available"`
	} `json:"job"`
}

func (s *service) result(t *testing.T, rid, jobID string) regResult {
	t.Helper()
	r := s.getRegistration(t, rid)
	expectStatus(t, r, http.StatusOK, "GET registration after delivery (RECAP_RETENTION_AFTER_DELIVERY must be > 0)")
	var res regResult
	if err := json.Unmarshal(r.body, &res); err != nil || res.Job == nil || res.Job.JobID != jobID {
		t.Fatalf("registration of job %s: %v %s", jobID, err, r)
	}
	j := res.Job
	if j.Outcome == nil || *j.Outcome != "delivered" {
		t.Errorf("registration after delivery: outcome %v, want delivered", j.Outcome)
	}
	if j.DeliveredAt == nil {
		t.Errorf("registration after delivery: delivered_at null")
	} else if _, err := time.Parse(time.RFC3339Nano, *j.DeliveredAt); err != nil {
		t.Errorf("delivered_at %q is not RFC 3339", *j.DeliveredAt)
	}
	if j.SummaryAvailable == nil {
		t.Errorf("registration after delivery: summary_available missing")
	}
	return res
}

func (s *service) summary(t *testing.T, method, rid string) reply {
	t.Helper()
	return s.do(t, method, "/api/v1/registrations/"+url.PathEscape(rid)+"/summary", credential(t, "frontend."+frontendID), "", nil)
}

// startJobVia is startJob with recap-sink set to sinkName.
func (s *service) startJobVia(t *testing.T, o owner, sinkName string) (rid, jobID string) {
	t.Helper()
	rid, mid := freshRecordID(), freshMeetingID()
	expectStatus(t, s.putRegistration(t, rid, mid, o), http.StatusCreated, "PUT registration")
	return rid, s.ingest(t, buildPackageFor(t, rid, mid, sinkName))
}

// withoutFooter drops the last non-empty line, the footer of templates.md
// §5.4 block 10, whose {generated_at} is the job's created_at.
func withoutFooter(md string) string {
	lines := strings.Split(strings.TrimRight(md, "\n"), "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

// testFrontendService: the same F-01 package delivered to Docs and to the
// frontend sink gives the same summary.md (apart from the footer's
// generated_at), the frontend job sends nothing to Docs, its delivery record
// is sent without a document ID, and the registration shows
// summary_available true and document_url null.
func testFrontendService(t *testing.T) {
	s := newService(t)
	s.docs.reset()
	docsRID, docsJob := s.startJobVia(t, ada, contractSink)
	s.waitDelivered(t, docsJob)
	expectRequests(t, s.docs, 1)
	sent := expectOneDocument(t, s.docs).content
	if res := s.result(t, docsRID, docsJob); res.Job.DocumentURL == nil ||
		!strings.HasSuffix(*res.Job.DocumentURL, "/docs/"+expectOneDocument(t, s.docs).id+"/") {
		t.Errorf("Docs delivery: document_url %v, want docs-contract's template with the document ID", res.Job.DocumentURL)
	}

	rid, jobID := s.startJobVia(t, ada, frontendContractSink)
	s.waitDelivered(t, jobID)
	expectRequests(t, s.docs, 1) // no network call for the frontend sink
	j := s.job(t, jobID)
	if j.Delivery == nil || j.Delivery.State != "sent" || str(j.Delivery.DocumentID) != "" {
		t.Errorf("frontend job delivery %+v, want {sent, empty document_id}", j.Delivery)
	}
	res := s.result(t, rid, jobID)
	if res.Job.DocumentURL != nil || res.Job.SummaryAvailable == nil || !*res.Job.SummaryAvailable {
		t.Errorf("frontend delivery: document_url %v, summary_available %v; want null and true", res.Job.DocumentURL, res.Job.SummaryAvailable)
	}

	r := s.summary(t, http.MethodGet, rid)
	expectStatus(t, r, http.StatusOK, "GET summary")
	if mt, _, err := mime.ParseMediaType(r.header.Get("Content-Type")); err != nil || mt != "text/markdown" {
		t.Errorf("GET summary: Content-Type %q, want text/markdown", r.header.Get("Content-Type"))
	}
	if got, want := withoutFooter(string(r.body)), withoutFooter(sent); got != want {
		t.Errorf("stored summary.md differs from the one Docs received beyond the footer (%d vs %d bytes)", len(got), len(want))
	}
	if !strings.HasPrefix(lastLine(string(r.body)), "*meeting-summary ") {
		t.Errorf("stored summary.md does not end with the footer of templates.md §5.4")
	}
	expectStatus(t, s.summary(t, http.MethodDelete, rid), http.StatusNoContent, "DELETE summary")
}

func lastLine(md string) string {
	lines := strings.Split(strings.TrimRight(md, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestResultDocumentURL is the release half of CT-REG-08 (metadata-interface.md
// §2.7): document_url is null after a release without a document ID, and the
// profile's link with the ID given to release otherwise. It drives the
// service with the mock Docs on RECAP_CONTRACT_DOCS_ADDR.
func TestResultDocumentURL(t *testing.T) {
	t.Run("CT-REG-08 document_url after admin release", func(t *testing.T) {
		harness.BaseURL(t)
		s := newService(t)
		t.Run("without-document-id-null", func(t *testing.T) {
			s.docs.reset()
			s.docs.queue(fault{dropAfterCreate: true})
			rid, jobID := s.startJobVia(t, ada, contractSink)
			s.waitHeld(t, jobID, "delivery_uncertain")
			s.action(t, jobID, "release", "{}")
			s.waitDelivered(t, jobID)
			if res := s.result(t, rid, jobID); res.Job.DocumentURL != nil {
				t.Errorf("document_url %q after release without document ID, want null", *res.Job.DocumentURL)
			}
		})
		t.Run("with-document-id", func(t *testing.T) {
			s.docs.reset()
			s.docs.queue(fault{dropAfterCreate: true})
			rid, jobID := s.startJobVia(t, ada, contractSink)
			s.waitHeld(t, jobID, "delivery_uncertain")
			found := expectOneDocument(t, s.docs).id
			body, _ := json.Marshal(map[string]string{"document_id": found})
			s.action(t, jobID, "release", string(body))
			s.waitDelivered(t, jobID)
			if res := s.result(t, rid, jobID); res.Job.DocumentURL == nil || !strings.HasSuffix(*res.Job.DocumentURL, "/docs/"+found+"/") {
				t.Errorf("document_url %v, want docs-contract's template with %s", res.Job.DocumentURL, found)
			}
		})
	})
}
