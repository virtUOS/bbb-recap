package registration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Results in the front end (metadata-interface.md §2.7, ADR-014): the result
// fields of the registration response (CT-REG-08), the summary sub-resource
// (CT-REG-09) and the schema of the response (CT-REG-01, response half).

// Contract environment names (contracts/README.md, tools/contract-env/config/sinks.yaml).
const (
	// frontendSink is the contract environment's sink profile of type
	// frontend (sinks.md §4a).
	frontendSink = "frontend-contract"
	// docsMainURLTemplate is docs-main's document_url_template in the
	// contract environment (sinks.md §3.1).
	docsMainURLTemplate = "https://docs.recap.test/docs/{id}/"
	// resultTTLVar is the RECAP_RESULT_TTL the service under test runs with
	// (contracts/README.md). Unset: the lifetime tests skip.
	resultTTLVar = "RECAP_CONTRACT_RESULT_TTL"
)

// Waiting for the lifetime tests. The contract environment runs cleanup
// RECAP_RETENTION_AFTER_DELIVERY (1h) after delivery; the cleanup loop runs
// hourly (architecture.md §8).
const (
	cleanupWait = 75 * time.Minute
	loopWait    = 65 * time.Minute
	slowPoll    = 15 * time.Second
)

// --- CT-REG-01, response half: schema against fixtures (no service) ---

// TestResponseSchema checks schemas/registration.schema.json against the
// fixtures, without a service: every testdata/registration/responses/valid.*
// is a valid registration_response and every invalid.* is not; the request
// fixtures of CT-REG-01 are valid requests and its invalid edits are not.
func TestResponseSchema(t *testing.T) {
	t.Run("CT-REG-01 response schema", func(t *testing.T) {
		resp := compileSchema(t, "#/$defs/registration_response")
		for _, kind := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join(testdataDir, "registration", "responses", kind+".*.json"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no %s response fixtures: %v", kind, err)
			}
			for _, f := range files {
				t.Run(filepath.Base(f), func(t *testing.T) {
					b, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					err = validate(resp, b)
					if kind == "valid" && err != nil {
						t.Fatalf("schema rejects a valid response: %v", err)
					}
					if kind == "invalid" && err == nil {
						t.Fatal("schema accepts an invalid response")
					}
				})
			}
		}
	})
	t.Run("CT-REG-01 request schema unchanged", func(t *testing.T) {
		req := compileSchema(t, "")
		for _, id := range []string{"F-01", "F-04", "F-06"} {
			if err := validate(req, mustJSON(t, fixtureRegistration(t, id))); err != nil {
				t.Errorf("%s registration.json: schema rejects it: %v", id, err)
			}
		}
		for _, rid := range []string{"a", "0" + strings.Repeat("a_-", 84) + "yz"} {
			m := fixtureRegistration(t, "F-01")
			m["record_id"] = rid
			if err := validate(req, mustJSON(t, m)); err != nil {
				t.Errorf("record_id of %d characters: schema rejects it: %v", len(rid), err)
			}
		}
		for _, c := range invalidRequests {
			m := fixtureRegistration(t, "F-01")
			c.edit(m)
			if err := validate(req, mustJSON(t, m)); err == nil {
				t.Errorf("invalid/%s: schema accepts it", c.name)
			}
		}
	})
}

// compileSchema compiles registration.schema.json, at the JSON pointer
// fragment if given, with formats asserted (draft 2020-12).
func compileSchema(t *testing.T, fragment string) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open(filepath.Join(schemaDir, "registration.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseRegexpEngine(ecmaRegexp)
	const loc = "urn:recap:test:registration.schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(loc + fragment)
	if err != nil {
		t.Fatalf("compile registration.schema.json%s: %v", fragment, err)
	}
	return s
}

// unicodeEscape is ECMA-262's \uXXXX, which Go's regexp spells \x{XXXX}. The
// schema patterns use no other ECMA-only syntax.
var unicodeEscape = regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`)

func ecmaRegexp(pattern string) (jsonschema.Regexp, error) {
	return regexp.Compile(unicodeEscape.ReplaceAllString(pattern, `\x{$1}`))
}

func validate(s *jsonschema.Schema, b []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return s.Validate(v)
}

// --- service helpers ---

// expectResult checks a 200 response of a matched registration: the schema
// (with the result fields), schema_version 1.1.0 or later, a job. It returns
// the job.
func expectResult(t *testing.T, r reply, recordID, meetingID, what string) regJob {
	t.Helper()
	resp := expectRegistration(t, r, http.StatusOK, recordID, meetingID, "matched", what)
	if err := validate(compileSchema(t, "#/$defs/registration_response"), r.body); err != nil {
		t.Fatalf("%s: response violates the schema: %v\n%s", what, err, r.body)
	}
	if minor(resp.SchemaVersion) < 1 {
		t.Errorf("%s: schema_version %q, want 1.1.0 or later (§2.3)", what, resp.SchemaVersion)
	}
	if resp.Job == nil {
		t.Fatalf("%s: matched without job", what)
	}
	return *resp.Job
}

func minor(semver string) int {
	parts := strings.Split(semver, ".")
	if len(parts) != 3 {
		return -1
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return n
}

// expectNoResult: before delivery the result fields are null and false (§2.3).
func expectNoResult(t *testing.T, j regJob, what string) {
	t.Helper()
	if j.Outcome != nil || j.DeliveredAt != nil || j.DocumentURL != nil || j.SummaryAvailable == nil || *j.SummaryAvailable {
		t.Errorf("%s: result fields %s, want outcome, delivered_at, document_url null and summary_available false", what, j)
	}
}

func (j regJob) String() string {
	s := func(p *string) string {
		if p == nil {
			return "null"
		}
		return strconv.Quote(*p)
	}
	b := "null"
	if j.SummaryAvailable != nil {
		b = strconv.FormatBool(*j.SummaryAvailable)
	}
	return fmt.Sprintf("{job_id %s state %s outcome %s delivered_at %s document_url %s summary_available %s}",
		j.JobID, j.State, s(j.Outcome), s(j.DeliveredAt), s(j.DocumentURL), b)
}

// expectDelivered checks the result fields after a delivery that happened
// after since (§2.7) and returns delivered_at. documentURL "" means null.
func expectDelivered(t *testing.T, j regJob, since time.Time, documentURL string, summary bool, what string) time.Time {
	t.Helper()
	if j.Outcome == nil || *j.Outcome != "delivered" {
		t.Errorf("%s: outcome %s, want delivered", what, j)
	}
	var at time.Time
	if j.DeliveredAt == nil {
		t.Errorf("%s: delivered_at null after delivery", what)
	} else {
		var err error
		if at, err = time.Parse(time.RFC3339Nano, *j.DeliveredAt); err != nil {
			t.Errorf("%s: delivered_at %q is not RFC 3339", what, *j.DeliveredAt)
		} else if at.Before(since.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
			t.Errorf("%s: delivered_at %s is not the time of this delivery (after %s)", what, at, since)
		}
	}
	switch {
	case documentURL == "" && j.DocumentURL != nil:
		t.Errorf("%s: document_url %q, want null", what, *j.DocumentURL)
	case documentURL != "" && (j.DocumentURL == nil || *j.DocumentURL != documentURL):
		t.Errorf("%s: %s, want document_url %q", what, j, documentURL)
	}
	if j.SummaryAvailable == nil || *j.SummaryAvailable != summary {
		t.Errorf("%s: %s, want summary_available %t", what, j, summary)
	}
	return at
}

// adminJob holds the admin job body fields (architecture.md §4) these tests read.
type adminJob struct {
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	Outcome  string `json:"outcome"`
	Delivery *struct {
		State      string  `json:"state"`
		DocumentID *string `json:"document_id"`
	} `json:"delivery"`
}

func (e *env) adminJob(t *testing.T, jobID string) adminJob {
	t.Helper()
	r := e.do(t, http.MethodGet, "/api/v1/admin/jobs/"+url.PathEscape(jobID), adminToken(t), nil)
	expectStatus(t, r, http.StatusOK, "GET admin job")
	var j adminJob
	if err := json.Unmarshal(r.body, &j); err != nil || j.JobID != jobID {
		t.Fatalf("admin job %s: %v %s", jobID, err, r)
	}
	return j
}

// documentURL is docs-main's link for the document of a delivered job.
func (e *env) documentURL(t *testing.T, jobID string) string {
	t.Helper()
	j := e.adminJob(t, jobID)
	if j.Delivery == nil || j.Delivery.DocumentID == nil || *j.Delivery.DocumentID == "" {
		t.Fatalf("admin job %s: no delivery document_id", jobID)
	}
	return strings.ReplaceAll(docsMainURLTemplate, "{id}", *j.Delivery.DocumentID)
}

// deliveredVia registers Ada, ingests F-01 with recap-sink set to sink and
// waits until the job is delivered.
func deliveredVia(t *testing.T, e *env, sink string) (rid, mid, jobID string) {
	t.Helper()
	rid, mid = freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, ada)
	jobID = e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: with(optedIn(), "recap-sink", sink)}))
	e.waitJob(t, jobID, pipelineTimeout, "delivered", func(j jobView) bool {
		if j.State == "held" { // no hold is expected on this path: fail at once
			t.Fatalf("job %s: %s, want delivered", jobID, j)
		}
		return j.State == "delivered"
	})
	return rid, mid, jobID
}

func summaryPath(recordID string) string { return regPath(recordID) + "/summary" }

func (e *env) getSummary(t *testing.T, token, recordID string) reply {
	t.Helper()
	return e.do(t, http.MethodGet, summaryPath(recordID), token, nil)
}

func (e *env) deleteSummary(t *testing.T, token, recordID string) reply {
	t.Helper()
	return e.do(t, http.MethodDelete, summaryPath(recordID), token, nil)
}

// expectNoSummary: 404 summary-not-available (§2.6, §2.7), the answer for
// every missing summary, also when the registration no longer exists or
// belongs to another front end.
func expectNoSummary(t *testing.T, r reply, what string) map[string]any {
	t.Helper()
	return expectProblem(t, r, http.StatusNotFound, "summary-not-available", what)
}

// expectMarkdown checks a 200 summary answer: text/markdown; charset=utf-8,
// valid UTF-8, the summary.md layout of templates.md §5.4 for F-01 (header
// with the meeting name, an Overview section), no owner data.
func expectMarkdown(t *testing.T, r reply, what string) {
	t.Helper()
	expectStatus(t, r, http.StatusOK, what)
	mt, params, err := mime.ParseMediaType(r.header.Get("Content-Type"))
	if err != nil || mt != "text/markdown" || !strings.EqualFold(params["charset"], "utf-8") {
		t.Errorf("%s: Content-Type %q, want text/markdown; charset=utf-8", what, r.header.Get("Content-Type"))
	}
	if !utf8.Valid(r.body) {
		t.Fatalf("%s: body is not UTF-8", what)
	}
	if !bytes.HasPrefix(r.body, []byte("*Fixture Widget Meeting · ")) || !bytes.Contains(r.body, []byte("\n## Overview\n")) {
		t.Fatalf("%s: body is not the rendered summary.md of F-01 (templates.md §5.4): %.200q", what, r.body)
	}
	expectNoOwnerData(t, r, what)
}

// summaryLines collects the lines of every summary the tests fetched, as
// canaries for the log scan.
var summaryLines struct {
	sync.Mutex
	lines []string
}

func rememberSummary(body []byte) {
	summaryLines.Lock()
	defer summaryLines.Unlock()
	for _, l := range strings.Split(string(body), "\n") {
		// Long lines only: short ones ("## Topics", table rows) may occur in
		// logs for other reasons. The footer carries IDs and times.
		if l = strings.TrimSpace(l); len(l) >= 24 && !strings.HasPrefix(l, "*meeting-summary ") {
			summaryLines.lines = append(summaryLines.lines, l)
		}
	}
}

// --- CT-REG-08, CT-REG-09 ---

// TestResults pins §2.7 against the running service. The lifetime parts
// (after cleanup, RECAP_RESULT_TTL, erase) are in TestResultLifetime.
func TestResults(t *testing.T) {
	t.Run("CT-REG-08 result fields", testResultFields)
	t.Run("CT-REG-09 summary", testSummary)
	t.Run("CT-REG-09 no summary content in logs", testSummaryLogs)
}

func testResultFields(t *testing.T) {
	e := newEnv(t)
	t.Run("null-before-delivery", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada) // registered: job null (expectRegistration)
		rid, mid, jobID := heldOwnerUnresolved(t, e)
		j := expectResult(t, e.getReg(t, frontendToken(t), rid), rid, mid, "GET with a held job")
		if j.JobID != jobID || j.State != "held" {
			t.Fatalf("job %s, want %s held", j, jobID)
		}
		expectNoResult(t, j, "held job")
	})
	t.Run("docs-sink-delivered", func(t *testing.T) {
		start := time.Now()
		rid, mid, jobID := deliveredVia(t, e, "docs-main")
		expectStillRegistered(t, e, rid)
		want := e.documentURL(t, jobID)
		j := expectResult(t, e.getReg(t, frontendToken(t), rid), rid, mid, "GET after delivery")
		if j.JobID != jobID || j.State != "delivered" {
			t.Fatalf("job %s, want %s delivered", j, jobID)
		}
		expectDelivered(t, j, start, want, false, "Docs sink")
		r := e.putReg(t, frontendToken(t), rid, regBody(t, rid, mid, ada))
		expectDelivered(t, expectResult(t, r, rid, mid, "PUT identical after delivery"), start, want, false, "PUT identical after delivery")
	})
	t.Run("frontend-sink-delivered", func(t *testing.T) {
		start := time.Now()
		rid, mid, jobID := deliveredVia(t, e, frontendSink)
		expectStillRegistered(t, e, rid)
		j := expectResult(t, e.getReg(t, frontendToken(t), rid), rid, mid, "GET after delivery")
		if j.JobID != jobID {
			t.Fatalf("job %s, want %s", j, jobID)
		}
		expectDelivered(t, j, start, "", true, "frontend sink")
	})
	t.Run("rerun-replaces-result", func(t *testing.T) {
		start := time.Now()
		rid, mid, oldJob := deliveredVia(t, e, "docs-main")
		expectStillRegistered(t, e, rid)
		oldURL := e.documentURL(t, oldJob)
		r := e.do(t, http.MethodPost, "/api/v1/admin/jobs/"+url.PathEscape(oldJob)+"/rerun", adminToken(t), []byte("{}"))
		expectStatus(t, r, http.StatusOK, "POST rerun")
		var nj adminJob
		if err := json.Unmarshal(r.body, &nj); err != nil || nj.JobID == "" || nj.JobID == oldJob {
			t.Fatalf("POST rerun: answer is not the new job: %v %s", err, r)
		}
		rerunAt := time.Now()
		e.waitJob(t, nj.JobID, pipelineTimeout, "delivered", func(j jobView) bool { return j.State == "delivered" })
		want := e.documentURL(t, nj.JobID)
		if want == oldURL {
			t.Fatalf("rerun delivered the same document %s (architecture.md §5.4: a new document)", want)
		}
		j := expectResult(t, e.getReg(t, frontendToken(t), rid), rid, mid, "GET after rerun")
		if j.JobID != nj.JobID {
			t.Fatalf("job %s, want the rerun job %s", j, nj.JobID)
		}
		at := expectDelivered(t, j, start, want, false, "rerun")
		if !at.IsZero() && at.Before(rerunAt.Add(-time.Second)) {
			t.Errorf("delivered_at %s is older than the rerun (%s)", at, rerunAt)
		}
	})
	t.Run("ttl-0-gone-at-cleanup", func(t *testing.T) {
		t.Skip("not run: RECAP_RESULT_TTL=0 needs a second service configuration (RECAP_RETENTION_AFTER_DELIVERY 0, " +
			"no frontend profile) beside the contract environment; internal/steps/cleanup TestResultTTLZero covers it")
	})
}

func testSummary(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	t.Run("registered-not-delivered-404", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		expectNoSummary(t, e.getSummary(t, token, rid), "GET summary before any package")
		expectStatus(t, e.deleteSummary(t, token, rid), http.StatusNoContent, "DELETE summary before any package")
	})
	t.Run("held-before-delivery-404", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: with(optedIn(), "recap-sink", frontendSink)}))
		e.waitHeld(t, jobID, validateTimeout, "no_registration")
		expectNoSummary(t, e.getSummary(t, token, rid), "GET summary without registration")
		e.registerLate(t, rid, mid, ada)
		// Matched, not yet delivered or already delivered: read the state first.
		r := e.getSummary(t, token, rid)
		if j := e.job(t, jobID); j.State != "delivered" && j.State != "cleaned" {
			expectNoSummary(t, r, "GET summary before delivery")
		}
		e.waitJob(t, jobID, pipelineTimeout, "delivered", func(j jobView) bool { return j.State == "delivered" })
		expectMarkdown(t, e.getSummary(t, token, rid), "GET summary after the late registration")
	})
	t.Run("frontend-sink-pickup", func(t *testing.T) {
		start := time.Now()
		rid, mid, _ := deliveredVia(t, e, frontendSink)
		expectStillRegistered(t, e, rid)
		first := e.getSummary(t, token, rid)
		expectMarkdown(t, first, "GET summary")
		rememberSummary(first.body)
		if again := e.getSummary(t, token, rid); !bytes.Equal(again.body, first.body) {
			t.Errorf("a second GET summary returns other bytes")
		}

		other := otherFrontendToken(t)
		expectNoDetails(t, expectNoSummary(t, e.getSummary(t, other, rid), "GET summary by another front end"), mid)
		expectNoDetails(t, expectNoSummary(t, e.deleteSummary(t, other, rid), "DELETE summary by another front end"), mid)
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			expectProblem(t, e.do(t, method, summaryPath(rid), "", nil), http.StatusUnauthorized, "unauthorized", method+" summary without credential")
		}
		if r := e.getSummary(t, token, rid); !bytes.Equal(r.body, first.body) {
			t.Fatalf("summary changed or gone after the foreign and unauthorized calls: %s", r)
		}
		expectProblem(t, e.deleteReg(t, token, rid), http.StatusConflict, "already-delivered", "DELETE registration after delivery")

		expectStatus(t, e.deleteSummary(t, token, rid), http.StatusNoContent, "DELETE summary")
		expectNoSummary(t, e.getSummary(t, token, rid), "GET summary after pickup")
		j := expectResult(t, e.getReg(t, token, rid), rid, mid, "GET registration after pickup")
		expectDelivered(t, j, start, "", false, "after pickup")
		expectStatus(t, e.deleteSummary(t, token, rid), http.StatusNoContent, "DELETE summary again")
	})
	t.Run("docs-sink-404", func(t *testing.T) {
		rid, mid, _ := deliveredVia(t, e, "docs-main")
		expectStillRegistered(t, e, rid)
		expectNoSummary(t, e.getSummary(t, token, rid), "GET summary of a Docs delivery")
		expectStatus(t, e.deleteSummary(t, token, rid), http.StatusNoContent, "DELETE summary of a Docs delivery")
		expectResult(t, e.getReg(t, token, rid), rid, mid, "GET registration")
	})
}

// testSummaryLogs: the summary text never appears in the service log (README
// "Logging"; privacy.md §6). The canaries are the lines of the summaries
// fetched above; the bodies are the same in every run, so a log of an
// earlier run is scanned as well.
func testSummaryLogs(t *testing.T) {
	newEnv(t)
	path := os.Getenv(logFileVar)
	if path == "" {
		t.Skipf("skipped: %s not set", logFileVar)
	}
	summaryLines.Lock()
	canaries := append([]string(nil), summaryLines.lines...)
	summaryLines.Unlock()
	if len(canaries) == 0 {
		t.Fatal("no summary was fetched, so there is nothing to scan for (CT-REG-09 frontend-sink-pickup failed)")
	}
	logs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", logFileVar, err)
	}
	for _, c := range canaries {
		if bytes.Contains(logs, []byte(c)) {
			t.Errorf("service log contains a summary line: %q", c)
		}
	}
}

// --- lifetime: cleanup, RECAP_RESULT_TTL, erase (slow) ---

// resultTTL reads RECAP_CONTRACT_RESULT_TTL (Go duration or <n>d) and skips
// when it is unset.
func resultTTL(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv(resultTTLVar)
	if v == "" {
		t.Skipf("skipped: %s not set (the RECAP_RESULT_TTL of the service under test, e.g. 90m; "+
			"these tests wait up to about 2.5 h, run go test with -timeout 4h)", resultTTLVar)
	}
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var days int
		days, err = strconv.Atoi(n)
		d = time.Duration(days) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(v)
	}
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q: want a positive duration", resultTTLVar, v)
	}
	return d
}

// waitCleaned waits until the delivered job is cleaned with outcome delivered.
func (e *env) waitCleaned(t *testing.T, jobID string) {
	t.Helper()
	deadline := time.Now().Add(cleanupWait)
	for {
		j := e.adminJob(t, jobID)
		if j.State == "cleaned" {
			if j.Outcome != "delivered" {
				t.Fatalf("job %s cleaned with outcome %q, want delivered", jobID, j.Outcome)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s not cleaned within %s (state %s)", jobID, cleanupWait, j.State)
		}
		time.Sleep(slowPoll)
	}
}

// waitUntil404 polls get until it answers 404, at most loopWait after expiry.
func waitUntil404(t *testing.T, expiry time.Time, get func() reply, what string) reply {
	t.Helper()
	time.Sleep(time.Until(expiry))
	deadline := time.Now().Add(loopWait)
	for {
		r := get()
		if r.status == http.StatusNotFound {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still %s, %s after the end of %s (the cleanup loop runs hourly)", what, r, time.Since(expiry).Round(time.Second), resultTTLVar)
		}
		time.Sleep(slowPoll)
	}
}

// TestResultLifetime pins the lifetime half of CT-REG-08 and CT-REG-09: the
// result record and the summary survive cleanup, the cleanup loop deletes
// them after RECAP_RESULT_TTL, admin erase deletes the summary. It runs only
// with RECAP_CONTRACT_RESULT_TTL set; the subtests run in parallel.
func TestResultLifetime(t *testing.T) {
	e := newEnv(t)
	ttl := resultTTL(t)
	token := frontendToken(t)
	t.Run("CT-REG-08 kept after cleanup until RECAP_RESULT_TTL", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		rid, mid, jobID := deliveredVia(t, e, "docs-main")
		want := e.documentURL(t, jobID)
		before := expectResult(t, e.getReg(t, token, rid), rid, mid, "GET after delivery")
		at := expectDelivered(t, before, start, want, false, "after delivery")
		e.waitCleaned(t, jobID)
		if time.Now().Before(at.Add(ttl)) {
			j := expectResult(t, e.getReg(t, token, rid), rid, mid, "GET after cleanup")
			if j.JobID != jobID || j.State != "cleaned" {
				t.Fatalf("job %s, want %s cleaned", j, jobID)
			}
			expectDelivered(t, j, start, want, false, "after cleanup")
			if *j.DeliveredAt != *before.DeliveredAt {
				t.Errorf("delivered_at changed at cleanup: %s, was %s", *j.DeliveredAt, *before.DeliveredAt)
			}
		} else {
			t.Errorf("%s=%s ends before cleanup: set it above RECAP_RETENTION_AFTER_DELIVERY (1h)", resultTTLVar, ttl)
		}
		r := waitUntil404(t, at.Add(ttl), func() reply { return e.getReg(t, token, rid) }, "GET registration")
		expectProblem(t, r, http.StatusNotFound, "not-found", "GET registration after RECAP_RESULT_TTL")
	})
	t.Run("CT-REG-09 summary gone after RECAP_RESULT_TTL", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		rid, mid, _ := deliveredVia(t, e, frontendSink)
		at := expectDelivered(t, expectResult(t, e.getReg(t, token, rid), rid, mid, "GET after delivery"), start, "", true, "after delivery")
		expectMarkdown(t, e.getSummary(t, token, rid), "GET summary after delivery")
		r := waitUntil404(t, at.Add(ttl), func() reply { return e.getSummary(t, token, rid) }, "GET summary")
		expectNoSummary(t, r, "GET summary after RECAP_RESULT_TTL")
		expectProblem(t, e.getReg(t, token, rid), http.StatusNotFound, "not-found", "GET registration after RECAP_RESULT_TTL")
	})
	t.Run("CT-REG-09 admin erase deletes the summary", func(t *testing.T) {
		t.Parallel()
		rid, _, jobID := deliveredVia(t, e, frontendSink)
		e.waitCleaned(t, jobID)
		r := e.getSummary(t, token, rid)
		if r.status == http.StatusNotFound {
			t.Fatalf("summary gone at cleanup, before erase: it must stay until pickup or %s (privacy.md §5.1); "+
				"or %s=%s is shorter than RECAP_RETENTION_AFTER_DELIVERY (1h)", resultTTLVar, resultTTLVar, ttl)
		}
		expectMarkdown(t, r, "GET summary after cleanup")
		er := e.do(t, http.MethodPost, "/api/v1/admin/jobs/"+url.PathEscape(jobID)+"/erase", adminToken(t), []byte("{}"))
		expectStatus(t, er, http.StatusOK, "POST erase")
		expectNoSummary(t, e.getSummary(t, token, rid), "GET summary after erase")
		expectProblem(t, e.getReg(t, token, rid), http.StatusNotFound, "not-found", "GET registration after erase (privacy.md §5.4: S9)")
	})
}
