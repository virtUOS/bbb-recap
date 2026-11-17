package validate

import (
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/config"
)

// Several sinks and the Matrix room (metadata-interface.md §1.2, §3 rows 5,
// 6, 6a, 6b, 9a; ADR-016). The contract tests (CT-META-07, CT-META-08) cover
// the service; rows 6a and 9a per name are left to these tests.

var multiConfigured = Configured{
	MeetingTypes: []string{"meeting-summary"},
	Sinks:        []string{"docs-main", "docs-other", "frontend-main", "frontend-other", "webhook-main", "matrix-main", "matrix-other"},
	SinkTypes: map[string]string{"docs-main": "docs", "docs-other": "docs", "frontend-main": "frontend",
		"frontend-other": "frontend", "webhook-main": "webhook", "matrix-main": "matrix", "matrix-other": "matrix"},
}

var goodReg = &Registration{ID: 7, FrontendID: "pilos-fixture", MeetingID: "meeting-1", Status: "registered"}

func sinkMeta(list string, kv ...string) map[string]string {
	return with(with(optedIn(), "recap-sink", list), kv...)
}

func TestDecideSinkList(t *testing.T) {
	invalid := Decision{Reason: HeldInvalidMetadata, Detail: DetailSinkInvalid}
	unknown := Decision{Reason: HeldUnknownSink, Detail: DetailSinkUnknown}
	listInvalid := Decision{Reason: HeldInvalidMetadata, Detail: DetailSinkListInvalid}
	ok := func(list string) Decision {
		return Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: list}
	}
	for _, c := range []struct {
		name string
		list string
		want Decision
	}{
		{"one", "docs-main", ok("docs-main")},
		{"two", "docs-main,frontend-main", ok("docs-main,frontend-main")},
		{"group-order-wins", "webhook-main,frontend-main,docs-main", ok("frontend-main,docs-main,webhook-main")},
		{"four", "webhook-main,docs-other,frontend-main,docs-main", ok("docs-other,frontend-main,docs-main,webhook-main")},
		{"five", "docs-main,docs-other,frontend-main,webhook-main,frontend-other", invalid},
		{"empty", "", invalid},
		{"trailing-comma", "docs-main,", invalid},
		{"leading-comma", ",docs-main", invalid},
		{"empty-element", "docs-main,,frontend-main", invalid},
		{"space", "docs-main, frontend-main", invalid},
		{"duplicate", "docs-main,frontend-main,docs-main", invalid},
		{"semicolon", "docs-main;frontend-main", invalid},
		{"uppercase", "docs-main,Frontend-main", invalid},
		{"unknown", "docs-main,no-such-sink", unknown},
		{"two-frontend", "frontend-main,frontend-other", listInvalid},
		{"two-matrix", "matrix-main,matrix-other", listInvalid},
		{"6-before-6a", "frontend-main,frontend-other,no-such-sink", unknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(goodReg, sinkMeta(c.list), "recap", multiConfigured, testServers, testJob); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDecideMatrixRoom(t *testing.T) {
	invalid := Decision{Reason: HeldInvalidMetadata, Detail: DetailMatrixRoomInvalid}
	for _, room := range []string{
		"!fixturejoined01:matrix.recap.test",
		"#fixture-team:matrix.recap.test",
		"!AbC_d-E=f+g/h~i.j:matrix.recap.test",
		"!fixtureRoomV12abcdefghijklmnopqrstuvwxyz0123456789_-",
		"!r:other.recap.test:8448",
		"#a:[2001:db8::1]:8448",
		"!" + strings.Repeat("a", 254),
	} {
		t.Run("valid/"+room[:min(len(room), 40)], func(t *testing.T) {
			got := Decide(goodReg, sinkMeta("docs-main,matrix-main", "recap-matrix-room", room), "recap",
				multiConfigured, testServers, testJob)
			want := Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: "docs-main,matrix-main",
				MatrixRoom: room}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
	for name, room := range map[string]string{
		"missing":              "<absent>",
		"empty":                "",
		"no-sigil":             "fixturejoined01:matrix.recap.test",
		"alias-without-server": "#fixture-team",
		"whitespace":           "!fixture joined01:matrix.recap.test",
		"empty-opaque":         "!:matrix.recap.test",
		"256-chars":            "!" + strings.Repeat("a", 255),
		"user-id":              "@fixture-bot:matrix.recap.test",
		"url":                  "https://matrix.to/#/!fixturejoined01:matrix.recap.test",
		"non-ascii":            "#tëam:matrix.recap.test",
		"empty-server":         "!room:",
		"port-too-long":        "!room:matrix.recap.test:123456",
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			got := Decide(goodReg, sinkMeta("docs-main,matrix-main", "recap-matrix-room", room), "recap",
				multiConfigured, testServers, testJob)
			if got != invalid {
				t.Fatalf("got %+v, want %+v", got, invalid)
			}
		})
	}
	t.Run("ignored-without-matrix-sink", func(t *testing.T) {
		got := Decide(goodReg, sinkMeta("docs-main", "recap-matrix-room", "not a room"), "recap", multiConfigured,
			testServers, testJob)
		want := Decision{RegistrationID: 7, MeetingType: "meeting-summary", SinkProfile: "docs-main"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("6a-before-6b", func(t *testing.T) {
		got := Decide(goodReg, sinkMeta("matrix-main,matrix-other"), "recap", multiConfigured, testServers, testJob)
		if got.Detail != DetailSinkListInvalid {
			t.Fatalf("got %+v, want %s", got, DetailSinkListInvalid)
		}
	})
}

// Row 9a: every name of the list must be allowed for the front end.
func TestDecideSinkListAllowList(t *testing.T) {
	c := multiConfigured
	c.FrontendSinks = []config.FrontendName{
		{FrontendID: "pilos-fixture", Name: "docs-main"}, {FrontendID: "pilos-fixture", Name: "frontend-main"},
	}
	if got := Decide(goodReg, sinkMeta("frontend-main,docs-main"), "recap", c, testServers, testJob); got.Reason != "" {
		t.Errorf("both allowed: %+v", got)
	}
	want := Decision{Reason: HeldUnknownSink, Detail: DetailSinkNotAllowed}
	if got := Decide(goodReg, sinkMeta("docs-main,webhook-main"), "recap", c, testServers, testJob); got != want {
		t.Errorf("one not allowed: got %+v, want %+v", got, want)
	}
}
