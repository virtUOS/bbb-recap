package main

import (
	"slices"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/config"
)

// A meeting type whose format directory is missing is not in validate's
// configured list, so validate holds it unknown_type (templates.md §1).
func TestConfiguredMeetingTypes(t *testing.T) {
	cfg := &config.Config{FormatsDir: "../../formats", MeetingTypes: []config.MeetingType{
		{Name: "meeting-summary", Format: "meeting-summary"},
		{Name: "lecture", Format: "no-such-format"},
	}}
	if got := configuredMeetingTypes(cfg); !slices.Equal(got, []string{"meeting-summary"}) {
		t.Errorf("configured meeting types %v, want [meeting-summary]", got)
	}
}
