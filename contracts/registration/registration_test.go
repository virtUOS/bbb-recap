package registration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRegistration pins the registration API (metadata-interface.md §2) and
// its data rules (§5; contract tests in §7).
func TestRegistration(t *testing.T) {
	t.Run("CT-REG-01 schema", testSchema)
	t.Run("CT-REG-02 put matrix", testPutMatrix)
	t.Run("CT-REG-03 delete matrix", testDeleteMatrix)
	t.Run("CT-REG-04 auth", testAuth)
	t.Run("CT-REG-05 problems and no owner", testProblems)
	t.Run("CT-REG-06 late registration", testLateRegistration)
	t.Run("CT-REG-07 logs without owner", testLogs)
}

// recordIDPattern is the record_id grammar of §2.3.
var recordIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$`)

// fixtureRegistration reads testdata/meetings/<id>/registration.json as a map.
func fixtureRegistration(t *testing.T, id string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(testdataDir + "/meetings/" + id + "/registration.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// invalidRequest is an edit of F-01's registration.json that makes it
// invalid, and the answer the service gives to it.
type invalidRequest struct {
	name   string
	edit   func(m map[string]any)
	status int
	slug   string
}

// invalidRequests are the invalid request bodies of CT-REG-01. The HTTP test
// sends them; TestResponseSchema checks them against the request schema.
var invalidRequests = []invalidRequest{
	{"missing-meeting_id", func(m map[string]any) { delete(m, "meeting_id") }, 400, "invalid-request"},
	{"missing-owner", func(m map[string]any) { delete(m, "owner") }, 400, "invalid-request"},
	{"missing-owner-email", func(m map[string]any) { delete(m["owner"].(map[string]any), "email") }, 400, "invalid-request"},
	{"extra-field", func(m map[string]any) { m["meeting_type"] = "meeting-summary" }, 400, "invalid-request"},
	{"extra-owner-field", func(m map[string]any) { m["owner"].(map[string]any)["name"] = "Ada Testperson" }, 400, "invalid-request"},
	{"record_id-bad-char", func(m map[string]any) { m["record_id"] = "bad.record.id" }, 400, "invalid-request"},
	{"record_id-256-chars", func(m map[string]any) { m["record_id"] = strings.Repeat("a", 256) }, 400, "invalid-request"},
	{"record_id-leading-underscore", func(m map[string]any) { m["record_id"] = "_" + freshRecordID() }, 400, "invalid-request"},
	{"record_id-leading-hyphen", func(m map[string]any) { m["record_id"] = "-" + freshRecordID() }, 400, "invalid-request"},
	{"sub-256-chars", func(m map[string]any) { m["owner"].(map[string]any)["sub"] = strings.Repeat("s", 256) }, 400, "invalid-request"},
	{"bad-email", func(m map[string]any) { m["owner"].(map[string]any)["email"] = "not-an-email" }, 400, "invalid-request"},
	{"schema_version-2.0.0", func(m map[string]any) { m["schema_version"] = "2.0.0" }, 422, "unsupported-schema-version"},
}

// §2.3 and schemas/registration.schema.json. The fixture bodies get a fresh
// record_id, so the test can run again; everything else is unchanged.
func testSchema(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	for _, id := range []string{"F-01", "F-04", "F-06"} {
		t.Run("valid/"+id, func(t *testing.T) {
			m := fixtureRegistration(t, id)
			rid := freshRecordID()
			m["record_id"] = rid
			r := e.putReg(t, token, rid, mustJSON(t, m))
			expectRegistration(t, r, http.StatusCreated, rid, m["meeting_id"].(string), "registered", "PUT fixture "+id)
		})
	}
	for _, c := range invalidRequests {
		t.Run("invalid/"+c.name, func(t *testing.T) {
			m := fixtureRegistration(t, "F-01")
			m["record_id"] = freshRecordID()
			c.edit(m)
			rid, _ := m["record_id"].(string)
			if rid == "" {
				rid = freshRecordID()
			}
			expectProblem(t, e.putReg(t, token, rid, mustJSON(t, m)), c.status, c.slug, "PUT invalid registration")
			if recordIDPattern.MatchString(rid) {
				expectProblem(t, e.getReg(t, token, rid), http.StatusNotFound, "not-found", "GET after rejected PUT (nothing stored)")
			}
		})
	}
	t.Run("record_id-255-chars", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		rid += "-" + strings.Repeat("a", 255-len(rid)-1)
		r := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
		expectRegistration(t, r, http.StatusCreated, rid, mid, "registered", "PUT with a 255-character record_id")
	})
	t.Run("sub-255-chars", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		r := e.putReg(t, token, rid, regBody(t, rid, mid, owner{Sub: longSub, Email: ada.Email}))
		expectRegistration(t, r, http.StatusCreated, rid, mid, "registered", "PUT with a 255-character sub")
	})
}

// heldOwnerUnresolved registers an owner Docs cannot resolve and runs a package
// to deliver: the job is held owner_unresolved with deliver pending.
func heldOwnerUnresolved(t *testing.T, e *env) (rid, mid, jobID string) {
	t.Helper()
	rid, mid = freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, unresolvable)
	jobID = e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
	e.waitHeld(t, jobID, pipelineTimeout, "owner_unresolved")
	return rid, mid, jobID
}

// delivered registers Ada and runs a package until the job is delivered.
func delivered(t *testing.T, e *env) (rid, mid, jobID string) {
	t.Helper()
	rid, mid = freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, ada)
	jobID = e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
	e.waitJob(t, jobID, pipelineTimeout, "delivered", func(j jobView) bool { return j.State == "delivered" })
	return rid, mid, jobID
}

// expectStillRegistered fails with a hint when cleanup already removed the
// registration (RECAP_RETENTION_AFTER_DELIVERY = 0; see the package doc).
func expectStillRegistered(t *testing.T, e *env, rid string) {
	t.Helper()
	r := e.getReg(t, frontendToken(t), rid)
	if r.status == http.StatusNotFound {
		t.Fatalf("registration %s is gone after delivery: run the service with RECAP_RETENTION_AFTER_DELIVERY > 0", rid)
	}
	expectStatus(t, r, http.StatusOK, "GET registration after delivery")
}

func notHeld(j jobView) bool { return j.State != "held" && j.State != "failed" }

// §2.4, one subtest per row.
func testPutMatrix(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)

	t.Run("none/valid-201", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		r := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
		expectRegistration(t, r, http.StatusCreated, rid, mid, "registered", "PUT new")
		g := e.getReg(t, token, rid)
		expectRegistration(t, g, http.StatusOK, rid, mid, "registered", "GET new")
	})
	t.Run("identical-200-unchanged", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		body := regBody(t, rid, mid, ada)
		first := e.putReg(t, token, rid, body)
		expectRegistration(t, first, http.StatusCreated, rid, mid, "registered", "PUT new")
		again := e.putReg(t, token, rid, body)
		expectRegistration(t, again, http.StatusOK, rid, mid, "registered", "PUT identical")
		var a, b map[string]any
		_ = json.Unmarshal(first.body, &a)
		_ = json.Unmarshal(again.body, &b)
		if !bytes.Equal(mustJSON(t, a), mustJSON(t, b)) {
			t.Errorf("identical PUT changed the registration:\nfirst %s\nagain %s", first.body, again.body)
		}
	})
	t.Run("other-owner-no-job-200", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		r := e.putReg(t, token, rid, regBody(t, rid, mid, dup))
		expectRegistration(t, r, http.StatusOK, rid, mid, "registered", "PUT other owner")
	})
	t.Run("other-owner-resumes-owner_unresolved-then-locked", func(t *testing.T) {
		rid, mid, jobID := heldOwnerUnresolved(t, e)
		r := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
		expectStatus(t, r, http.StatusOK, "PUT corrected owner while deliver is pending")
		expectNoOwnerData(t, r, "PUT corrected owner")
		// The release happens in the same transaction as the PUT (architecture.md §5.1).
		if j := e.job(t, jobID); !notHeld(j) {
			t.Fatalf("job after owner correction: %s, want resumed", j)
		}
		e.waitJob(t, jobID, pipelineTimeout, "delivered", func(j jobView) bool { return j.State == "delivered" })
		expectStillRegistered(t, e, rid)
		expectProblem(t, e.putReg(t, token, rid, regBody(t, rid, mid, dup)), http.StatusConflict, "registration-locked", "PUT other owner after delivery")
	})
	t.Run("other-meeting_id-409", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		expectProblem(t, e.putReg(t, token, rid, regBody(t, rid, freshMeetingID(), ada)), http.StatusConflict, "immutable-field", "PUT other meeting_id")
	})
	t.Run("cancelled-no-job-200", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusNoContent, "DELETE")
		r := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
		expectRegistration(t, r, http.StatusOK, rid, mid, "registered", "PUT after cancel without job")
	})
	t.Run("cancelled-job-exists-409", func(t *testing.T) {
		rid, mid, _ := heldOwnerUnresolved(t, e)
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusAccepted, "DELETE with a held job")
		expectProblem(t, e.putReg(t, token, rid, regBody(t, rid, mid, ada)), http.StatusConflict, "registration-cancelled", "PUT after cancel with job")
	})
	t.Run("other-frontend-409", func(t *testing.T) {
		other := otherFrontendToken(t)
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		p := expectProblem(t, e.putReg(t, other, rid, regBody(t, rid, mid, ada)), http.StatusConflict, "conflict", "PUT by another front end")
		expectNoDetails(t, p, mid)
	})
	t.Run("path-body-mismatch-400", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		expectProblem(t, e.putReg(t, token, rid, regBody(t, freshRecordID(), mid, ada)), http.StatusBadRequest, "invalid-request", "PUT path ≠ body record_id")
	})
}

// expectNoDetails: answers to a foreign front end reveal nothing (§2.1): no
// meeting_id and no owning front end. The record_id is the caller's own input,
// so it is not checked, and an RFC 9457 instance echoing the path is ignored.
func expectNoDetails(t *testing.T, p map[string]any, meetingID string) {
	t.Helper()
	c := make(map[string]any, len(p))
	for k, v := range p {
		if k != "instance" {
			c[k] = v
		}
	}
	s := string(mustJSON(t, c))
	for _, v := range []string{meetingID, frontendID} {
		if strings.Contains(s, v) {
			t.Errorf("problem for a foreign front end contains %q: %s", v, s)
		}
	}
}

// §2.5, one subtest per row, plus the tombstone discarding a later package.
func testDeleteMatrix(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	t.Run("registered-204-then-already-cancelled-204", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusNoContent, "DELETE registered")
		expectRegistration(t, e.getReg(t, token, rid), http.StatusOK, rid, mid, "cancelled", "GET cancelled")
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusNoContent, "DELETE already cancelled")
	})
	t.Run("tombstone-discards-later-package", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusNoContent, "DELETE registered")
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
		e.waitJob(t, jobID, validateTimeout, "cleaned/cancelled", func(j jobView) bool {
			return j.State == "cleaned" && j.Outcome == "cancelled"
		})
		expectRegistration(t, e.getReg(t, token, rid), http.StatusOK, rid, mid, "cancelled", "GET after discarded package")
	})
	t.Run("job-before-deliver-202", func(t *testing.T) {
		rid, mid, jobID := heldOwnerUnresolved(t, e)
		expectStatus(t, e.deleteReg(t, token, rid), http.StatusAccepted, "DELETE with a held job")
		e.waitJob(t, jobID, validateTimeout, "cleaned/cancelled", func(j jobView) bool {
			return j.State == "cleaned" && j.Outcome == "cancelled"
		})
		expectRegistration(t, e.getReg(t, token, rid), http.StatusOK, rid, mid, "cancelled", "GET after cancel with job")
	})
	t.Run("delivered-409", func(t *testing.T) {
		rid, _, _ := delivered(t, e)
		expectStillRegistered(t, e, rid)
		expectProblem(t, e.deleteReg(t, token, rid), http.StatusConflict, "already-delivered", "DELETE after delivery")
	})
	t.Run("not-found-404", func(t *testing.T) {
		expectProblem(t, e.deleteReg(t, token, freshRecordID()), http.StatusNotFound, "not-found", "DELETE unknown")
	})
}

// §2.1, §2.6: credentials and foreign registrations.
func testAuth(t *testing.T) {
	e := newEnv(t)
	rid, mid := freshRecordID(), freshMeetingID()
	body := regBody(t, rid, mid, ada)
	for _, tok := range []struct{ name, token string }{
		{"none", ""},
		{"unknown", "FIXTURE-NOT-A-SECRET-fe-unknown"},
	} {
		t.Run("credential-"+tok.name, func(t *testing.T) {
			expectProblem(t, e.putReg(t, tok.token, rid, body), http.StatusUnauthorized, "unauthorized", "PUT")
			expectProblem(t, e.getReg(t, tok.token, rid), http.StatusUnauthorized, "unauthorized", "GET")
			expectProblem(t, e.deleteReg(t, tok.token, rid), http.StatusUnauthorized, "unauthorized", "DELETE")
			expectProblem(t, e.getReg(t, frontendToken(t), rid), http.StatusNotFound, "not-found", "GET after unauthorized PUT (nothing stored)")
		})
	}
	t.Run("foreign-registration", func(t *testing.T) {
		other := otherFrontendToken(t)
		e.register(t, rid, mid, ada)
		expectNoDetails(t, expectProblem(t, e.getReg(t, other, rid), http.StatusNotFound, "not-found", "GET foreign"), mid)
		expectNoDetails(t, expectProblem(t, e.deleteReg(t, other, rid), http.StatusNotFound, "not-found", "DELETE foreign"), mid)
		expectNoDetails(t, expectProblem(t, e.putReg(t, other, rid, body), http.StatusConflict, "conflict", "PUT foreign"), mid)
		expectRegistration(t, e.getReg(t, frontendToken(t), rid), http.StatusOK, rid, mid, "registered", "GET own after foreign calls")
	})
}

// §2.6: every error is an RFC 9457 problem with urn:recap:problem:<slug> and
// no personal data; no response carries the owner.
func testProblems(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	rid, mid := freshRecordID(), freshMeetingID()
	body := regBody(t, rid, mid, ada)

	expectProblem(t, e.putReg(t, token, rid, []byte("not json")), http.StatusBadRequest, "invalid-request", "PUT not JSON")
	expectProblem(t, e.putReg(t, "", rid, body), http.StatusUnauthorized, "unauthorized", "PUT without credential")
	expectProblem(t, e.getReg(t, token, rid), http.StatusNotFound, "not-found", "GET unknown")
	// Valid JSON padded past the 8 KiB body limit: only the size is wrong.
	big := append(append([]byte{}, body...), bytes.Repeat([]byte(" "), 8*1024)...)
	expectProblem(t, e.putReg(t, token, rid, big), http.StatusRequestEntityTooLarge, "payload-too-large", "PUT 8 KiB + body")
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["schema_version"] = "2.0.0"
	expectProblem(t, e.putReg(t, token, rid, mustJSON(t, m)), http.StatusUnprocessableEntity, "unsupported-schema-version", "PUT schema_version 2.0.0")

	// Success responses: PUT 201, PUT 200, GET; none carries owner data
	// (expectRegistration checks it).
	expectRegistration(t, e.putReg(t, token, rid, body), http.StatusCreated, rid, mid, "registered", "PUT new")
	expectRegistration(t, e.putReg(t, token, rid, body), http.StatusOK, rid, mid, "registered", "PUT identical")
	expectRegistration(t, e.getReg(t, token, rid), http.StatusOK, rid, mid, "registered", "GET")
	expectProblem(t, e.putReg(t, token, rid, regBody(t, rid, freshMeetingID(), ada)), http.StatusConflict, "immutable-field", "PUT other meeting_id")
	expectStatus(t, e.deleteReg(t, token, rid), http.StatusNoContent, "DELETE")
	expectRegistration(t, e.getReg(t, token, rid), http.StatusOK, rid, mid, "cancelled", "GET cancelled")
}

// §3 row 7, README "States": a package without registration is held
// no_registration; the PUT releases it in the same transaction
// (architecture.md §5.1) and the job is matched.
func testLateRegistration(t *testing.T) {
	e := newEnv(t)
	rid, mid := freshRecordID(), freshMeetingID()
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
	e.waitHeld(t, jobID, validateTimeout, "no_registration")
	e.registerLate(t, rid, mid, ada)
	if j := e.job(t, jobID); !notHeld(j) {
		t.Fatalf("job right after the registration PUT: %s, want resumed", j)
	}
	e.waitMatched(t, rid, mid, jobID)
}

// logFileVar names the log of the binary under test: JSON lines, all levels
// including DEBUG (contracts/README.md).
const logFileVar = "RECAP_CONTRACT_LOG_FILE"

// §5 and README "Logging": IDs may be logged, owner.sub and owner.email never.
// The test drives the registration paths that touch owner data, then scans the
// whole log, which also covers every other test of this package.
func testLogs(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	rid, mid := freshRecordID(), freshMeetingID()
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
	e.waitHeld(t, jobID, validateTimeout, "no_registration")
	e.registerLate(t, rid, mid, ada)
	again := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
	expectStatus(t, again, http.StatusOK, "PUT identical")
	expectNoOwnerData(t, again, "PUT identical")
	bad := regBody(t, rid, mid, owner{Sub: ada.Sub, Email: "not-an-email"})
	expectProblem(t, e.putReg(t, token, rid, bad), http.StatusBadRequest, "invalid-request", "PUT bad email")
	e.waitMatched(t, rid, mid, jobID)

	path := os.Getenv(logFileVar)
	if path == "" {
		t.Skipf("skipped: %s not set; requests ran, only the log scan is skipped", logFileVar)
	}
	logs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", logFileVar, err)
	}
	low := strings.ToLower(string(logs))
	for _, v := range allOwnerValues {
		if strings.Contains(low, strings.ToLower(v)) {
			t.Errorf("service log contains owner value %q", v)
		}
	}
}
