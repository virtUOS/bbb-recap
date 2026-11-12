package sink

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Environment of the service-level tests (README.md "Service-level tests").
const (
	docsAddrVar = "RECAP_CONTRACT_DOCS_ADDR" // host:port where the test serves mock Docs over HTTP
	logFileVar  = "RECAP_CONTRACT_LOG_FILE"  // log of the service under test (contracts/README.md)
	// contractSink is the sink profile of the contract environment whose
	// docs_base_url is http://$RECAP_CONTRACT_DOCS_ADDR. Packages of these
	// tests name it in meta recap-sink.
	contractSink = "docs-contract"
)

// Fixture names (fixtures.md §3, §5).
const (
	frontendID = "pilos-fixture"
	serverID   = "bbb-fixture-a"
	adminName  = "fixture-admin"
)

// Waiting for the service: a run through to deliver takes every step with
// mocks (as contracts/registration).
const (
	pipelineTimeout = 3 * time.Minute
	pollInterval    = time.Second
)

// service is a client for the running service plus the mock Docs it calls
// through the docs-contract sink profile.
type service struct {
	base string
	http *http.Client
	docs *mockDocs
}

// serviceDocs is the mock Docs on RECAP_CONTRACT_DOCS_ADDR, shared by the
// service-level tests of this package, which run one after another.
var serviceDocs struct {
	once sync.Once
	m    *mockDocs
	srv  *http.Server
	err  error
}

// TestMain closes the shared mock Docs after all tests of the package.
func TestMain(m *testing.M) {
	code := m.Run()
	if serviceDocs.srv != nil {
		_ = serviceDocs.srv.Close()
	}
	os.Exit(code)
}

func newService(t *testing.T) *service {
	t.Helper()
	base := strings.TrimRight(harness.BaseURL(t), "/")
	addr := os.Getenv(docsAddrVar)
	if addr == "" {
		t.Skipf("skipped: %s not set", docsAddrVar)
	}
	serviceDocs.once.Do(func() {
		m := newMockDocs(t)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			serviceDocs.err = err
			return
		}
		serviceDocs.srv = &http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = serviceDocs.srv.Serve(l) }() // closed by TestMain
		serviceDocs.m = m
	})
	if serviceDocs.err != nil {
		t.Fatalf("%s=%s: %v", docsAddrVar, addr, serviceDocs.err)
	}
	serviceDocs.m.reset()
	return &service{base: base, http: &http.Client{Timeout: 30 * time.Second}, docs: serviceDocs.m}
}

// reply is a buffered HTTP response.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) String() string {
	b := r.body
	if len(b) > 300 {
		b = b[:300]
	}
	return "HTTP " + strconv.Itoa(r.status) + " " + string(b)
}

func (s *service) do(t *testing.T, method, path, token, contentType string, body []byte) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	r := reply{status: resp.StatusCode, header: resp.Header, body: b}
	expectNoCanary(t, method+" "+path+" response", b, identityCanaries)
	return r
}

func expectStatus(t *testing.T, r reply, want int, what string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("%s: got %s, want HTTP %d", what, r, want)
	}
}

func credential(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", "clients", name))))
}

func freshRecordID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func freshMeetingID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "fixture-meeting-ct-" + hex.EncodeToString(b)
}

// --- registration API (metadata-interface.md §2) ---

func (s *service) putRegistration(t *testing.T, rid, mid string, o owner) reply {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"schema_version": "1.0.0", "meeting_id": mid, "record_id": rid, "owner": o})
	return s.do(t, http.MethodPut, "/api/v1/registrations/"+url.PathEscape(rid), credential(t, "frontend."+frontendID), "application/json", b)
}

func (s *service) getRegistration(t *testing.T, rid string) reply {
	t.Helper()
	return s.do(t, http.MethodGet, "/api/v1/registrations/"+url.PathEscape(rid), credential(t, "frontend."+frontendID), "", nil)
}

// --- admin API (architecture.md §4, §5.4) ---

type jobView struct {
	JobID         string `json:"job_id"`
	State         string `json:"state"`
	HeldReason    string `json:"held_reason"`
	FailureReason string `json:"failure_reason"`
	Outcome       string `json:"outcome"`
	// Delivery is the job's delivery record, or null (architecture.md §4).
	Delivery *struct {
		State      string  `json:"state"`
		DocumentID *string `json:"document_id"`
	} `json:"delivery"`
}

// expectDelivery checks the delivery field of a job body; an empty state
// means null. documentID "" means document_id null.
func expectDelivery(t *testing.T, j jobView, state, documentID, what string) {
	t.Helper()
	switch {
	case state == "" && j.Delivery != nil:
		t.Fatalf("%s: delivery %+v, want null", what, *j.Delivery)
	case state == "":
	case j.Delivery == nil:
		t.Fatalf("%s: delivery null, want state %s", what, state)
	case j.Delivery.State != state || str(j.Delivery.DocumentID) != documentID || (documentID == "" && j.Delivery.DocumentID != nil):
		t.Fatalf("%s: delivery {state %s, document_id %v}, want {state %s, document_id %q (\"\" = null)}",
			what, j.Delivery.State, str(j.Delivery.DocumentID), state, documentID)
	}
}

func (j jobView) String() string {
	return "state=" + j.State + " held_reason=" + j.HeldReason + " failure_reason=" + j.FailureReason + " outcome=" + j.Outcome
}

func (s *service) job(t *testing.T, jobID string) jobView {
	t.Helper()
	r := s.do(t, http.MethodGet, "/api/v1/admin/jobs/"+url.PathEscape(jobID), credential(t, "admin."+adminName), "", nil)
	expectStatus(t, r, http.StatusOK, "GET admin job")
	var j jobView
	if err := json.Unmarshal(r.body, &j); err != nil || j.JobID != jobID {
		t.Fatalf("admin job %s: %v %s", jobID, err, r)
	}
	return j
}

// waitJob polls until ok(job) holds; it stops early on failed or cleaned.
func (s *service) waitJob(t *testing.T, jobID, want string, ok func(jobView) bool) jobView {
	t.Helper()
	deadline := time.Now().Add(pipelineTimeout)
	for {
		j := s.job(t, jobID)
		if ok(j) {
			return j
		}
		if j.State == "failed" || j.State == "cleaned" || time.Now().After(deadline) {
			t.Fatalf("job %s: %s, want %s", jobID, j, want)
		}
		time.Sleep(pollInterval)
	}
}

func (s *service) waitHeld(t *testing.T, jobID, reason string) {
	t.Helper()
	s.waitJob(t, jobID, "held/"+reason, func(j jobView) bool { return j.State == "held" && j.HeldReason == reason })
}

func (s *service) waitDelivered(t *testing.T, jobID string) {
	t.Helper()
	s.waitJob(t, jobID, "delivered", func(j jobView) bool { return j.State == "delivered" })
}

// postAction posts an admin action (architecture.md §4, §5.4) with body.
func (s *service) postAction(t *testing.T, jobID, name, body string) reply {
	t.Helper()
	return s.do(t, http.MethodPost, "/api/v1/admin/jobs/"+url.PathEscape(jobID)+"/"+name, credential(t, "admin."+adminName), "application/json", []byte(body))
}

// action posts an admin action and expects 200 with the job body.
func (s *service) action(t *testing.T, jobID, name, body string) jobView {
	t.Helper()
	r := s.postAction(t, jobID, name, body)
	expectStatus(t, r, http.StatusOK, "POST "+name)
	var j jobView
	if err := json.Unmarshal(r.body, &j); err != nil || j.JobID != jobID {
		t.Fatalf("POST %s: answer is not the job body of %s: %v %s", name, jobID, err, r)
	}
	return j
}

// --- running a job to deliver ---

// startJob registers o for a fresh record and ingests F-01 with recap-sink
// docs-contract. It returns the IDs and the job_id.
func (s *service) startJob(t *testing.T, o owner) (rid, mid, jobID string) {
	t.Helper()
	rid, mid = freshRecordID(), freshMeetingID()
	expectStatus(t, s.putRegistration(t, rid, mid, o), http.StatusCreated, "PUT registration")
	return rid, mid, s.ingest(t, buildPackage(t, rid, mid))
}

// pkg is a meeting package ready for ingest.
type pkg struct {
	id       string
	manifest []byte
	files    map[string][]byte // sha256 → content
}

// ingest pushes p as the exporter does (bbb-exporter.md §6) and returns the job_id.
func (s *service) ingest(t *testing.T, p *pkg) string {
	t.Helper()
	token := credential(t, "bbb_server."+serverID)
	path := "/api/v1/ingest/packages/" + p.id
	r := s.do(t, http.MethodPut, path, token, "application/json", p.manifest)
	expectStatus(t, r, http.StatusCreated, "PUT manifest")
	var st struct {
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal(r.body, &st); err != nil {
		t.Fatalf("IngestStatus: %v", err)
	}
	for _, sha := range st.Missing {
		data, ok := p.files[sha]
		if !ok {
			t.Fatalf("service asks for %s, which is not in the manifest", sha)
		}
		r := s.do(t, http.MethodPut, path+"/files/"+sha, token, "application/octet-stream", data)
		if r.status != http.StatusNoContent && r.status != http.StatusOK {
			t.Fatalf("PUT file: got %s, want 204 or 200", r)
		}
	}
	r = s.do(t, http.MethodPost, path+"/commit", token, "application/json", []byte("{}"))
	expectStatus(t, r, http.StatusOK, "POST commit")
	var a struct {
		State string `json:"state"`
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(r.body, &a); err != nil || a.State != "committed" || a.JobID == "" {
		t.Fatalf("IngestAck: %v %s", err, r)
	}
	return a.JobID
}

// buildPackage builds F-01 with a fresh record_id and meeting_id and
// recap-sink docs-contract (meeting-package.md §4). It first checks that its
// canonical JSON reproduces the golden package_id.
func buildPackage(t *testing.T, rid, mid string) *pkg {
	t.Helper()
	return buildPackageFor(t, rid, mid, contractSink)
}

// buildPackageFor is buildPackage with recap-sink set to sinkName.
func buildPackageFor(t *testing.T, rid, mid, sinkName string) *pkg {
	t.Helper()
	dir := testdataPath(t, "meetings", "F-01")
	mb := readFile(t, filepath.Join(dir, "expected", "manifest.json"))
	d := json.NewDecoder(bytes.NewReader(mb))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	golden := strings.TrimSpace(string(readFile(t, filepath.Join(dir, "expected", "package_id.txt"))))
	if c, err := canonicalJSON(m); err != nil || sha(c) != golden {
		t.Fatalf("package builder: F-01 manifest gives package_id %s, golden %s (%v)", sha(c), golden, err)
	}
	oldRecord, _ := m["record_id"].(string)
	oldMeeting, _ := m["meeting_id"].(string)
	raw := filepath.Join(dir, "raw", oldRecord)

	ev := string(readFile(t, filepath.Join(raw, "events.xml")))
	ev = strings.ReplaceAll(ev, oldRecord, rid)
	for _, pair := range [][2]string{
		{`externalId="` + oldMeeting + `"`, `externalId="` + mid + `"`},
		{`meetingId="` + oldMeeting + `"`, `meetingId="` + mid + `"`},
		{`recap-sink="docs-main"`, `recap-sink="` + sinkName + `"`},
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
	m["metadata"].(map[string]any)["recap-sink"] = sinkName
	m["record_id"], m["meeting_id"] = rid, mid
	c, err := canonicalJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	p.manifest, p.id = c, sha(c)
	return p
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// canonicalJSON serializes per RFC 8785 (JCS) for the value types a manifest
// has: objects, arrays, strings, integers, booleans and null (contracts/README.md).
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	err := writeJCS(&b, v)
	return b.Bytes(), err
}

func writeJCS(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeJCSString(b, x)
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case json.Number:
		n, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return fmt.Errorf("only integers are supported, got %s", x)
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { // RFC 8785 §3.2.3: UTF-16 code units
			return slices.Compare(utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))) < 0
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJCSString(b, k)
			b.WriteByte(':')
			if err := writeJCS(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

// writeJCSString escapes per RFC 8785 §3.2.2.2.
func writeJCSString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// --- log file ---

// scanServiceLog scans RECAP_CONTRACT_LOG_FILE when it and the base URL are
// set; otherwise only this scan is skipped (contracts/README.md).
func scanServiceLog(t *testing.T, canaries []string) {
	t.Helper()
	path := os.Getenv(logFileVar)
	if path == "" || os.Getenv(harness.EnvBaseURL) == "" {
		t.Logf("service log scan skipped: %s or %s not set", logFileVar, harness.EnvBaseURL)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", logFileVar, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("%s %s is empty", logFileVar, path)
	}
	expectLogLines(t, "service log", b)
	expectNoCanary(t, "service log", b, canaries)
}

// --- admin release and retry (service level) ---

// testAdminActions (sinks.md §1.7, architecture.md §4, §5.4): an uncertain
// outcome holds the job delivery_uncertain with the record in sending after
// one POST; release with the document ID the admin found marks it delivered
// with record confirmed_by_admin and sends nothing; retry sends exactly one
// new POST and ends with record sent.
func testAdminActions(t *testing.T) {
	s := newService(t)
	t.Run("release-with-document-id", func(t *testing.T) {
		s.docs.reset()
		s.docs.queue(fault{dropAfterCreate: true}) // created, answer lost
		_, _, jobID := s.startJob(t, ada)
		s.waitHeld(t, jobID, "delivery_uncertain")
		expectRequests(t, s.docs, 1)
		expectDelivery(t, s.job(t, jobID), "sending", "", "held job")
		found := expectOneDocument(t, s.docs).id
		body, _ := json.Marshal(map[string]string{"document_id": found})
		s.action(t, jobID, "release", string(body))
		s.waitDelivered(t, jobID)
		expectDelivery(t, s.job(t, jobID), "confirmed_by_admin", found, "released job")
		expectRequests(t, s.docs, 1) // release never sends
		r := s.postAction(t, jobID, "retry", "{}")
		expectStatus(t, r, http.StatusConflict, "POST retry on a delivered job")
		if !strings.Contains(string(r.body), "urn:recap:problem:invalid-job-state") {
			t.Errorf("retry on a delivered job: %s, want problem invalid-job-state", r)
		}
		expectRequests(t, s.docs, 1)
	})
	t.Run("retry", func(t *testing.T) {
		s.docs.reset()
		s.docs.queue(statusFault(http.StatusInternalServerError, bodyServerError))
		_, _, jobID := s.startJob(t, ada)
		s.waitHeld(t, jobID, "delivery_uncertain")
		expectRequests(t, s.docs, 1)
		expectDelivery(t, s.job(t, jobID), "sending", "", "held job")
		s.action(t, jobID, "retry", "{}")
		s.waitDelivered(t, jobID)
		expectRequests(t, s.docs, 2) // exactly one new POST
		doc := expectOneDocument(t, s.docs)
		if doc.ownerSub != ada.Sub {
			t.Errorf("document owner %q, want %s", doc.ownerSub, ada.Sub)
		}
		expectDelivery(t, s.job(t, jobID), "sent", doc.id, "retried job")
	})
}
