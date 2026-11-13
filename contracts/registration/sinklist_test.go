package registration

import (
	"os"
	"strings"
	"testing"
)

// Several sinks and the Matrix room in meta (metadata-interface.md §1.2, §3
// rows 5, 6, 6a, 6b; ADR-016): CT-META-07 and CT-META-08. Written before the
// implementation: the valid lists and rooms fail until validate reads lists.
//
// The contract environment has the sink profiles docs-main and
// frontend-contract. CT-META-08 also needs the Matrix profile
// matrix-contract (allowed_servers [matrix.recap.test]), which the contract
// environment adds when RECAP_CONTRACT_HOOKS_ADDR is set (contracts/README.md);
// without it CT-META-08 skips. RECAP_FRONTEND_SINKS is not set there, so the
// per-name allow-list (row 9a) and row 6a (two frontend profiles) are left to
// unit tests.

const (
	hooksAddrVar       = "RECAP_CONTRACT_HOOKS_ADDR"
	matrixContractSink = "matrix-contract"
)

// TestSinkList pins the list grammar and the Matrix room checks.
func TestSinkList(t *testing.T) {
	t.Run("CT-META-07 sink lists", testSinkLists)
	t.Run("CT-META-08 Matrix room", testMatrixRoom)
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
		"room-id-with-port":     "!fixturejoined01:matrix.recap.test:8448",
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
	for name, v := range map[string]string{
		"other-server-room-id": "!fixtureother01:other.recap.test",
		"other-server-alias":   "#fixture-team:other.recap.test",
		"room-id-no-server":    "!fixtureRoomV12abcdefghijklmnopqrstuvwxyz0123456789_-",
	} {
		t.Run("not-allowed/"+name, func(t *testing.T) { e.heldAs(t, room(v), "invalid_metadata", "matrix-room-not-allowed") })
	}
	t.Run("ignored-without-matrix-sink", func(t *testing.T) {
		e.heldAs(t, with(optedIn(), "recap-matrix-room", "not a room"), "no_registration", "")
	})
}
