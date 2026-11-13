package sink

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// TestIdentity holds the tests of identity-mapping.md §9. The first drives
// `recap tool sink`, the second and third drive the running service, the
// fourth scans the logs of both. The subtests run in this order.
func TestIdentity(t *testing.T) {
	t.Run("CT-ID-01 sub and email unchanged", testOwnerUnchanged)
	t.Run("CT-ID-02 duplicate email held then resumed", testOwnerResumed)
	t.Run("CT-ID-03 no owner data after cleanup", testOwnerCleanup)
	t.Run("CT-ID-04 no identity in logs", testIdentityLogs)
}

// longSub is the longest sub Docs accepts (metadata-interface.md §2.3).
var longSub = "fixture-sub-" + strings.Repeat("s", 255-len("fixture-sub-"))

// serviceRuns records which service-level identity tests completed, so the
// log test knows the service log holds their runs.
var serviceRuns = map[string]bool{}

func testOwnerUnchanged(t *testing.T) {
	harness.Bin(t) // skip here, not in every case
	for _, c := range []struct {
		name  string
		owner owner
	}{
		{"known-owner", ada},
		{"mixed-case-email", owner{Sub: ada.Sub, Email: "Ada.Testperson@Example.ORG"}},
		{"255-character-sub", owner{Sub: longSub, Email: nobody.Email}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, e := setup(t)
			req := baseRequest(t).withOwner(c.owner)
			o := deliverOnce(t, e, req)
			expectCreateRequest(t, expectRequests(t, m, 1)[0], m.key, req)
			expectSent(t, o, req, expectOneDocument(t, m).id)
		})
	}
}

func testOwnerResumed(t *testing.T) {
	s := newService(t)
	rid, mid, jobID := s.startJob(t, unresolvable)
	s.waitHeld(t, jobID, "owner_unresolved")
	expectDelivery(t, s.job(t, jobID), "", "", "job held after the 400 (sinks.md §1.4 item 3)")
	first := expectRequests(t, s.docs, 1)
	expectOwnerFields(t, first[0], unresolvable)
	if len(s.docs.documents()) != 0 {
		t.Fatal("mock Docs holds a document after the duplicate-email 400")
	}
	expectStatus(t, s.putRegistration(t, rid, mid, ada), http.StatusOK, "PUT corrected owner")
	// The release happens in the same transaction as the PUT (architecture.md §5.1).
	if j := s.job(t, jobID); j.State == "held" || j.State == "failed" {
		t.Fatalf("job after the owner correction: %s, want resumed", j)
	}
	s.waitDelivered(t, jobID)
	r := expectRequests(t, s.docs, 2)
	expectOwnerFields(t, r[1], ada)
	if doc := expectOneDocument(t, s.docs); doc.ownerSub != ada.Sub {
		t.Errorf("document owner %q, want %s", doc.ownerSub, ada.Sub)
	}
	serviceRuns["resumed"] = true
}

// expectOwnerFields checks that a Docs request carries exactly o's sub and email.
func expectOwnerFields(t *testing.T, r docsRequest, o owner) {
	t.Helper()
	var body struct {
		Sub   *string `json:"sub"`
		Email *string `json:"email"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("Docs request body: %v", err)
	}
	if str(body.Sub) != o.Sub || str(body.Email) != o.Email {
		t.Fatalf("Docs request owner fields differ from the registration")
	}
}

// testOwnerCleanup: a held job is purged (cleanup runs, architecture.md
// §5.4); afterwards the registration is gone (privacy.md §5.1 step 3) and no
// admin answer carries owner data. Responses are scanned for every fixture
// sub and email by service.do.
func testOwnerCleanup(t *testing.T) {
	s := newService(t)
	rid, _, jobID := s.startJob(t, unresolvable)
	s.waitHeld(t, jobID, "owner_unresolved")
	expectDelivery(t, s.job(t, jobID), "", "", "job held after the 400 (sinks.md §1.4 item 3)")
	s.action(t, jobID, "purge", "{}")
	s.waitJob(t, jobID, "cleaned/purged", func(j jobView) bool { return j.State == "cleaned" && j.Outcome == "purged" })
	expectStatus(t, s.getRegistration(t, rid), http.StatusNotFound, "GET registration after cleanup")
	admin := credential(t, "admin."+adminName)
	for _, path := range []string{"/api/v1/admin/jobs/" + jobID, "/api/v1/admin/jobs?state=cleaned", "/api/v1/admin/audit?job_id=" + jobID} {
		r := s.do(t, http.MethodGet, path, admin, "", nil)
		expectStatus(t, r, http.StatusOK, "GET "+path)
		if strings.Contains(string(r.body), `"owner"`) || strings.Contains(string(r.body), `"email"`) || strings.Contains(string(r.body), `"sub"`) {
			t.Errorf("GET %s: answer has an owner, email or sub field", path)
		}
	}
	expectRequests(t, s.docs, 1) // purge never sends
	serviceRuns["cleanup"] = true
}

// testIdentityLogs scans the stderr of tool runs of the owner scenarios and the
// service log after the resume and cleanup tests for fixture subs, emails and the
// F-01 externalUserIds (as JSON strings; they are too short to scan as bare
// text).
func testIdentityLogs(t *testing.T) {
	bin, base := os.Getenv(harness.EnvBin), os.Getenv(harness.EnvBaseURL)
	if base != "" && os.Getenv(docsAddrVar) == "" {
		// The resume and cleanup tests skip without it, so the service log
		// holds no run to scan.
		t.Logf("service log scan skipped: %s not set", docsAddrVar)
		base = ""
	}
	if bin == "" && base == "" {
		t.Skipf("skipped: %s not set, and %s not set together with %s", harness.EnvBin, harness.EnvBaseURL, docsAddrVar)
	}
	canaries := append(append([]string{}, identityCanaries...), longSub, "Ada.Testperson@Example.ORG", `"u1"`, `"u2"`)
	if bin != "" {
		m, e := setup(t)
		mark := stderrLen()
		deliverOnce(t, e, baseRequest(t))
		deliverOnce(t, e, baseRequest(t).withOwner(owner{Sub: longSub, Email: nobody.Email}))
		expectClass(t, deliverOnce(t, e, baseRequest(t).withOwner(unresolvable)), "recipient_unresolved")
		expectRequests(t, m, 3)
		logs := stderrSince(mark)
		expectLogLines(t, "tool stderr", logs)
		expectNoCanary(t, "tool stderr", logs, canaries)
	}
	if base != "" {
		if !serviceRuns["resumed"] || !serviceRuns["cleanup"] {
			t.Fatal("the resume or cleanup test did not complete, so the service log holds no run to scan")
		}
		scanServiceLog(t, canaries)
	}
}
