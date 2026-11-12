package sink

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// TestSink holds the sink tests of sinks.md §4. All but the admin-action
// test drive `recap tool sink` (RECAP_BIN) against a mock Docs started by the
// test; that one drives the running service (service_test.go).
func TestSink(t *testing.T) {
	t.Run("CT-SINK-01 known owner by sub", testKnownOwner)
	t.Run("CT-SINK-02 owner by email or invited", testEmailAndInvite)
	t.Run("CT-SINK-03 not sent then sent", testNotSent)
	t.Run("CT-SINK-04 uncertain outcomes", testUncertain)
	t.Run("CT-SINK-05 re-entry in sending", testReentry)
	t.Run("CT-SINK-06 admin release and retry", testAdminActions)
	t.Run("CT-SINK-07 config and rejected", testConfigRejected)
	t.Run("CT-SINK-08 duplicate email", testDuplicateEmail)
	t.Run("CT-SINK-09 rejected before any call", testPreChecks)
	t.Run("CT-SINK-10 no content in logs", testSinkLogs)
	t.Run("CT-SINK-11 golden summary through the dialect", testGoldenDialect)
	t.Run("CT-SINK-12 frontend sink", testFrontendSink)
}

// setup starts a mock Docs over TLS and a tool environment pointing at it.
func setup(t *testing.T) (*mockDocs, sinkEnv) {
	t.Helper()
	m := newMockDocs(t)
	srv := m.startTLS(t)
	return m, newSinkEnv(t, srv, srv.URL)
}

// deliverOnce runs one deliver call without an existing delivery record.
func deliverOnce(t *testing.T, e sinkEnv, req deliveryRequest) output {
	t.Helper()
	return e.run(t, toolInput{Request: req})
}

func testKnownOwner(t *testing.T) {
	m, e := setup(t)
	req := baseRequest(t)
	o := deliverOnce(t, e, req)
	r := expectRequests(t, m, 1)
	expectCreateRequest(t, r[0], m.key, req)
	doc := expectOneDocument(t, m)
	if doc.ownerSub != ada.Sub {
		t.Errorf("document owner %q, want an owner access for %s", doc.ownerSub, ada.Sub)
	}
	expectSent(t, o, req, doc.id)
}

func testEmailAndInvite(t *testing.T) {
	harness.Bin(t) // skip here, not in every case
	var records []record
	for _, c := range []struct {
		name  string
		owner owner
		check func(document) bool
	}{
		{"email-match", adaByEmail, func(d document) bool { return d.ownerSub == ada.Sub }},
		{"unknown-invited", nobody, func(d document) bool { return d.invitedEmail == nobody.Email && d.ownerSub == "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, e := setup(t)
			req := baseRequest(t).withOwner(c.owner)
			o := deliverOnce(t, e, req)
			expectCreateRequest(t, expectRequests(t, m, 1)[0], m.key, req)
			doc := expectOneDocument(t, m)
			if !c.check(doc) {
				t.Errorf("mock document %+v is not the expected owner access / invitation", doc)
			}
			expectSent(t, o, req, doc.id)
			records = append(records, *o.Delivery)
		})
	}
	// The 201 does not say which case happened (sinks.md §3.3): same record.
	if len(records) == 2 {
		a, b := records[0], records[1]
		a.DocumentID, a.SentAt, b.DocumentID, b.SentAt = nil, nil, nil, nil
		if a != b {
			t.Errorf("records differ beyond document_id and sent_at: %+v vs %+v", a, b)
		}
	}
}

func testNotSent(t *testing.T) {
	m, e := setup(t)
	req := baseRequest(t)
	refused := e.withValue(t, "docs_base_url", closedURL(t))

	o := deliverOnce(t, refused, req)
	expectClass(t, o, "not_sent")
	expectRequests(t, m, 0)

	m.queue(statusFault(http.StatusTooManyRequests, bodyThrottled))
	o = deliverOnce(t, e, req)
	expectClass(t, o, "not_sent")
	expectRequests(t, m, 1)

	o = deliverOnce(t, e, req)
	expectRequests(t, m, 2)
	doc := expectOneDocument(t, m)
	expectSent(t, o, req, doc.id)
}

func testUncertain(t *testing.T) {
	harness.Bin(t) // skip here, not in every case
	for _, c := range []struct {
		name string
		f    fault
	}{
		{"timeout-after-create", fault{hangAfterCreate: true}},
		{"http-500", statusFault(http.StatusInternalServerError, bodyServerError)},
		{"http-503", statusFault(http.StatusServiceUnavailable, bodyServerError)},
		{"connection-lost-after-create", fault{dropAfterCreate: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, e := setup(t)
			m.queue(c.f)
			o := e.run(t, toolInput{Request: baseRequest(t), RequestTimeoutMS: ms(1000)})
			expectClass(t, o, "uncertain")
			expectRequests(t, m, 1) // no second POST
		})
	}
}

func testReentry(t *testing.T) {
	m, e := setup(t)
	req := baseRequest(t)
	sending := &record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: "sending"}
	o := e.run(t, toolInput{Request: req, Delivery: sending})
	expectClass(t, o, "uncertain")
	expectRequests(t, m, 0)
}

func testConfigRejected(t *testing.T) {
	harness.Bin(t) // skip here, not in every case
	for _, c := range []struct {
		name, class string
		f           *fault
		wrongKey    bool
	}{
		{name: "401-wrong-key", class: "config", wrongKey: true},
		{name: "403", class: "config", f: &fault{status: http.StatusForbidden, body: bodyForbidden}},
		{name: "400-content", class: "rejected", f: &fault{status: http.StatusBadRequest, body: bodyBadContent}},
		{name: "404", class: "rejected", f: &fault{status: http.StatusNotFound, body: bodyNotFound}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, e := setup(t)
			if c.f != nil {
				m.queue(*c.f)
			}
			if c.wrongKey {
				p := filepath.Join(t.TempDir(), "docs-s2s.key")
				writeFile(t, p, []byte("FIXTURE-NOT-A-SECRET-docs-s2s-wrong\n"))
				e = e.withValue(t, "api_key_file", p)
			}
			o := deliverOnce(t, e, baseRequest(t))
			expectClass(t, o, c.class)
			expectRequests(t, m, 1) // no retry inside the call
			if len(m.documents()) != 0 {
				t.Errorf("mock holds a document after %s", c.name)
			}
		})
	}
}

// testDuplicateEmail: the adapter part. The service part (the registration
// PUT resumes the held job and it is delivered) is testOwnerResumed in
// identity_test.go.
func testDuplicateEmail(t *testing.T) {
	m, e := setup(t)
	o := deliverOnce(t, e, baseRequest(t).withOwner(unresolvable))
	expectClass(t, o, "recipient_unresolved")
	expectRequests(t, m, 1)
	if len(m.documents()) != 0 {
		t.Fatal("mock holds a document after the duplicate-email 400")
	}
}

func testPreChecks(t *testing.T) {
	m, e := setup(t)
	summary := baseRequest(t).SummaryMD
	if errs := dialectErrors(summary); len(errs) != 0 {
		t.Fatalf("testdata/sink/summary.md fails the test's dialect check: %v", errs)
	}
	reject := func(t *testing.T, req deliveryRequest) {
		t.Helper()
		before := len(m.received())
		o := deliverOnce(t, e, req)
		expectRejectedWithoutCall(t, o)
		if n := len(m.received()); n != before {
			t.Fatalf("mock Docs received %d requests; a pre-check violation is rejected before any call", n-before)
		}
	}
	t.Run("oversized", func(t *testing.T) {
		// max_document_bytes of the fixture profile is 2097152 (2 MiB).
		big := summary + strings.Repeat("Filler paragraph about the widget.\n\n", 2097152/36+1)
		reject(t, baseRequest(t).withContent(big[:2097153]))
	})
	files, err := filepath.Glob(testdataPath(t, "sink", "forbidden", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no files in testdata/sink/forbidden: %v", err)
	}
	for _, f := range files {
		t.Run("forbidden-"+strings.TrimSuffix(filepath.Base(f), ".md"), func(t *testing.T) {
			md := summary + "\n" + string(readFile(t, f))
			if len(dialectErrors(md)) == 0 {
				t.Fatalf("test fixture %s passes the test's own dialect check", f)
			}
			reject(t, baseRequest(t).withContent(md))
		})
	}
	t.Run("title-201-characters", func(t *testing.T) {
		req := baseRequest(t)
		req.Title = strings.Repeat("ü", 190) + " 2025-10-29" // 201 characters, 391 bytes
		reject(t, req)
	})
	// Controls: what §2.1 allows, and a title of exactly 200 characters, are sent.
	t.Run("allowed-constructs-sent", func(t *testing.T) {
		md := string(readFile(t, testdataPath(t, "sink", "allowed.md")))
		if errs := dialectErrors(md); len(errs) != 0 {
			t.Fatalf("testdata/sink/allowed.md fails the test's dialect check: %v", errs)
		}
		req := baseRequest(t).withContent(md)
		req.Title = strings.Repeat("ü", 189) + " 2025-10-29" // 200 characters
		before := len(m.received())
		o := deliverOnce(t, e, req)
		r := m.received()
		if len(r) != before+1 {
			t.Fatalf("mock Docs received %d requests, want 1", len(r)-before)
		}
		expectCreateRequest(t, r[before], m.key, req)
		docs := m.documents()
		expectSent(t, o, req, docs[len(docs)-1].id)
	})
}

// testSinkLogs runs every scenario of this file once more and scans the
// tool's stderr, at DEBUG, for content, title, emails, subs, the key and
// response bodies (sinks.md §4; privacy.md §6). It also scans the stderr of
// all earlier tool runs of this package and, when set, the service log.
func testSinkLogs(t *testing.T) {
	m, e := setup(t)
	req := baseRequest(t)
	m.queue(statusFault(http.StatusTooManyRequests, bodyThrottled))
	expectClass(t, deliverOnce(t, e, req), "not_sent")
	expectSent(t, deliverOnce(t, e, req), req, m.documents()[0].id)
	deliverOnce(t, e, req.withOwner(nobody))
	expectClass(t, deliverOnce(t, e, req.withOwner(unresolvable)), "recipient_unresolved")
	for _, f := range []fault{
		statusFault(http.StatusBadRequest, bodyBadContent),
		statusFault(http.StatusForbidden, bodyForbidden),
		statusFault(http.StatusInternalServerError, bodyServerError),
		{dropAfterCreate: true},
		{hangAfterCreate: true},
	} {
		m.queue(f)
		o := e.run(t, toolInput{Request: req, RequestTimeoutMS: ms(1000)})
		if o.Error == nil {
			t.Fatalf("fault %+v: got %s, want an error", f, o.raw)
		}
	}
	expectClass(t, e.run(t, toolInput{Request: req, Delivery: &record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: "sending"}}), "uncertain")
	expectRejectedWithoutCall(t, deliverOnce(t, e, req.withContent(req.SummaryMD+"\n<div>x</div>\n")))

	canaries := append(append(append([]string{fixtureKey(t), req.Title}, contentCanaries...), identityCanaries...), responseCanaries...)
	stderrLog.Lock()
	logs := append([]byte(nil), stderrLog.buf.Bytes()...)
	stderrLog.Unlock()
	expectLogLines(t, "tool stderr", logs)
	expectNoCanary(t, "tool stderr", logs, canaries)
	scanServiceLog(t, canaries)
}

// testGoldenDialect: every rendered summary.md golden of the render contract
// (fixtures.md §7: testdata/render/) passes the test's §2
// check and is sent to Docs byte for byte.
func testGoldenDialect(t *testing.T) {
	m, e := setup(t)
	var goldens []string
	root := testdataPath(t, "render")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "summary") && strings.HasSuffix(d.Name(), ".md") {
			goldens = append(goldens, p)
		}
		return nil
	})
	if len(goldens) == 0 {
		t.Fatalf("no summary*.md goldens under %s (render contract)", root)
	}
	for _, g := range goldens {
		rel, _ := filepath.Rel(root, g)
		t.Run(rel, func(t *testing.T) {
			md, err := os.ReadFile(g)
			if err != nil {
				t.Fatal(err)
			}
			if errs := dialectErrors(string(md)); len(errs) != 0 {
				t.Fatalf("golden fails the sinks.md §2 check: %v", errs)
			}
			req := baseRequest(t).withContent(string(md))
			before := len(m.received())
			o := deliverOnce(t, e, req)
			r := m.received()
			if len(r) != before+1 {
				t.Fatalf("mock Docs received %d requests, want 1", len(r)-before)
			}
			expectCreateRequest(t, r[before], m.key, req)
			docs := m.documents()
			expectSent(t, o, req, docs[len(docs)-1].id)
		})
	}
	// The second half of the sinks.md §4 golden test, a pinned y-provider /convert giving the
	// stored BlockNote JSON, needs a y-provider at the pinned Docs version
	// (6c730f7); it is a manual step (contracts/render/README.md; templates.md §8)
	// until one is pinned for CI.
	t.Log("y-provider /convert comparison not run: no pinned y-provider (contracts/render/README.md)")
}
