package registration

import (
	"net/http"
	"strings"
	"testing"
)

// TestJoin pins the join of package and registration (metadata-interface.md
// §3 rows 7–9, §4).
func TestJoin(t *testing.T) {
	t.Run("CT-JOIN-01 record_id join", testJoin)
}

func testJoin(t *testing.T) {
	e := newEnv(t)
	t.Run("same-record_id-matches", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
		e.waitMatched(t, rid, mid, jobID)
	})
	t.Run("exact-record_id-only", func(t *testing.T) {
		// A registration whose record_id differs only in case is not a match.
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, strings.ToUpper(rid), mid, ada)
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn()}))
		e.waitHeld(t, jobID, validateTimeout, "no_registration")
		expectRegistration(t, e.getReg(t, frontendToken(t), strings.ToUpper(rid)), http.StatusOK, strings.ToUpper(rid), mid, "registered", "GET case-variant registration")
	})
	t.Run("server-not-allowed", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		jobID := e.ingest(t, buildPackage(t, pkgOpts{server: serverB, recordID: rid, meetingID: mid, meta: optedIn()}))
		j := e.waitHeld(t, jobID, validateTimeout, "no_registration")
		if j.Detail != "server-not-allowed" {
			t.Errorf("admin job detail %q, want server-not-allowed", j.Detail)
		}
		expectRegistration(t, e.getReg(t, frontendToken(t), rid), http.StatusOK, rid, mid, "registered", "GET registration not claimed")
	})
	t.Run("meeting_id-mismatch", func(t *testing.T) {
		rid, mid := freshRecordID(), freshMeetingID()
		e.register(t, rid, mid, ada)
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: freshMeetingID(), meta: optedIn()}))
		j := e.waitHeld(t, jobID, validateTimeout, "no_registration")
		if j.Detail != "meeting-id-mismatch" {
			t.Errorf("admin job detail %q, want meeting-id-mismatch", j.Detail)
		}
		expectRegistration(t, e.getReg(t, frontendToken(t), rid), http.StatusOK, rid, mid, "registered", "GET registration not claimed")
	})
}
