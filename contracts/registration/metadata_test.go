package registration

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestMetadata pins the meta_recap-* keys and the validate checks
// (metadata-interface.md §1, §3; contract tests in §7).
func TestMetadata(t *testing.T) {
	t.Run("CT-META-01 key names", testKeyNames)
	t.Run("CT-META-02 name grammar", testNameGrammar)
	t.Run("CT-META-03 opt-in literal", testOptIn)
	t.Run("CT-META-04 validate order", testValidateOrder)
	t.Run("CT-META-05 unknown key ignored", testUnknownKey)
	t.Run("CT-META-06 manifest copy differs", testManifestMismatch)
}

// §1.1, §1.2: BBB accepts meta_[a-zA-Z][a-zA-Z0-9-]* and lowercases keys. A
// package that carries exactly the four lowercase keys passes validate.
func testKeyNames(t *testing.T) {
	e := newEnv(t)
	bbbParam := regexp.MustCompile(`^meta_[a-zA-Z][a-zA-Z0-9-]*$`)
	keys := []string{"recap-v", "recap-enabled", "recap-type", "recap-sink"}
	for _, k := range keys {
		if !bbbParam.MatchString("meta_"+k) || k != strings.ToLower(k) {
			t.Errorf("key %q is not a lowercase valid BBB meta name", k)
		}
	}
	meta := optedIn()
	if len(meta) != len(keys) {
		t.Fatalf("opted-in fixture metadata has %d recap keys, want %d", len(meta), len(keys))
	}
	for _, k := range keys {
		if _, ok := meta[k]; !ok {
			t.Fatalf("opted-in fixture metadata lacks %s", k)
		}
	}
	rid, mid := freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, ada)
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: meta}))
	e.waitMatched(t, rid, mid, jobID)
}

// §1.2 and README "Grammars": names are ^[a-z][a-z0-9-]{0,62}$. A bad value
// is held invalid_metadata; a valid but unconfigured one gets past the grammar
// check to unknown_type / unknown_sink. Control characters are already
// refused by the manifest schema (metadata values), so ingest answers 422.
func testNameGrammar(t *testing.T) {
	e := newEnv(t)
	bad := map[string]string{
		"empty":         "",
		"64-chars":      "a" + strings.Repeat("b", 63),
		"uppercase":     "Meeting-summary",
		"underscore":    "meeting_summary",
		"dot":           "meeting.summary",
		"leading-digit": "1meeting-summary",
		"whitespace":    "meeting summary",
	}
	valid63 := "a" + strings.Repeat("b", 62)
	for _, key := range []string{"recap-type", "recap-sink"} {
		unknown := map[string]string{"recap-type": "unknown_type", "recap-sink": "unknown_sink"}[key]
		for name, v := range bad {
			t.Run(key+"/"+name, func(t *testing.T) {
				jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: with(optedIn(), key, v)}))
				e.waitHeld(t, jobID, validateTimeout, "invalid_metadata")
			})
		}
		t.Run(key+"/control-char", func(t *testing.T) {
			p := buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: with(optedIn(), key, "meeting\tsummary")})
			expectProblem(t, e.putManifest(t, p, serverToken(t, serverA)), http.StatusUnprocessableEntity, "manifest-invalid", "PUT manifest with a control character")
		})
		t.Run(key+"/valid-63-chars", func(t *testing.T) {
			jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: with(optedIn(), key, valid63)}))
			e.waitHeld(t, jobID, validateTimeout, unknown)
		})
	}
	t.Run("valid-configured", func(t *testing.T) {
		// Both names valid and configured; without a registration the next
		// check holds the job (§3 row 7).
		jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: optedIn()}))
		e.waitHeld(t, jobID, validateTimeout, "no_registration")
	})
}

// §1.3, README "Grammars", V-I07: only the literal true opts in. Anything else
// is 422 not-opted-in at ingest and nothing is stored.
func testOptIn(t *testing.T) {
	e := newEnv(t)
	token := serverToken(t, serverA)
	cases := map[string]map[string]string{
		"absent":        without(optedIn(), "recap-enabled"),
		"false":         with(optedIn(), "recap-enabled", "false"),
		"True":          with(optedIn(), "recap-enabled", "True"),
		"1":             with(optedIn(), "recap-enabled", "1"),
		"yes":           with(optedIn(), "recap-enabled", "yes"),
		"leading-space": with(optedIn(), "recap-enabled", " true"),
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			p := buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: meta})
			expectProblem(t, e.putManifest(t, p, token), http.StatusUnprocessableEntity, "not-opted-in", "PUT manifest, not opted in")
			r := e.do(t, http.MethodGet, ingestPath(p.id), token, nil)
			expectStatus(t, r, http.StatusNotFound, "GET package after not-opted-in (nothing stored)")
		})
	}
	t.Run("true", func(t *testing.T) {
		p := buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: optedIn()})
		expectStatus(t, e.putManifest(t, p, token), http.StatusCreated, "PUT manifest, opted in")
	})
}

// §3: the first failing check sets the outcome. Each row uses a package that
// fails its own check and a later one, so the row's outcome also pins the order.
func testValidateOrder(t *testing.T) {
	e := newEnv(t)
	type row struct {
		name     string
		base     string            // fixture the package comes from
		meta     map[string]string // events.xml and manifest
		server   string
		register *owner // registration before ingest, nil for none
		meeting  bool   // register a different meeting_id than the package's
		cancel   bool   // DELETE the registration before ingest (tombstone)
		want     string // held reason, or "cancelled"
		detail   string // admin job detail: the code of the first failing check (§3; architecture.md §4)
	}
	unresolved := unresolvable
	rows := []row{
		{name: "1-tombstone-before-version", meta: with(optedIn(), "recap-v", "2"), register: &ada, cancel: true, want: "cancelled"},
		{name: "2-version-before-type-configured", meta: with(with(optedIn(), "recap-v", "2"), "recap-type", "no-such-type"), want: "invalid_metadata"},
		{name: "3-type-grammar-before-sink-configured", meta: with(with(optedIn(), "recap-type", "Meeting-summary"), "recap-sink", "no-such-sink"), want: "invalid_metadata"},
		{name: "4-type-configured-before-sink-grammar", base: "F-04", meta: without(with(optedIn(), "recap-type", "no-such-type"), "recap-sink"), want: "unknown_type"},
		{name: "5-sink-grammar-before-registration", base: "F-06", want: "invalid_metadata"},
		{name: "6-sink-configured-before-registration", meta: with(optedIn(), "recap-sink", "no-such-sink"), want: "unknown_sink"},
		{name: "7-registration", want: "no_registration"},
		{name: "8-server-before-meeting-id", server: serverB, register: &ada, meeting: true, want: "no_registration", detail: "server-not-allowed"},
		{name: "9-meeting-id-before-owner", register: &unresolved, meeting: true, want: "no_registration", detail: "meeting-id-mismatch"},
		{name: "10-owner", register: &unresolved, want: "owner_unresolved"},
	}
	t.Run("0-opt-in-before-all", func(t *testing.T) {
		p := buildPackage(t, pkgOpts{recordID: freshRecordID(), meetingID: freshMeetingID(), meta: without(with(optedIn(), "recap-v", "2"), "recap-enabled")})
		expectProblem(t, e.putManifest(t, p, serverToken(t, serverA)), http.StatusUnprocessableEntity, "not-opted-in", "PUT manifest, not opted in")
	})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rid, mid := freshRecordID(), freshMeetingID()
			if r.register != nil {
				regMeeting := mid
				if r.meeting {
					regMeeting = freshMeetingID()
				}
				e.register(t, rid, regMeeting, *r.register)
				if r.cancel {
					expectStatus(t, e.deleteReg(t, frontendToken(t), rid), http.StatusNoContent, "DELETE registration")
				}
			}
			jobID := e.ingest(t, buildPackage(t, pkgOpts{base: r.base, server: r.server, recordID: rid, meetingID: mid, meta: r.meta}))
			timeout := validateTimeout
			if r.want == "owner_unresolved" {
				timeout = pipelineTimeout
			}
			var j jobView
			if r.want == "cancelled" {
				j = e.waitJob(t, jobID, timeout, "cleaned/cancelled", func(j jobView) bool {
					return j.State == "cleaned" && j.Outcome == "cancelled"
				})
			} else {
				j = e.waitHeld(t, jobID, timeout, r.want)
			}
			if r.detail != "" && j.Detail != r.detail {
				t.Errorf("admin job detail %q, want %q", j.Detail, r.detail)
			}
		})
	}
}

// §1.2: other recap-* keys are reserved and ignored by v1.
func testUnknownKey(t *testing.T) {
	e := newEnv(t)
	rid, mid := freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, ada)
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: with(optedIn(), "recap-future-key", "x")}))
	e.waitMatched(t, rid, mid, jobID)
}

// V-S04 (meeting-package.md §7): manifest metadata must equal
// /recording/metadata. The packages differ only in bbb-origin, which no §3
// check reads, so the outcome can only come from V-S04.
func testManifestMismatch(t *testing.T) {
	e := newEnv(t)
	rid, mid := freshRecordID(), freshMeetingID()
	e.register(t, rid, mid, ada)
	jobID := e.ingest(t, buildPackage(t, pkgOpts{recordID: rid, meetingID: mid, meta: optedIn(),
		manifestMeta: with(optedIn(), "bbb-origin", "Greenlight")}))
	e.waitJob(t, jobID, validateTimeout, "failed/manifest_metadata_mismatch", func(j jobView) bool {
		return j.State == "failed" && j.FailureReason == "manifest_metadata_mismatch"
	})
}
