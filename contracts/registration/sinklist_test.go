package registration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Several sinks and the Matrix room in meta (metadata-interface.md §1.2, §3
// rows 5, 6, 6a, 6b; ADR-016): CT-META-07 and CT-META-08; the registration's
// result_url (§2.3, §2.7): CT-REG-10. Written before the implementation: the
// valid lists, rooms and result_url fail until the service has them.
//
// The contract environment has the sink profiles docs-main and
// frontend-contract. CT-META-08 also needs the Matrix profile
// matrix-contract, which the contract environment adds when RECAP_CONTRACT_HOOKS_ADDR is set (contracts/README.md);
// without it CT-META-08 skips. RECAP_FRONTEND_SINKS is not set there, so the
// per-name allow-list (row 9a) and row 6a (two frontend or two matrix
// profiles) are left to unit tests.

const (
	hooksAddrVar       = "RECAP_CONTRACT_HOOKS_ADDR"
	matrixContractSink = "matrix-contract"
)

// TestSinkList pins the list grammar and the Matrix room checks.
func TestSinkList(t *testing.T) {
	t.Run("CT-META-07 sink lists", testSinkLists)
	t.Run("CT-META-08 Matrix room", testMatrixRoom)
}

// TestResultURL pins result_url of the registration (metadata-interface.md
// §2.3, §2.7; CT-REG-10). The schema part needs no service.
func TestResultURL(t *testing.T) {
	t.Run("CT-REG-10 request schema", testResultURLSchema)
	t.Run("CT-REG-10 result_url echoed", testResultURLService)
}

const fixtureResultURL = "https://pilos.recap.test/rooms/abc-def-123/summaries"

func testResultURLSchema(t *testing.T) {
	req := compileSchema(t, "")
	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(testdataDir, "registration", "requests", kind+".*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no %s request fixtures: %v", kind, err)
		}
		for _, f := range files {
			t.Run(filepath.Base(f), func(t *testing.T) {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				err = validate(req, b)
				if kind == "valid" && err != nil {
					t.Fatalf("schema rejects a valid request: %v", err)
				}
				if kind == "invalid" && err == nil {
					t.Fatal("schema accepts an invalid request")
				}
			})
		}
	}
}

// regBodyURL is regBody with schema_version 1.1.0 and result_url.
func regBodyURL(t *testing.T, recordID, meetingID string, o owner, resultURL any) []byte {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(regBody(t, recordID, meetingID, o), &m); err != nil {
		t.Fatal(err)
	}
	m["schema_version"], m["result_url"] = "1.1.0", resultURL
	return mustJSON(t, m)
}

func testResultURLService(t *testing.T) {
	e := newEnv(t)
	token := frontendToken(t)
	readURL := func(t *testing.T, r reply) any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatalf("response is not a JSON object: %v", err)
		}
		v, ok := m["result_url"]
		if !ok {
			t.Fatalf("response has no result_url (schema_version 1.3.0, metadata-interface.md §2.3): %s", r.body)
		}
		return v
	}
	t.Run("set-and-echoed", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		r := e.putReg(t, token, rid, regBodyURL(t, rid, mid, ada, fixtureResultURL))
		expectStatus(t, r, http.StatusCreated, "PUT with result_url")
		if v := readURL(t, r); v != fixtureResultURL {
			t.Errorf("PUT answer result_url %v, want %s", v, fixtureResultURL)
		}
		g := e.getReg(t, token, rid)
		expectStatus(t, g, http.StatusOK, "GET")
		if v := readURL(t, g); v != fixtureResultURL {
			t.Errorf("GET result_url %v, want %s", v, fixtureResultURL)
		}
		if err := validate(compileSchema(t, "#/$defs/registration_response"), g.body); err != nil {
			t.Errorf("response violates the schema: %v", err)
		}
	})
	t.Run("replaced-by-a-later-put", func(t *testing.T) {
		// §2.4: an other result_url replaces it while no job exists.
		rid, mid := freshRecordID(), freshMeetingID()
		expectStatus(t, e.putReg(t, token, rid, regBodyURL(t, rid, mid, ada, fixtureResultURL)), http.StatusCreated, "PUT with result_url")
		other := fixtureResultURL + "-other"
		r := e.putReg(t, token, rid, regBodyURL(t, rid, mid, ada, other))
		expectStatus(t, r, http.StatusOK, "PUT with another result_url")
		if v := readURL(t, r); v != other {
			t.Errorf("result_url %v after a PUT with another one, want %s", v, other)
		}
	})
	t.Run("removed-by-a-put-without-it", func(t *testing.T) {
		// §2.4: a PUT replaces the whole body, so omitting result_url removes it.
		rid, mid := freshRecordID(), freshMeetingID()
		expectStatus(t, e.putReg(t, token, rid, regBodyURL(t, rid, mid, ada, fixtureResultURL)), http.StatusCreated, "PUT with result_url")
		r := e.putReg(t, token, rid, regBody(t, rid, mid, ada))
		expectStatus(t, r, http.StatusOK, "PUT without result_url")
		if v := readURL(t, r); v != nil {
			t.Errorf("result_url %v after a PUT without it, want null", v)
		}
		if v := readURL(t, e.getReg(t, token, rid)); v != nil {
			t.Errorf("GET result_url %v after a PUT without it, want null", v)
		}
	})
	t.Run("null-without-one", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		if v := readURL(t, e.getReg(t, token, rid)); v != nil {
			t.Errorf("result_url %v without one in the request, want null", v)
		}
	})
	for name, v := range map[string]any{
		"http":       "http://pilos.recap.test/rooms/abc",
		"user-info":  "https://user@pilos.recap.test/rooms/abc",
		"whitespace": "https://pilos.recap.test/rooms/a b",
		"too-long":   "https://pilos.recap.test/" + strings.Repeat("a", 2030),
		"null":       nil,
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			rid, mid := freshRecordID(), freshMeetingID()
			expectProblem(t, e.putReg(t, token, rid, regBodyURL(t, rid, mid, ada, v)), http.StatusBadRequest, "invalid-request", "PUT with a bad result_url")
		})
	}
}

// heldAs ingests a package with meta and waits for the held reason; detail is
// checked when not empty.
func (e *env) heldAs(t *testing.T, meta map[string]string, reason, detail string) {
	t.Helper()
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: meta}))
	j := e.waitHeld(t, jobID, validateTimeout, reason)
	if detail != "" && j.Detail != detail {
		t.Errorf("admin job detail %q, want %q", j.Detail, detail)
	}
}

func testSinkLists(t *testing.T) {
	e := newEnv(t)
	sinks := func(v string) map[string]string { return with(optedIn(), "recap-sink", v) }
	// Valid lists pass rows 5, 6 and 6a; without a registration row 7 holds.
	for name, v := range map[string]string{
		"two-names":       "docs-main," + frontendSink,
		"list-order-kept": frontendSink + ",docs-main",
	} {
		t.Run("valid/"+name, func(t *testing.T) { e.heldAs(t, sinks(v), "no_registration", "") })
	}
	for name, v := range map[string]string{
		"trailing-comma":     "docs-main,",
		"leading-comma":      ",docs-main",
		"empty-element":      "docs-main,," + frontendSink,
		"space-after-comma":  "docs-main, " + frontendSink,
		"duplicate":          "docs-main,docs-main",
		"five-names":         "docs-main," + frontendSink + ",sink-c,sink-d,sink-e",
		"bad-name-in-list":   "docs-main,Frontend-contract",
		"semicolon-not-list": "docs-main;" + frontendSink,
	} {
		t.Run("invalid/"+name, func(t *testing.T) { e.heldAs(t, sinks(v), "invalid_metadata", "") })
	}
	t.Run("unknown-name-in-list", func(t *testing.T) {
		e.heldAs(t, sinks("docs-main,no-such-sink"), "unknown_sink", "")
	})
	t.Run("four-names-grammar-ok", func(t *testing.T) {
		// Four valid names pass the grammar; the unconfigured ones stop at row 6.
		e.heldAs(t, sinks("docs-main,"+frontendSink+",sink-c,sink-d"), "unknown_sink", "")
	})
}

func testMatrixRoom(t *testing.T) {
	if os.Getenv(hooksAddrVar) == "" {
		t.Skipf("skipped: %s not set, so the contract environment has no %s profile (contracts/README.md)", hooksAddrVar, matrixContractSink)
	}
	e := newEnv(t)
	list := "docs-main," + matrixContractSink
	room := func(v string) map[string]string {
		return with(with(optedIn(), "recap-sink", list), "recap-matrix-room", v)
	}
	for name, v := range map[string]string{
		"room-id":               "!fixturejoined01:matrix.recap.test",
		"alias":                 "#fixture-team:matrix.recap.test",
		"room-id-base64-opaque": "!AbC_d-E=f+g/h~i.j:matrix.recap.test",
	} {
		t.Run("valid/"+name, func(t *testing.T) { e.heldAs(t, room(v), "no_registration", "") })
	}
	t.Run("missing", func(t *testing.T) {
		e.heldAs(t, with(optedIn(), "recap-sink", list), "invalid_metadata", "matrix-room-invalid")
	})
	for name, v := range map[string]string{
		"no-sigil":             "fixturejoined01:matrix.recap.test",
		"alias-without-server": "#fixture-team",
		"whitespace":           "!fixture joined01:matrix.recap.test",
		"empty-opaque":         "!:matrix.recap.test",
		"256-chars":            "!" + strings.Repeat("a", 255-len(":matrix.recap.test")) + ":matrix.recap.test",
		"user-id":              "@fixture-bot:matrix.recap.test",
		"url":                  "https://matrix.to/#/!fixturejoined01:matrix.recap.test",
	} {
		t.Run("invalid/"+name, func(t *testing.T) { e.heldAs(t, room(v), "invalid_metadata", "matrix-room-invalid") })
	}
	// No server allow-list (sinks.md §4c.1): any server, and room IDs without
	// a server part (room version 12), pass.
	for name, v := range map[string]string{
		"other-server-room-id": "!fixtureother01:other.recap.test",
		"other-server-alias":   "#fixture-team:other.recap.test",
		"room-id-no-server":    "!fixtureRoomV12abcdefghijklmnopqrstuvwxyz0123456789_-",
	} {
		t.Run("valid/"+name, func(t *testing.T) { e.heldAs(t, room(v), "no_registration", "") })
	}
	t.Run("ignored-without-matrix-sink", func(t *testing.T) {
		e.heldAs(t, with(optedIn(), "recap-matrix-room", "not a room"), "no_registration", "")
	})
}
