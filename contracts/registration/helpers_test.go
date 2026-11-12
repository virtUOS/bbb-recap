package registration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Paths relative to this package directory (go test runs tests there).
const (
	testdataDir = "../../testdata"
	schemaDir   = "../../docs/reference/schemas"
)

// Fixture names (docs/reference/fixtures.md §3, §5).
const (
	frontendID = "pilos-fixture"
	serverA    = "bbb-fixture-a" // may be claimed by pilos-fixture
	serverB    = "bbb-fixture-b" // may not be claimed by pilos-fixture
	adminName  = "fixture-admin"

	problemPrefix = "urn:recap:problem:"
)

// Waiting for the service. validate runs within a few worker polls (2 s,
// architecture.md §6.2); a run through to deliver takes every step with mocks.
const (
	validateTimeout = 60 * time.Second
	pipelineTimeout = 3 * time.Minute
	pollInterval    = time.Second
)

// owner is the owner object of a registration request.
type owner struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// Fixture identities (fixtures.md §5). Mock Docs knows Ada; fixture-sub-unknown-0009
// has no account, and two mock users share dup.testperson@example.org, so the
// pair below matches no sub and two emails: Docs answers 400 (sinks.md §3.3).
var (
	ada          = owner{Sub: "fixture-sub-ada-0001", Email: "ada.testperson@example.org"}
	unresolvable = owner{Sub: "fixture-sub-unknown-0009", Email: "dup.testperson@example.org"}
	dup          = owner{Sub: "fixture-sub-dup-0007", Email: "dup.testperson@example.org"}
	longSub      = strings.Repeat("s", 255) // the longest valid sub (§2.3)
)

// allOwnerValues are all sub and email values this package sends. No log line
// and no response may contain them.
var allOwnerValues = []string{ada.Sub, ada.Email, unresolvable.Sub, unresolvable.Email, dup.Sub, dup.Email, longSub}

// env is a client for the service under test.
type env struct {
	base string
	http *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := strings.TrimRight(harness.BaseURL(t), "/")
	return &env{base: base, http: &http.Client{Timeout: 30 * time.Second}}
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

// do sends one request with a JSON body (or none). An empty token sends no
// Authorization header.
func (e *env) do(t *testing.T, method, path, token string, body []byte) reply {
	t.Helper()
	return e.send(t, method, path, token, "application/json", body)
}

// send sends one request with the given body Content-Type.
func (e *env) send(t *testing.T, method, path, token, contentType string, body []byte) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

// credential reads a fixture client credential (fixtures.md §5).
func credential(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataDir, "secrets", "clients", name))
	if err != nil {
		t.Fatalf("fixture credential: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func frontendToken(t *testing.T) string { return credential(t, "frontend."+frontendID) }
func serverToken(t *testing.T, id string) string {
	return credential(t, "bbb_server."+id)
}
func adminToken(t *testing.T) string { return credential(t, "admin."+adminName) }

// otherFrontendToken is the credential of a second front end, which may not
// see or change pilos-fixture's registrations (metadata-interface.md §2.1).
func otherFrontendToken(t *testing.T) string { return credential(t, "frontend.other-fixture") }

// freshRecordID returns a new record_id shaped like BBB's <40 hex>-<epoch ms>
// (metadata-interface.md §2.3), so runs never collide.
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

// expectStatus fails the test unless r has the wanted status.
func expectStatus(t *testing.T, r reply, want int, what string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("%s: got %s, want HTTP %d", what, r, want)
	}
}

// expectProblem checks an RFC 9457 problem with type urn:recap:problem:<slug>
// (README "HTTP"; metadata-interface.md §2.6).
func expectProblem(t *testing.T, r reply, status int, slug, what string) map[string]any {
	t.Helper()
	expectStatus(t, r, status, what)
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("%s: Content-Type %q, want application/problem+json", what, ct)
	}
	var p map[string]any
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("%s: problem body is not JSON: %v", what, err)
	}
	if got := p["type"]; got != problemPrefix+slug {
		t.Fatalf("%s: problem type %v, want %s", what, got, problemPrefix+slug)
	}
	if s, ok := p["status"]; ok {
		if n, isNum := s.(float64); !isNum || int(n) != status {
			t.Fatalf("%s: problem status %v, want %d", what, s, status)
		}
	}
	expectNoOwnerData(t, r, what)
	return p
}

// expectNoOwnerData fails if a response carries an owner object or any owner value.
func expectNoOwnerData(t *testing.T, r reply, what string) {
	t.Helper()
	low := strings.ToLower(string(r.body))
	for _, v := range allOwnerValues {
		if strings.Contains(low, strings.ToLower(v)) {
			t.Fatalf("%s: response contains owner value %q", what, v)
		}
	}
	var m map[string]any
	if json.Unmarshal(r.body, &m) == nil {
		if _, ok := m["owner"]; ok {
			t.Fatalf("%s: response contains an owner field", what)
		}
	}
}

// --- registration API (metadata-interface.md §2) ---

func regPath(recordID string) string { return "/api/v1/registrations/" + url.PathEscape(recordID) }

func regBody(t *testing.T, recordID, meetingID string, o owner) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"schema_version": "1.0.0",
		"meeting_id":     meetingID,
		"record_id":      recordID,
		"owner":          o,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) putReg(t *testing.T, token, recordID string, body []byte) reply {
	t.Helper()
	return e.do(t, http.MethodPut, regPath(recordID), token, body)
}

func (e *env) getReg(t *testing.T, token, recordID string) reply {
	t.Helper()
	return e.do(t, http.MethodGet, regPath(recordID), token, nil)
}

func (e *env) deleteReg(t *testing.T, token, recordID string) reply {
	t.Helper()
	return e.do(t, http.MethodDelete, regPath(recordID), token, nil)
}

// regResponse is the registration response ($defs/registration_response).
type regResponse struct {
	SchemaVersion string  `json:"schema_version"`
	RecordID      string  `json:"record_id"`
	MeetingID     string  `json:"meeting_id"`
	FrontendID    string  `json:"frontend_id"`
	Status        string  `json:"status"`
	ExpiresAt     *string `json:"expires_at"`
	Job           *regJob `json:"job"`
}

// regJob is the job of a matched registration. The result fields (§2.7,
// ADR-014; schema_version 1.1.0) are null/false until delivery.
type regJob struct {
	JobID            string  `json:"job_id"`
	State            string  `json:"state"`
	HeldReason       *string `json:"held_reason"`
	Outcome          *string `json:"outcome"`
	DeliveredAt      *string `json:"delivered_at"`
	DocumentURL      *string `json:"document_url"`
	SummaryAvailable *bool   `json:"summary_available"`
}

// expectRegistration checks a 200/201 registration response against the
// schema's key set and the field rules of §2.3, and returns it.
func expectRegistration(t *testing.T, r reply, status int, recordID, meetingID, wantStatus, what string) regResponse {
	t.Helper()
	expectStatus(t, r, status, what)
	expectNoOwnerData(t, r, what)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("%s: body is not a JSON object: %v", what, err)
	}
	required, allowed := responseKeys(t)
	for _, k := range required {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s: response lacks required field %q", what, k)
		}
	}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("%s: response has field %q not in the schema", what, k)
		}
	}
	var resp regResponse
	if err := json.Unmarshal(r.body, &resp); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !strings.HasPrefix(resp.SchemaVersion, "1.") {
		t.Errorf("%s: schema_version %q, want major 1", what, resp.SchemaVersion)
	}
	if resp.RecordID != recordID || resp.MeetingID != meetingID || resp.FrontendID != frontendID {
		t.Errorf("%s: ids (%s, %s, %s), want (%s, %s, %s)", what,
			resp.RecordID, resp.MeetingID, resp.FrontendID, recordID, meetingID, frontendID)
	}
	if resp.Status != wantStatus {
		t.Fatalf("%s: status %q, want %q", what, resp.Status, wantStatus)
	}
	if (resp.Status == "registered") != (resp.ExpiresAt != nil) {
		t.Errorf("%s: status %q with expires_at %v: expires_at is set exactly when registered", what, resp.Status, resp.ExpiresAt)
	}
	if resp.Status == "registered" && resp.Job != nil {
		t.Errorf("%s: registered but job is set", what)
	}
	return resp
}

// responseKeys reads required and allowed keys of $defs/registration_response.
func responseKeys(t *testing.T) ([]string, map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(schemaDir, "registration.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs struct {
			Resp struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"registration_response"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for k := range s.Defs.Resp.Properties {
		allowed[k] = true
	}
	return s.Defs.Resp.Required, allowed
}

// register creates a registration and expects 201.
func (e *env) register(t *testing.T, recordID, meetingID string, o owner) {
	t.Helper()
	r := e.putReg(t, frontendToken(t), recordID, regBody(t, recordID, meetingID, o))
	expectRegistration(t, r, http.StatusCreated, recordID, meetingID, "registered", "PUT new registration")
}

// registerLate registers for a record whose package is already held: the PUT
// may answer registered or already matched.
func (e *env) registerLate(t *testing.T, recordID, meetingID string, o owner) {
	t.Helper()
	r := e.putReg(t, frontendToken(t), recordID, regBody(t, recordID, meetingID, o))
	expectStatus(t, r, http.StatusCreated, "PUT late registration")
	var resp regResponse
	_ = json.Unmarshal(r.body, &resp)
	want := "registered"
	if resp.Status == "matched" {
		want = "matched"
	}
	expectRegistration(t, r, http.StatusCreated, recordID, meetingID, want, "PUT late registration")
}

// waitMatched polls GET until the registration is matched to jobID.
func (e *env) waitMatched(t *testing.T, recordID, meetingID, jobID string) {
	t.Helper()
	deadline := time.Now().Add(validateTimeout)
	var last string
	for time.Now().Before(deadline) {
		r := e.getReg(t, frontendToken(t), recordID)
		expectStatus(t, r, http.StatusOK, "GET registration")
		var resp regResponse
		_ = json.Unmarshal(r.body, &resp)
		if resp.Status == "matched" {
			resp = expectRegistration(t, r, http.StatusOK, recordID, meetingID, "matched", "GET matched registration")
			if resp.Job == nil || resp.Job.JobID != jobID {
				t.Fatalf("registration matched, job %+v, want job_id %s", resp.Job, jobID)
			}
			return
		}
		last = resp.Status
		time.Sleep(pollInterval)
	}
	t.Fatalf("registration %s not matched within %s (last status %q)", recordID, validateTimeout, last)
}

// --- admin API (architecture.md §4, §6.1) ---

// jobView holds the fields of the admin job body (architecture.md §4) that the
// tests read. Values that do not apply are null, which decodes to "".
type jobView struct {
	JobID         string `json:"job_id"`
	State         string `json:"state"`
	HeldReason    string `json:"held_reason"`
	FailureReason string `json:"failure_reason"`
	Outcome       string `json:"outcome"`
	Detail        string `json:"detail"`
}

func (j jobView) String() string {
	return "state=" + j.State + " held_reason=" + j.HeldReason + " failure_reason=" + j.FailureReason + " outcome=" + j.Outcome + " detail=" + j.Detail
}

func (e *env) job(t *testing.T, jobID string) jobView {
	t.Helper()
	r := e.do(t, http.MethodGet, "/api/v1/admin/jobs/"+url.PathEscape(jobID), adminToken(t), nil)
	expectStatus(t, r, http.StatusOK, "GET admin job")
	var j jobView
	if err := json.Unmarshal(r.body, &j); err != nil {
		t.Fatalf("admin job: %v", err)
	}
	if j.JobID != jobID {
		t.Fatalf("admin job: job_id %q, want %s", j.JobID, jobID)
	}
	return j
}

// waitJob polls the admin API until ok(job) holds. It stops early when the job
// reaches failed or cleaned without satisfying ok.
func (e *env) waitJob(t *testing.T, jobID string, timeout time.Duration, want string, ok func(jobView) bool) jobView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := e.job(t, jobID)
		if ok(j) {
			return j
		}
		if j.State == "failed" || j.State == "cleaned" || time.Now().After(deadline) {
			t.Fatalf("job %s: %s, want %s", jobID, j, want)
		}
		time.Sleep(pollInterval)
	}
}

func (e *env) waitHeld(t *testing.T, jobID string, timeout time.Duration, reason string) jobView {
	t.Helper()
	return e.waitJob(t, jobID, timeout, "held/"+reason, func(j jobView) bool {
		return j.State == "held" && j.HeldReason == reason
	})
}

// --- ingest API (bbb-exporter.md §6.2) ---

func ingestPath(packageID string) string { return "/api/v1/ingest/packages/" + packageID }

type ingestStatus struct {
	State   string   `json:"state"`
	Missing []string `json:"missing"`
}

type ingestAck struct {
	PackageID string `json:"package_id"`
	State     string `json:"state"`
	JobID     string `json:"job_id"`
	RecordID  string `json:"record_id"`
}

func (e *env) putManifest(t *testing.T, p *pkg, token string) reply {
	t.Helper()
	return e.do(t, http.MethodPut, ingestPath(p.id), token, p.manifest)
}

// ingest pushes p as the exporter does (manifest, missing files, commit) and
// returns the job_id of the ack.
func (e *env) ingest(t *testing.T, p *pkg) string {
	t.Helper()
	token := serverToken(t, p.server)
	r := e.putManifest(t, p, token)
	expectStatus(t, r, http.StatusCreated, "PUT manifest")
	var st ingestStatus
	if err := json.Unmarshal(r.body, &st); err != nil {
		t.Fatalf("IngestStatus: %v", err)
	}
	for _, sha := range st.Missing {
		data, ok := p.files[sha]
		if !ok {
			t.Fatalf("service asks for %s, which is not in the manifest", sha)
		}
		r := e.send(t, http.MethodPut, ingestPath(p.id)+"/files/"+sha, token, "application/octet-stream", data)
		if r.status != http.StatusNoContent && r.status != http.StatusOK {
			t.Fatalf("PUT file %s: got %s, want 204 or 200", sha, r)
		}
	}
	r = e.do(t, http.MethodPost, ingestPath(p.id)+"/commit", token, []byte("{}"))
	expectStatus(t, r, http.StatusOK, "POST commit")
	var ack ingestAck
	if err := json.Unmarshal(r.body, &ack); err != nil {
		t.Fatalf("IngestAck: %v", err)
	}
	if ack.State != "committed" || ack.RecordID != p.recordID || ack.JobID == "" {
		t.Fatalf("IngestAck %+v does not match the package (record_id %s)", ack, p.recordID)
	}
	return ack.JobID
}
