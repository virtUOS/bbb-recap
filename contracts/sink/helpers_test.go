package sink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// testdataDir is relative to this package directory (go test runs tests there).
const testdataDir = "../../testdata"

// toolTimeout bounds one tool run.
const toolTimeout = 2 * time.Minute

func testdataPath(t *testing.T, elem ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{testdataDir}, elem...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureKey is the Docs server-to-server key (fixtures.md §5), trimmed of
// the trailing newline as the service trims secret files.
func fixtureKey(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", "docs-s2s.key"))))
}

// --- identities (fixtures.md §5) -------------------------------------------

type owner struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

var (
	ada = owner{Sub: "fixture-sub-ada-0001", Email: "ada.testperson@example.org"}
	// adaByEmail: no Docs user has this sub, Ada has the email.
	adaByEmail = owner{Sub: "fixture-sub-unknown-0009", Email: "ada.testperson@example.org"}
	// nobody: no Docs account, so Docs invites the email.
	nobody = owner{Sub: "fixture-sub-unknown-0009", Email: "nobody.testperson@example.org"}
	// unresolvable: no sub match, two Docs users share the email (400).
	unresolvable = owner{Sub: "fixture-sub-unknown-0009", Email: "dup.testperson@example.org"}
)

// identityCanaries are the fixture subs and emails. No log line and no tool
// output may contain them (README "Logging"; privacy.md §6).
var identityCanaries = []string{
	"fixture-sub-ada-0001", "fixture-sub-dup-0007", "fixture-sub-dup-0008", "fixture-sub-unknown-0009",
	"ada.testperson@example.org", "dup.testperson@example.org", "nobody.testperson@example.org",
}

// --- delivery request (sinks.md §1.2) ---------------------------------------

type deliveryRequest struct {
	JobID         string `json:"job_id"`
	SinkProfile   string `json:"sink_profile"`
	Title         string `json:"title"`
	SummaryMD     string `json:"summary_md"`
	SummarySHA256 string `json:"summary_sha256"`
	Owner         owner  `json:"owner"`
}

// baseRequest is testdata/sink/request.json with testdata/sink/summary.md.
func baseRequest(t *testing.T) deliveryRequest {
	t.Helper()
	var r deliveryRequest
	if err := json.Unmarshal(readFile(t, testdataPath(t, "sink", "request.json")), &r); err != nil {
		t.Fatalf("testdata/sink/request.json: %v", err)
	}
	return r.withContent(string(readFile(t, testdataPath(t, "sink", "summary.md"))))
}

// withContent sets summary_md and its SHA-256.
func (r deliveryRequest) withContent(md string) deliveryRequest {
	r.SummaryMD = md
	sum := sha256.Sum256([]byte(md))
	r.SummarySHA256 = hex.EncodeToString(sum[:])
	return r
}

func (r deliveryRequest) withOwner(o owner) deliveryRequest { r.Owner = o; return r }

// contentCanaries are lines of summary.md that no log line may contain.
var contentCanaries = []string{
	"The team chose blue for the test widget",
	"Ben proposed blue because blue widgets passed every test",
	"Review the widget specification",
	"Fixture Widget Meeting",
}

// --- delivery record and tool output (README.md) ----------------------------

type record struct {
	JobID       string  `json:"job_id"`
	SinkProfile string  `json:"sink_profile"`
	State       string  `json:"state"`
	DocumentID  *string `json:"document_id"`
	SentAt      *string `json:"sent_at"`
}

type toolError struct {
	Class string `json:"class"`
	Code  string `json:"code"`
}

// output is the stdout of `recap tool sink`.
type output struct {
	Outcome       string     `json:"outcome"`
	HeldReason    *string    `json:"held_reason"`
	FailureReason *string    `json:"failure_reason"`
	Error         *toolError `json:"error"`
	Delivery      *record    `json:"delivery"`
	raw           []byte
}

// toolInput is the stdin of `recap tool sink`.
type toolInput struct {
	Request          deliveryRequest `json:"request"`
	Delivery         *record         `json:"delivery"`
	RequestTimeoutMS *int64          `json:"request_timeout_ms,omitempty"`
}

// --- running the tool --------------------------------------------------------

// sinkEnv is a tool environment: a sinks file pointing at one Docs URL and
// the CA bundle of the mock.
type sinkEnv struct {
	bin, sinksFile, certFile, text string
	extraEnv                       []string // more variables for the tool (the frontend sink's blob store)
}

// stderrLog collects the stderr of every tool run of this package for the
// log scans.
var stderrLog struct {
	sync.Mutex
	buf bytes.Buffer
}

// newSinkEnv writes the fixture sinks file with docsURL as docs_base_url and
// the fixture key path, and the mock's certificate for SSL_CERT_FILE.
func newSinkEnv(t *testing.T, srv *httptest.Server, docsURL string) sinkEnv {
	t.Helper()
	return newSinkEnvFrom(t, "sinks.yaml", srv, docsURL)
}

// newSinkEnvFrom is newSinkEnv with another sinks file of testdata/sink/.
func newSinkEnvFrom(t *testing.T, file string, srv *httptest.Server, docsURL string) sinkEnv {
	t.Helper()
	bin := harness.Bin(t)
	dir := t.TempDir()
	text := string(readFile(t, testdataPath(t, "sink", file)))
	text = strings.ReplaceAll(text, "/run/secrets/recap/sinks/", testdataPath(t, "secrets")+"/")
	text = replaceKey(t, text, "docs_base_url", docsURL)
	e := sinkEnv{bin: bin, sinksFile: filepath.Join(dir, "sinks.yaml"), certFile: filepath.Join(dir, "ca.pem"), text: text}
	writeFile(t, e.sinksFile, []byte(text))
	writeFile(t, e.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return e
}

// replaceKey sets the value of a key that appears exactly once in the YAML text.
func replaceKey(t *testing.T, text, key, value string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^(\s+` + regexp.QuoteMeta(key) + `:) .*$`)
	if n := len(re.FindAllString(text, -1)); n != 1 {
		t.Fatalf("sinks.yaml: key %s found %d times, want 1", key, n)
	}
	return re.ReplaceAllString(text, "${1} "+value)
}

// withValue returns a copy of e whose profile has key set to value.
func (e sinkEnv) withValue(t *testing.T, key, value string) sinkEnv {
	t.Helper()
	text := replaceKey(t, e.text, key, value)
	c := e
	c.sinksFile, c.text = filepath.Join(t.TempDir(), "sinks.yaml"), text
	writeFile(t, c.sinksFile, []byte(text))
	return c
}

// run runs `recap tool sink` with in on stdin at RECAP_LOG_LEVEL=debug. It
// fails the test unless the tool exits 0 with one JSON object on stdout.
func (e sinkEnv) run(t *testing.T, in toolInput) output {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "tool", "sink")
	cmd.Env = append(withoutRecapVars(os.Environ()),
		"RECAP_SINKS_FILE="+e.sinksFile, "RECAP_LOG_LEVEL=debug", "SSL_CERT_FILE="+e.certFile)
	cmd.Env = append(cmd.Env, e.extraEnv...)
	cmd.Stdin = bytes.NewReader(b)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	stderrLog.Lock()
	stderrLog.buf.Write(errOut.Bytes())
	stderrLog.buf.WriteByte('\n')
	stderrLog.Unlock()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		t.Fatalf("recap tool sink: exit %d, want 0\nstderr: %s", exitErr.ExitCode(), errOut.Bytes())
	case err != nil:
		t.Fatalf("recap tool sink: %v", err)
	}
	var o output
	d := json.NewDecoder(bytes.NewReader(out.Bytes()))
	if err := d.Decode(&o); err != nil || d.More() {
		t.Fatalf("recap tool sink: stdout is not one JSON object: %v\nstdout: %q", err, out.Bytes())
	}
	o.raw = out.Bytes()
	expectNoCanary(t, "tool stdout", o.raw, identityCanaries)
	return o
}

// withoutRecapVars drops inherited RECAP_* variables: the tool gets only the
// ones the README lists.
func withoutRecapVars(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "RECAP_") && !strings.HasPrefix(kv, "SSL_CERT_") {
			out = append(out, kv)
		}
	}
	return out
}

func ms(n int64) *int64 { return &n }

// --- checking outputs ----------------------------------------------------------

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// expectSent checks a 201 result: outcome ok and a sent record with the
// mock's document ID (sinks.md §1.3).
func expectSent(t *testing.T, o output, req deliveryRequest, documentID string) {
	t.Helper()
	if o.Outcome != "ok" || o.Error != nil || o.HeldReason != nil || o.FailureReason != nil {
		t.Fatalf("got %s, want outcome ok without error", o.raw)
	}
	r := o.Delivery
	if r == nil || r.State != "sent" || r.JobID != req.JobID || r.SinkProfile != req.SinkProfile || str(r.DocumentID) != documentID {
		t.Fatalf("delivery record %s, want state sent, job_id %s, sink_profile %s, document_id %s", o.raw, req.JobID, req.SinkProfile, documentID)
	}
	at, err := time.Parse(time.RFC3339Nano, str(r.SentAt))
	if err != nil || (!strings.HasSuffix(*r.SentAt, "Z") && !strings.HasSuffix(*r.SentAt, "+00:00")) {
		t.Fatalf("sent_at %q is not RFC 3339 UTC", str(r.SentAt))
	}
	if d := time.Since(at); d < -time.Minute || d > 10*time.Minute {
		t.Errorf("sent_at %s is not the time of the call", *r.SentAt)
	}
	for _, s := range []string{req.Title, req.SummaryMD, req.Owner.Sub, req.Owner.Email} {
		if strings.Contains(string(o.raw), s) {
			t.Errorf("tool output contains request content %q (the record has no content, title, email or sub)", s)
		}
	}
}

// expectClass checks a failed delivery after Docs answered or the connection
// failed: error class, step outcome, held or failure reason, and the delivery
// record left behind (README.md table). sinks.md §1.4 item 3: not_sent and
// any 4xx other than 429 delete the record, uncertain keeps it in sending.
func expectClass(t *testing.T, o output, class string) {
	t.Helper()
	expectClassRecord(t, o, class)
}

// expectRejectedWithoutCall checks a pre-check rejection. sinks.md §2.4: the
// check runs before step 1 of §1.4, so no delivery record is written.
func expectRejectedWithoutCall(t *testing.T, o output) {
	t.Helper()
	expectClass(t, o, "rejected")
}

func expectClassRecord(t *testing.T, o output, class string) {
	t.Helper()
	want := map[string]struct{ outcome, held, failure, state string }{
		"not_sent":             {"transient", "", "", ""},
		"uncertain":            {"hold", "delivery_uncertain", "", "sending"},
		"config":               {"permanent", "", "config_error", ""},
		"recipient_unresolved": {"hold", "owner_unresolved", "", ""},
		"rejected":             {"permanent", "", "rejected_by_sink", ""},
	}[class]
	if o.Error == nil || o.Error.Class != class {
		t.Fatalf("got %s, want error class %s", o.raw, class)
	}
	if o.Error.Code == "" {
		t.Errorf("error %s has no code", o.raw)
	}
	if o.Outcome != want.outcome || str(o.HeldReason) != want.held || str(o.FailureReason) != want.failure {
		t.Fatalf("got %s, want outcome %s, held_reason %q, failure_reason %q", o.raw, want.outcome, want.held, want.failure)
	}
	switch {
	case want.state == "" && o.Delivery != nil:
		t.Fatalf("got delivery %s, want null: a %s result leaves no record, so a later call may send", o.raw, class)
	case want.state != "" && (o.Delivery == nil || o.Delivery.State != want.state):
		t.Fatalf("got %s, want the delivery record in state %s", o.raw, want.state)
	case want.state != "" && str(o.Delivery.DocumentID) != "":
		t.Fatalf("got %s, want no document_id while sending", o.raw)
	}
}

// expectRequests fails unless the mock received exactly n requests.
func expectRequests(t *testing.T, m *mockDocs, n int) []docsRequest {
	t.Helper()
	got := m.received()
	if len(got) != n {
		t.Fatalf("mock Docs received %d requests, want %d", len(got), n)
	}
	return got
}

// expectOneDocument fails unless the mock holds exactly one document, and
// returns it.
func expectOneDocument(t *testing.T, m *mockDocs) document {
	t.Helper()
	docs := m.documents()
	if len(docs) != 1 {
		t.Fatalf("mock Docs holds %d documents, want 1", len(docs))
	}
	return docs[0]
}

// expectCreateRequest checks one request against sinks.md §3.2: method,
// path, no query, bearer key, JSON body with exactly title, content, sub and
// email, values unchanged.
func expectCreateRequest(t *testing.T, r docsRequest, key string, req deliveryRequest) {
	t.Helper()
	if r.method != "POST" || r.path != createPath || r.query != "" {
		t.Fatalf("request %s %s?%s, want POST %s", r.method, r.path, r.query, createPath)
	}
	if got := r.header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+key {
		t.Fatalf("Authorization header is not exactly \"Bearer <fixture key>\" (%d values)", len(got))
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("request body is not a JSON object: %v", err)
	}
	want := map[string]string{"title": req.Title, "content": req.SummaryMD, "sub": req.Owner.Sub, "email": req.Owner.Email}
	for k := range body {
		if _, ok := want[k]; !ok {
			t.Errorf("request body has field %q; sinks.md §3.2 sends only title, content, sub, email", k)
		}
	}
	for k, v := range want {
		var got string
		if err := json.Unmarshal(body[k], &got); err != nil {
			t.Errorf("request field %q missing or not a string", k)
			continue
		}
		if got != v {
			t.Errorf("request field %q differs from the delivery request (%d vs %d bytes)", k, len(got), len(v))
		}
	}
}

// --- log scans -------------------------------------------------------------------

// expectNoCanary fails for every canary found in b (case-insensitive).
func expectNoCanary(t *testing.T, what string, b []byte, canaries []string) {
	t.Helper()
	low := strings.ToLower(string(b))
	for _, c := range canaries {
		if c != "" && strings.Contains(low, strings.ToLower(c)) {
			t.Errorf("%s contains %q", what, c)
		}
	}
}

// --- Markdown dialect (sinks.md §2) ------------------------------------------------

// dialectErrors is the test's own scan for the forbidden constructs of
// sinks.md §2.2 that it can tell apart reliably: raw HTML and autolinks,
// images, reference links and definitions, footnotes, setext headings. Code
// spans, fenced code and backslash escapes are skipped.
func dialectErrors(md string) []string {
	var errs []string
	inFence := false
	prev := ""
	for i, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "```") {
			inFence = !inFence
			prev = ""
			continue
		}
		if inFence {
			continue
		}
		s := escapedChar.ReplaceAllString(line, "")
		s = codeSpan.ReplaceAllString(s, "")
		for _, f := range forbiddenInline {
			if f.re.MatchString(s) {
				errs = append(errs, f.name+" in line "+strconv.Itoa(i+1))
			}
		}
		if setextLine.MatchString(line) && strings.TrimSpace(prev) != "" && !blockStart.MatchString(prev) {
			errs = append(errs, "setext heading in line "+strconv.Itoa(i+1))
		}
		prev = line
	}
	return errs
}

var (
	escapedChar     = regexp.MustCompile(`\\.`)
	codeSpan        = regexp.MustCompile("`[^`]*`")
	setextLine      = regexp.MustCompile(`^ {0,3}(=+|-+)\s*$`)
	blockStart      = regexp.MustCompile(`^ {0,3}([-*+>|#]|[0-9]+\.|---)`)
	forbiddenInline = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"raw HTML or autolink", regexp.MustCompile(`<[A-Za-z!/?]`)},
		{"image", regexp.MustCompile(`!\[`)},
		{"footnote", regexp.MustCompile(`\[\^`)},
		{"reference link", regexp.MustCompile(`\]\[`)},
		{"link reference definition", regexp.MustCompile(`^ {0,3}\[[^\]]+\]:`)},
	}
)

func stderrLen() int {
	stderrLog.Lock()
	defer stderrLog.Unlock()
	return stderrLog.buf.Len()
}

// stderrSince returns the tool stderr collected after mark.
func stderrSince(mark int) []byte {
	stderrLog.Lock()
	defer stderrLog.Unlock()
	return append([]byte(nil), stderrLog.buf.Bytes()[mark:]...)
}

// expectLogLines checks the log format of README "Logging": one JSON object
// per line with time (RFC 3339), level (DEBUG, INFO, WARN, ERROR; compared
// case-insensitively) and msg. Empty lines are skipped.
func expectLogLines(t *testing.T, what string, b []byte) {
	t.Helper()
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Errorf("%s line %d is not a JSON object: %v", what, i+1, err)
			continue
		}
		ts, _ := l["time"].(string)
		level, _ := l["level"].(string)
		_, hasMsg := l["msg"].(string)
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil || !hasMsg ||
			!slices.Contains([]string{"debug", "info", "warn", "error"}, strings.ToLower(level)) {
			t.Errorf("%s line %d lacks time (RFC 3339), level or msg", what, i+1)
		}
	}
}
