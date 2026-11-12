package normalize

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

// normalizeEvents normalizes a package that holds only the given events.xml.
func normalizeEvents(t *testing.T, file string) *meeting.Meeting {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Normalize(fstest.MapFS{"events.xml": {Data: b}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Chat edits, deletions (with replies), a clear, HTML and the pause rule on
// a synthetic events.xml.
func TestChatContent(t *testing.T) {
	m := normalizeEvents(t, "testdata/content-events.xml")
	want := []meeting.Chat{
		{TMS: 500, Participant: "p-1", Text: "Fish & chips\nsecond line"},
		{TMS: 600, Participant: "p-2", Text: "fixed <3"},
		{TMS: 2500, Participant: "p-3", Text: "last word"},
	}
	if !reflect.DeepEqual(m.Chat, want) {
		t.Errorf("chat\n got %+v\nwant %+v", m.Chat, want)
	}
	// Una's rejoin key wrote only a deleted reply; the sender without a
	// join event gets a participant without a name.
	if len(m.Participants) != 3 || m.Participants[2].DisplayName != "" {
		t.Errorf("participants %+v", m.Participants)
	}
}

// Polls: options from the start event, else from the responses; a user's
// last response counts, with all its options; secret responses count one
// each; unpublished polls and polls published in a pause are dropped.
func TestPollContent(t *testing.T) {
	m := normalizeEvents(t, "testdata/content-events.xml")
	want := []meeting.Poll{
		{TMS: 1600, Question: "Pick colours", Options: []meeting.PollOption{
			{Text: "Red", Votes: 0}, {Text: "Green", Votes: 1}, {Text: "Blue", Votes: 2}}},
		{TMS: 1800, Question: "Secret?", Options: []meeting.PollOption{{Text: "Yes", Votes: 1}, {Text: "No", Votes: 2}}},
	}
	if !reflect.DeepEqual(m.Polls, want) {
		t.Errorf("polls\n got %+v\nwant %+v", m.Polls, want)
	}
}

// F-01's poll: question, option and counts, and nothing that identifies a
// participant in the poll part of the normalized JSON.
func TestPollsF01(t *testing.T) {
	m, out := normalizeDir(t, validatetest.F01Dir)
	want := []meeting.Poll{{TMS: 26000, Question: "Blue or green?",
		Options: []meeting.PollOption{{Text: "Blue", Votes: 2}}}}
	if !reflect.DeepEqual(m.Polls, want) {
		t.Fatalf("polls %+v, want %+v", m.Polls, want)
	}
	var raw struct {
		Polls json.RawMessage `json:"polls"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(string(raw.Polls))
	for _, bad := range []string{"w_fix", "testperson", "ada", "ben", "userid", "p-1", "p-2"} {
		if strings.Contains(low, bad) {
			t.Errorf("poll part contains %q: %s", bad, raw.Polls)
		}
	}
	if len(m.Chat) != 2 || m.Chat[0].Text != "Link to the widget spec will follow." || m.Chat[1].Text != "Thanks." {
		t.Errorf("chat %+v", m.Chat)
	}
}
