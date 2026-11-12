package validate

import (
	"testing"

	"github.com/virtUOS/bbb-recap/internal/config"
)

var (
	testConfigured = Configured{MeetingTypes: []string{"meeting-summary"}, Sinks: []string{"docs-main"}}
	testServers    = []config.FrontendServer{{FrontendID: "pilos-fixture", BBBServerID: "bbb-fixture-a"}}
	testJob        = &Job{RecordID: "rec-1", MeetingID: "meeting-1", BBBServerID: "bbb-fixture-a"}
)

func optedIn() map[string]string {
	return map[string]string{
		"recap-enabled": "true", "recap-v": "1", "recap-type": "meeting-summary", "recap-sink": "docs-main",
	}
}

func with(m map[string]string, kv ...string) map[string]string {
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "<absent>" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

// TestDecideOrder covers rows 1–9 of metadata-interface.md §3: each row's
// case also fails a later check, so the first failing check must win.
func TestDecideOrder(t *testing.T) {
	good := &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "meeting-1", Status: "registered"}
	tomb := &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "meeting-1", Status: "cancelled"}
	otherFE := &Registration{ID: 7, FrontendID: "other-fixture", MeetingID: "other", Status: "registered"}
	otherMeeting := &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "other", Status: "registered"}
	rows := []struct {
		name string
		reg  *Registration
		meta map[string]string
		want Decision
	}{
		{"1-tombstone-before-version", tomb, with(optedIn(), "recap-v", "2"), Decision{Cancel: true}},
		{"2-version-before-type", nil, with(optedIn(), "recap-v", "2", "recap-type", "no-such-type"),
			Decision{Reason: HeldInvalidMetadata, Detail: DetailVersion}},
		{"2-version-absent", good, with(optedIn(), "recap-v", "<absent>"),
			Decision{Reason: HeldInvalidMetadata, Detail: DetailVersion}},
		{"3-type-grammar-before-sink", nil, with(optedIn(), "recap-type", "Meeting-summary", "recap-sink", "no-such-sink"),
			Decision{Reason: HeldInvalidMetadata, Detail: DetailTypeInvalid}},
		{"3-type-absent", good, with(optedIn(), "recap-type", "<absent>"),
			Decision{Reason: HeldInvalidMetadata, Detail: DetailTypeInvalid}},
		{"4-type-configured-before-sink-grammar", nil, with(optedIn(), "recap-type", "no-such-type", "recap-sink", "<absent>"),
			Decision{Reason: HeldUnknownType, Detail: DetailTypeUnknown}},
		{"5-sink-grammar-before-registration", nil, with(optedIn(), "recap-sink", "<absent>"),
			Decision{Reason: HeldInvalidMetadata, Detail: DetailSinkInvalid}},
		{"6-sink-configured-before-registration", nil, with(optedIn(), "recap-sink", "no-such-sink"),
			Decision{Reason: HeldUnknownSink, Detail: DetailSinkUnknown}},
		{"7-registration", nil, optedIn(), Decision{Reason: HeldNoRegistration, Detail: DetailNoRegistration}},
		{"8-server-before-meeting-id", otherFE, optedIn(), Decision{Reason: HeldNoRegistration, Detail: DetailServerNotAllowed}},
		{"9-meeting-id", otherMeeting, optedIn(), Decision{Reason: HeldNoRegistration, Detail: DetailMeetingIDMismatch}},
		{"ok", good, optedIn(), Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: "docs-main"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := Decide(r.reg, r.meta, "recap", testConfigured, testServers, testJob); got != r.want {
				t.Fatalf("got %+v, want %+v", got, r.want)
			}
		})
	}
}

// TestDecideWithoutServerMapping covers row 8 without
// RECAP_FRONTEND_SERVERS: any front end may claim packages of any server;
// the later rows still apply.
func TestDecideWithoutServerMapping(t *testing.T) {
	otherFE := &Registration{ID: 7, FrontendID: "other-fixture", MeetingID: "meeting-1", Status: "registered"}
	otherMeeting := &Registration{ID: 7, FrontendID: "other-fixture", MeetingID: "other", Status: "registered"}
	otherServer := &Job{RecordID: "rec-1", MeetingID: "meeting-1", BBBServerID: "bbb-cluster"}
	ok := Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: "docs-main"}
	for _, servers := range [][]config.FrontendServer{nil, {}} {
		for _, j := range []*Job{testJob, otherServer} {
			if got := Decide(otherFE, optedIn(), "recap", testConfigured, servers, j); got != ok {
				t.Errorf("server %s: got %+v, want %+v", j.BBBServerID, got, ok)
			}
		}
		want := Decision{Reason: HeldNoRegistration, Detail: DetailMeetingIDMismatch}
		if got := Decide(otherMeeting, optedIn(), "recap", testConfigured, servers, otherServer); got != want {
			t.Errorf("meeting id: got %+v, want %+v", got, want)
		}
	}
}

// TestDecideAllowLists covers row 9a of metadata-interface.md §3: a front
// end with entries in RECAP_FRONTEND_TYPES or RECAP_FRONTEND_SINKS may
// request only those names; a front end without entries may request all.
func TestDecideAllowLists(t *testing.T) {
	good := &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "meeting-1", Status: "registered"}
	otherMeeting := &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "other", Status: "registered"}
	c := Configured{MeetingTypes: []string{"meeting-summary", "lecture"}, Sinks: []string{"docs-main", "docs-other"}}
	onlyLecture := []config.FrontendName{{FrontendID: "pilos-fixture", Name: "lecture"}}
	onlyOther := []config.FrontendName{{FrontendID: "pilos-fixture", Name: "docs-other"}}
	otherFE := []config.FrontendName{{FrontendID: "moodle-fixture", Name: "lecture"}}
	both := []config.FrontendName{
		{FrontendID: "pilos-fixture", Name: "lecture"}, {FrontendID: "pilos-fixture", Name: "meeting-summary"},
	}
	ok := Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: "docs-main"}
	rows := []struct {
		name         string
		reg          *Registration
		types, sinks []config.FrontendName
		want         Decision
	}{
		{"no-lists-allow-all", good, nil, nil, ok},
		{"other-front-end-listed", good, otherFE, otherFE, ok},
		{"type-in-list", good, both, nil, ok},
		{"type-not-allowed", good, onlyLecture, nil, Decision{Reason: HeldUnknownType, Detail: DetailTypeNotAllowed}},
		{"sink-not-allowed", good, nil, onlyOther, Decision{Reason: HeldUnknownSink, Detail: DetailSinkNotAllowed}},
		{"type-before-sink", good, onlyLecture, onlyOther, Decision{Reason: HeldUnknownType, Detail: DetailTypeNotAllowed}},
		{"9-meeting-id-before-9a", otherMeeting, onlyLecture, onlyOther,
			Decision{Reason: HeldNoRegistration, Detail: DetailMeetingIDMismatch}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := c
			c.FrontendTypes, c.FrontendSinks = r.types, r.sinks
			if got := Decide(r.reg, optedIn(), "recap", c, testServers, testJob); got != r.want {
				t.Fatalf("got %+v, want %+v", got, r.want)
			}
		})
	}
}

func TestDecideMetaPrefix(t *testing.T) {
	meta := map[string]string{"rc-v": "1", "rc-type": "meeting-summary", "rc-sink": "docs-main"}
	reg := &Registration{ID: 1, FrontendID: "pilos-fixture", MeetingID: "meeting-1", Status: "registered"}
	if got := Decide(reg, meta, "rc", testConfigured, testServers, testJob); got.Reason != "" {
		t.Fatalf("prefix rc: %+v", got)
	}
	if got := Decide(reg, meta, "recap", testConfigured, testServers, testJob); got.Detail != DetailVersion {
		t.Fatalf("prefix recap: %+v", got)
	}
}
