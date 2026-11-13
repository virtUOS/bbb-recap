package sink

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Several sinks per job (sinks.md §1.8, ADR-016): CT-SINK-13 to CT-SINK-18
// and the service half of CT-REG-10 (metadata-interface.md §2.7). They drive
// the running service with the contract environment's profiles docs-contract
// (the mock Docs of this package on RECAP_CONTRACT_DOCS_ADDR) and
// frontend-contract. CT-SINK-18 also needs RECAP_CONTRACT_HOOKS_ADDR: the test
// serves a webhook receiver and a Matrix homeserver there, and the contract
// environment adds the profiles webhook-contract and matrix-contract
// (contracts/README.md). Written before the implementation: they fail until
// the service delivers to several sinks.

const (
	hooksAddrVar         = "RECAP_CONTRACT_HOOKS_ADDR"
	webhookContractSink  = "webhook-contract"
	matrixContractSink   = "matrix-contract"
	webhookContractPath  = "/webhook"
	listDocsFirst        = contractSink + "," + frontendContractSink // docs-contract,frontend-contract
	skippedByAdminStatus = "skipped_by_admin"
)

// TestSeveralSinks holds the multi-sink tests of sinks.md §4.
func TestSeveralSinks(t *testing.T) {
	t.Run("CT-SINK-13 two sinks delivered", testTwoSinks)
	t.Run("CT-SINK-14 partial failure and skip", testPartialFailure)
	t.Run("CT-SINK-15 uncertain sink and per-sink retry", testUncertainSink)
	t.Run("CT-SINK-16 not sent, then delivered once", testNotSentSink)
	t.Run("CT-SINK-17 admin actions with sink", testSinkActionErrors)
	t.Run("CT-SINK-18 fixed order and links to webhook and Matrix", testOrderAndLinks)
}

// --- job body and registration with deliveries ---

type sinkStatus struct {
	SinkProfile   string  `json:"sink_profile"`
	State         string  `json:"state"`
	DocumentID    *string `json:"document_id"`
	FailureReason *string `json:"failure_reason"`
}

type multiJob struct {
	JobID         string       `json:"job_id"`
	State         string       `json:"state"`
	HeldReason    string       `json:"held_reason"`
	FailureReason string       `json:"failure_reason"`
	Deliveries    []sinkStatus `json:"deliveries"`
}

type regDelivery struct {
	SinkProfile      string  `json:"sink_profile"`
	State            string  `json:"state"`
	DocumentURL      *string `json:"document_url"`
	SummaryAvailable *bool   `json:"summary_available"`
}

type regWithDeliveries struct {
	SchemaVersion string `json:"schema_version"`
	Job           *struct {
		JobID            string        `json:"job_id"`
		DocumentURL      *string       `json:"document_url"`
		SummaryAvailable *bool         `json:"summary_available"`
		Deliveries       []regDelivery `json:"deliveries"`
	} `json:"job"`
}

func (s *service) multiJob(t *testing.T, jobID string) multiJob {
	t.Helper()
	r := s.do(t, http.MethodGet, "/api/v1/admin/jobs/"+jobID, credential(t, "admin."+adminName), "", nil)
	expectStatus(t, r, http.StatusOK, "GET admin job")
	var j multiJob
	if err := json.Unmarshal(r.body, &j); err != nil || j.JobID != jobID {
		t.Fatalf("admin job %s: %v %s", jobID, err, r)
	}
	return j
}

// expectStatuses checks the admin job body's deliveries (architecture.md §4):
// profiles in delivery order with these sink statuses (sinks.md §1.8).
func expectStatuses(t *testing.T, j multiJob, want ...string) {
	t.Helper()
	var got []string
	for _, d := range j.Deliveries {
		got = append(got, d.SinkProfile+"="+d.State)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("job %s deliveries %v, want %v (sinks.md §1.8, delivery order)", j.JobID, got, want)
	}
}

// registration reads GET …/registrations/{rid}: schema_version 1.3.0 or later
// and job.deliveries (metadata-interface.md §2.7).
func (s *service) registration(t *testing.T, rid, jobID string) regWithDeliveries {
	t.Helper()
	r := s.getRegistration(t, rid)
	expectStatus(t, r, http.StatusOK, "GET registration (RECAP_RETENTION_AFTER_DELIVERY must be > 0)")
	var res regWithDeliveries
	if err := json.Unmarshal(r.body, &res); err != nil || res.Job == nil || res.Job.JobID != jobID {
		t.Fatalf("registration of job %s: %v %s", jobID, err, r)
	}
	if v := strings.Split(res.SchemaVersion, "."); len(v) != 3 || v[0] != "1" || len(v[1]) == 1 && v[1] < "3" {
		t.Errorf("schema_version %q, want 1.3.0 or later (metadata-interface.md §2.3)", res.SchemaVersion)
	}
	return res
}

// waitState polls the job until it is in state (and reason, if not empty).
func (s *service) waitState(t *testing.T, jobID, state, reason string) multiJob {
	t.Helper()
	deadline := time.Now().Add(pipelineTimeout)
	for {
		j := s.multiJob(t, jobID)
		if j.State == state && (reason == "" || j.HeldReason == reason || j.FailureReason == reason) {
			return j
		}
		settled := j.State == "cleaned" || j.State == "delivered" || j.State == "failed" || j.State == "held"
		if settled && (j.State != state || reason != "") || time.Now().After(deadline) {
			t.Fatalf("job %s: state %s held %q failed %q, want %s %s", jobID, j.State, j.HeldReason, j.FailureReason, state, reason)
		}
		time.Sleep(pollInterval)
	}
}

func expectProblemSlug(t *testing.T, r reply, status int, slug, what string) {
	t.Helper()
	expectStatus(t, r, status, what)
	if !strings.Contains(string(r.body), "urn:recap:problem:"+slug) {
		t.Errorf("%s: %s, want problem %s", what, r, slug)
	}
}

// --- tests (docs-contract and frontend-contract) ---

func testTwoSinks(t *testing.T) {
	s := newService(t)
	t.Run("docs-first-in-list", func(t *testing.T) {
		s.docs.reset()
		rid, jobID := s.startJobVia(t, ada, listDocsFirst)
		s.waitState(t, jobID, "delivered", "")
		expectRequests(t, s.docs, 1)
		doc := expectOneDocument(t, s.docs)
		j := s.multiJob(t, jobID)
		expectStatuses(t, j, contractSink+"=sent", frontendContractSink+"=sent")
		if str(j.Deliveries[0].DocumentID) != doc.id {
			t.Errorf("docs-contract document_id %q, want %s", str(j.Deliveries[0].DocumentID), doc.id)
		}
		t.Run("CT-REG-10 deliveries in the registration", func(t *testing.T) {
			res := s.registration(t, rid, jobID)
			d := res.Job.Deliveries
			if len(d) != 2 || d[0].SinkProfile != contractSink || d[1].SinkProfile != frontendContractSink {
				t.Fatalf("deliveries %+v, want docs-contract, frontend-contract", d)
			}
			if d[0].State != "sent" || d[0].DocumentURL == nil || !strings.HasSuffix(*d[0].DocumentURL, "/docs/"+doc.id+"/") || d[0].SummaryAvailable == nil || *d[0].SummaryAvailable {
				t.Errorf("docs entry %+v, want sent with the document's link and summary_available false", d[0])
			}
			if d[1].State != "sent" || d[1].DocumentURL != nil || d[1].SummaryAvailable == nil || !*d[1].SummaryAvailable {
				t.Errorf("frontend entry %+v, want sent, document_url null, summary_available true", d[1])
			}
			if res.Job.DocumentURL == nil || *res.Job.DocumentURL != *d[0].DocumentURL || res.Job.SummaryAvailable == nil || !*res.Job.SummaryAvailable {
				t.Errorf("top-level document_url %v, summary_available %v; want the Docs link and true", res.Job.DocumentURL, res.Job.SummaryAvailable)
			}
			expectStatus(t, s.summary(t, http.MethodGet, rid), http.StatusOK, "GET summary")
		})
	})
	t.Run("list-order-within-group", func(t *testing.T) {
		s.docs.reset()
		_, jobID := s.startJobVia(t, ada, frontendContractSink+","+contractSink)
		s.waitState(t, jobID, "delivered", "")
		expectStatuses(t, s.multiJob(t, jobID), frontendContractSink+"=sent", contractSink+"=sent")
	})
}

func testPartialFailure(t *testing.T) {
	s := newService(t)
	s.docs.reset()
	s.docs.queue(statusFault(http.StatusBadRequest, bodyBadContent))
	rid, jobID := s.startJobVia(t, ada, listDocsFirst)
	j := s.waitState(t, jobID, "failed", "rejected_by_sink")
	expectStatuses(t, j, contractSink+"=failed", frontendContractSink+"=sent")
	if str(j.Deliveries[0].FailureReason) != "rejected_by_sink" {
		t.Errorf("failed sink failure_reason %q, want rejected_by_sink", str(j.Deliveries[0].FailureReason))
	}
	expectProblemSlug(t, s.postAction(t, jobID, "release", "{}"), http.StatusConflict, "invalid-job-state", "release {} on a failed job")
	r := s.postAction(t, jobID, "release", `{"sink":"`+contractSink+`"}`)
	expectStatus(t, r, http.StatusOK, "release with sink on a failed sink")
	j = s.waitState(t, jobID, "delivered", "")
	expectStatuses(t, j, contractSink+"="+skippedByAdminStatus, frontendContractSink+"=sent")
	expectRequests(t, s.docs, 1) // skipping sends nothing
	res := s.registration(t, rid, jobID)
	if d := res.Job.Deliveries; len(d) != 2 || d[0].State != skippedByAdminStatus || d[0].DocumentURL != nil {
		t.Errorf("registration deliveries %+v, want docs-contract skipped_by_admin without link", d)
	}
	if res.Job.DocumentURL != nil {
		t.Errorf("top-level document_url %q, want null (no sink has a link)", *res.Job.DocumentURL)
	}
}

func testUncertainSink(t *testing.T) {
	s := newService(t)
	s.docs.reset()
	s.docs.queue(statusFault(http.StatusInternalServerError, bodyServerError))
	rid, jobID := s.startJobVia(t, ada, listDocsFirst)
	j := s.waitState(t, jobID, "held", "delivery_uncertain")
	expectStatuses(t, j, contractSink+"=sending", frontendContractSink+"=sent")
	expectRequests(t, s.docs, 1)
	before := s.summary(t, http.MethodGet, rid)
	expectStatus(t, before, http.StatusOK, "GET summary while held")
	expectStatus(t, s.postAction(t, jobID, "retry", `{"sink":"`+contractSink+`"}`), http.StatusOK, "retry with sink")
	j = s.waitState(t, jobID, "delivered", "")
	expectRequests(t, s.docs, 2) // exactly one new POST
	expectStatuses(t, j, contractSink+"=sent", frontendContractSink+"=sent")
	after := s.summary(t, http.MethodGet, rid)
	expectStatus(t, after, http.StatusOK, "GET summary after retry")
	if !bytes.Equal(before.body, after.body) {
		t.Error("the frontend summary changed: a sent sink must not be delivered again")
	}
}

func testNotSentSink(t *testing.T) {
	s := newService(t)
	s.docs.reset()
	s.docs.queue(statusFault(http.StatusTooManyRequests, bodyThrottled))
	rid, jobID := s.startJobVia(t, ada, listDocsFirst)
	// After the 429 the docs sink waits for the backoff (architecture.md §5.3,
	// about 30 s); the frontend sink is sent in the first attempt.
	j := s.waitState(t, jobID, "delivered", "")
	expectRequests(t, s.docs, 2)
	expectOneDocument(t, s.docs)
	expectStatuses(t, j, contractSink+"=sent", frontendContractSink+"=sent")
	expectStatus(t, s.summary(t, http.MethodGet, rid), http.StatusOK, "GET summary")
}

func testSinkActionErrors(t *testing.T) {
	s := newService(t)
	s.docs.reset()
	s.docs.queue(statusFault(http.StatusBadRequest, bodyBadContent))
	_, jobID := s.startJobVia(t, ada, contractSink) // one sink
	s.waitState(t, jobID, "failed", "rejected_by_sink")
	expectProblemSlug(t, s.postAction(t, jobID, "release", `{"sink":"no-such-sink"}`), http.StatusBadRequest, "invalid-request", "release with a sink not in the job")
	expectProblemSlug(t, s.postAction(t, jobID, "release", `{"sink":"`+contractSink+`"}`), http.StatusConflict, "invalid-job-state", "skip of the job's only sink")
	expectProblemSlug(t, s.postAction(t, jobID, "purge", `{"sink":"`+contractSink+`"}`), http.StatusBadRequest, "invalid-request", "purge with sink")
	if j := s.multiJob(t, jobID); j.State != "failed" {
		t.Errorf("job %s after refused actions, want failed", j.State)
	}
}

// --- CT-SINK-18: webhook and Matrix in the contract environment ---

// hooksServer is the webhook receiver and Matrix homeserver on
// RECAP_CONTRACT_HOOKS_ADDR, shared by the tests of this package.
var hooksServer struct {
	once sync.Once
	recv *mockReceiver
	hs   *mockHomeserver
	err  error
	// docsBefore records, per request, how many Docs requests had arrived.
	mu         sync.Mutex
	docsBefore map[string][]int
}

func startHooks(t *testing.T, s *service) (*mockReceiver, *mockHomeserver) {
	t.Helper()
	addr := os.Getenv(hooksAddrVar)
	if addr == "" {
		t.Skipf("skipped: %s not set (contracts/README.md)", hooksAddrVar)
	}
	hooksServer.once.Do(func() {
		recv := &mockReceiver{hold: 10 * time.Second}
		hs := newMockHomeserver(t)
		hooksServer.docsBefore = map[string][]int{}
		note := func(what string) {
			n := len(s.docs.received())
			hooksServer.mu.Lock()
			hooksServer.docsBefore[what] = append(hooksServer.docsBefore[what], n)
			hooksServer.mu.Unlock()
		}
		hs.onRequest = func(r mxRequest) {
			if r.endpoint == "send" {
				note("matrix")
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc(webhookContractPath, func(w http.ResponseWriter, r *http.Request) { note("webhook"); recv.ServeHTTP(w, r) })
		mux.Handle("/_matrix/", hs)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			hooksServer.err = err
			return
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(l) }() // ends with the test process
		hooksServer.recv, hooksServer.hs = recv, hs
	})
	if hooksServer.err != nil {
		t.Fatalf("%s=%s: %v", hooksAddrVar, addr, hooksServer.err)
	}
	hooksServer.recv.reset()
	hooksServer.hs.reset()
	hooksServer.mu.Lock()
	hooksServer.docsBefore = map[string][]int{}
	hooksServer.mu.Unlock()
	return hooksServer.recv, hooksServer.hs
}

// startJobRoom registers Ada and ingests F-01 with recap-sink sinks and
// recap-matrix-room room.
func (s *service) startJobRoom(t *testing.T, sinks, room string) (rid, jobID string) {
	t.Helper()
	rid, mid := freshRecordID(), freshMeetingID()
	expectStatus(t, s.putRegistration(t, rid, mid, ada), http.StatusCreated, "PUT registration")
	return rid, s.ingest(t, buildPackageMeta(t, rid, mid, sinks, map[string]string{"recap-matrix-room": room}))
}

func testOrderAndLinks(t *testing.T) {
	s := newService(t)
	recv, hs := startHooks(t, s)
	s.docs.reset()
	// List order puts Matrix and the webhook first; group order wins (sinks.md §1.1).
	list := matrixContractSink + "," + webhookContractSink + "," + contractSink
	rid, jobID := s.startJobRoom(t, list, roomJoined)
	j := s.waitState(t, jobID, "delivered", "")
	expectStatuses(t, j, contractSink+"=sent", matrixContractSink+"=sent", webhookContractSink+"=sent")
	doc := expectOneDocument(t, s.docs)
	res := s.registration(t, rid, jobID)
	docsURL := ""
	if res.Job.Deliveries[0].DocumentURL != nil {
		docsURL = *res.Job.Deliveries[0].DocumentURL
	}
	if !strings.HasSuffix(docsURL, "/docs/"+doc.id+"/") {
		t.Fatalf("docs-contract link %q, want the document %s", docsURL, doc.id)
	}
	hooksServer.mu.Lock()
	before := hooksServer.docsBefore
	hooksServer.mu.Unlock()
	for _, what := range []string{"matrix", "webhook"} {
		if n := before[what]; len(n) != 1 || n[0] != 1 {
			t.Errorf("%s requests saw %v Docs requests before them, want one request after the Docs POST", what, n)
		}
	}
	// Webhook: signed with the fixture secret, links carry the Docs link.
	got := recv.received()
	if len(got) != 1 {
		t.Fatalf("webhook receiver got %d requests, want 1", len(got))
	}
	id, ts := got[0].header.Get("webhook-id"), got[0].header.Get("webhook-timestamp")
	if id != jobID+":"+webhookContractSink {
		t.Errorf("webhook-id %q, want %s:%s", id, jobID, webhookContractSink)
	}
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(fixtureSigningSecret(t), "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(got[0].body)
	if sig := got[0].header.Get("webhook-signature"); sig != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Error("webhook-signature does not verify with the fixture secret")
	}
	var body struct {
		Data struct {
			JobID string `json:"job_id"`
			Links []link `json:"links"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil || body.Data.JobID != jobID ||
		!slices.Equal(body.Data.Links, []link{{SinkProfile: contractSink, DocumentURL: docsURL}}) {
		t.Errorf("webhook data job_id %q links %v, want %s and the docs-contract link", body.Data.JobID, body.Data.Links, jobID)
	}
	// Matrix: one notice in the joined room with the Docs link.
	ev := hs.posted()
	if len(ev) != 1 || ev[0].room != roomJoined || ev[0].txn != "recap-"+jobID+"-"+matrixContractSink {
		t.Fatalf("homeserver events %+v, want one in %s with txnId recap-%s-%s", ev, roomJoined, jobID, matrixContractSink)
	}
	var c struct {
		MsgType string `json:"msgtype"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(ev[0].content, &c); err != nil || c.MsgType != "m.notice" || !strings.HasSuffix(c.Body, "\n"+docsURL) {
		t.Errorf("Matrix event %s, want an m.notice whose last line is the Docs link", ev[0].content)
	}
	if str(j.Deliveries[1].DocumentID) != ev[0].eventID {
		t.Errorf("matrix-contract document_id %q, want the event ID %s", str(j.Deliveries[1].DocumentID), ev[0].eventID)
	}
	scanServiceLog(t, append(append([]string{roomJoined, fixtureMatrixToken(t), fixtureSigningSecret(t)}, contentCanaries...), identityCanaries...))
}

// buildPackageMeta is buildPackageFor with more recap-* metadata in
// events.xml and the manifest (meeting-package.md §4).
func buildPackageMeta(t *testing.T, rid, mid, sinks string, extra map[string]string) *pkg {
	t.Helper()
	dir := testdataPath(t, "meetings", "F-01")
	mb := readFile(t, filepath.Join(dir, "expected", "manifest.json"))
	d := json.NewDecoder(bytes.NewReader(mb))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	oldRecord, _ := m["record_id"].(string)
	oldMeeting, _ := m["meeting_id"].(string)
	raw := filepath.Join(dir, "raw", oldRecord)
	attrs := `recap-sink="` + sinks + `"`
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		attrs += " " + k + `="` + extra[k] + `"`
	}
	ev := string(readFile(t, filepath.Join(raw, "events.xml")))
	ev = strings.ReplaceAll(ev, oldRecord, rid)
	for _, pair := range [][2]string{
		{`externalId="` + oldMeeting + `"`, `externalId="` + mid + `"`},
		{`meetingId="` + oldMeeting + `"`, `meetingId="` + mid + `"`},
		{`recap-sink="docs-main"`, attrs},
	} {
		if strings.Count(ev, pair[0]) != 1 {
			t.Fatalf("events.xml: want exactly one %s", pair[0])
		}
		ev = strings.Replace(ev, pair[0], pair[1], 1)
	}
	p := &pkg{files: map[string][]byte{}}
	var total int64
	for _, f := range m["files"].([]any) {
		f := f.(map[string]any)
		data := []byte(ev)
		if f["path"] != "events.xml" {
			data = readFile(t, filepath.Join(raw, filepath.FromSlash(f["path"].(string))))
		}
		f["sha256"], f["size"] = sha(data), int64(len(data))
		p.files[sha(data)] = data
		total += int64(len(data))
	}
	m["totals"] = map[string]any{"file_count": int64(len(m["files"].([]any))), "size": total}
	meta := m["metadata"].(map[string]any)
	meta["recap-sink"] = sinks
	for k, v := range extra {
		meta[k] = v
	}
	m["record_id"], m["meeting_id"] = rid, mid
	c, err := canonicalJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	p.manifest, p.id = c, sha(c)
	return p
}
