package events

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

const f01Events = "../../testdata/meetings/F-01/raw/536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000/events.xml"

func TestParseF01(t *testing.T) {
	f, err := os.Open(f01Events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ev, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if ev.MeetingID != "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000" || ev.ExternalID != "fixture-meeting-01" ||
		ev.RecordingMeetingID != ev.MeetingID {
		t.Errorf("ids: %q %q %q", ev.RecordingMeetingID, ev.MeetingID, ev.ExternalID)
	}
	if ev.Metadata["recap-type"] != "meeting-summary" || len(ev.Metadata) != 8 {
		t.Errorf("metadata: %d keys", len(ev.Metadata))
	}
	if len(ev.Marks) != 4 || !ev.Marks[0].Start || ev.Marks[1].Start || ev.Marks[0].UTC != 1761732005000 {
		t.Errorf("marks: %+v", ev.Marks)
	}
	if len(ev.Joins) != 2 || len(ev.Chat) != 2 || len(ev.Polls) != 5 || ev.VoiceStarts != 0 {
		t.Errorf("joins %d chat %d polls %d voice %d", len(ev.Joins), len(ev.Chat), len(ev.Polls), ev.VoiceStarts)
	}
	// 4 microphone and 1 screen-share publish, 4 + 1 unpublish.
	if len(ev.Tracks) != 10 || len(ev.MicPublished()) != 4 {
		t.Errorf("tracks %d, mic published %d", len(ev.Tracks), len(ev.MicPublished()))
	}
	if ev.LastTS != 5155000 || !ev.HasEvents {
		t.Errorf("last ts %d", ev.LastTS)
	}
	pub := ev.MicPublished()["microphone-w_fixada0001-TR_fixA1-1761732005200.webm"]
	if un, ok := ev.Unpublished(pub.Basename(), pub.TS); !ok || un.TS != 5060000 {
		t.Errorf("unpublish: %+v %v", un, ok)
	}
}

func TestParseFormatErrors(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":         ``,
		"not closed":    `<recording><meeting id="x"/>`,
		"wrong root":    `<foo/>`,
		"two roots":     `<recording/><recording/>`,
		"bad timestamp": `<recording><event timestamp="x" module="PARTICIPANT" eventname="RecordStatusEvent"><status>true</status></event></recording>`,
		"garbage":       `not xml at all`,
	} {
		if _, err := Parse(strings.NewReader(doc)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
		}
	}
	// An unknown event with a bad timestamp is skipped.
	if _, err := Parse(strings.NewReader(`<recording><event timestamp="x" module="CHAT" eventname="Other"/></recording>`)); err != nil {
		t.Errorf("unknown event: %v", err)
	}
}

type failingReader struct{}

var errDisk = errors.New("disk on fire")

func (failingReader) Read([]byte) (int, error) { return 0, errDisk }

func TestParseReaderError(t *testing.T) {
	_, err := Parse(io.MultiReader(strings.NewReader(`<recording>`), failingReader{}))
	if !errors.Is(err, errDisk) || errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
}

func TestUserKey(t *testing.T) {
	for in, want := range map[string]string{
		"w_fixada0001": "w_fixada0001", "w_fixada0001_1": "w_fixada0001", "w_fixada0001_12": "w_fixada0001",
		"w_x_": "w_x", "abc": "abc",
	} {
		if got := UserKey(in); got != want {
			t.Errorf("UserKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChatAndPolls(t *testing.T) {
	doc := `<recording>
  <event timestamp="10" module="CHAT" eventname="PublicChatEvent"><senderId>u1</senderId><messageId>m1</messageId><message>Hi &amp;lt;b&amp;gt;</message></event>
  <event timestamp="11" module="CHAT" eventname="PublicChatEvent"><senderId>u2</senderId><messageId>m2</messageId><replyToMessageId>m1</replyToMessageId><message>re</message></event>
  <event timestamp="12" module="CHAT" eventname="EditPublicChatMessageRecordEvent"><messageId>m1</messageId><message>Hello</message></event>
  <event timestamp="13" module="CHAT" eventname="DeletePublicChatMessageRecordEvent"><messageId>m2</messageId><deletedBy>u1</deletedBy></event>
  <event timestamp="14" module="CHAT" eventname="ClearPublicChatEvent"></event>
  <event timestamp="20" module="POLL" eventname="PollStartedRecordEvent"><pollId>p1</pollId><question>Q?</question><answers>[{"id":0,"key":"A"}]</answers></event>
  <event timestamp="21" module="POLL" eventname="UserRespondedToPollRecordEvent"><pollId>p1</pollId><userId>u1</userId><answerId>0</answerId><answer>A</answer></event>
  <event timestamp="22" module="POLL" eventname="PollStoppedRecordEvent"><pollId>p1</pollId></event>
  <event timestamp="23" module="POLL" eventname="PollPublishedRecordEvent"><pollId>p1</pollId><question>Q?</question></event>
  <event timestamp="24" module="OTHER" eventname="PollPublishedRecordEvent"><pollId>p2</pollId></event>
</recording>`
	ev, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{EventPublicChat, EventPublicChat, EventChatEdit, EventChatDelete, EventChatClear}
	if len(ev.Chat) != len(kinds) {
		t.Fatalf("chat %+v", ev.Chat)
	}
	for i, k := range kinds {
		if ev.Chat[i].Kind != k {
			t.Errorf("chat %d kind %q, want %q", i, ev.Chat[i].Kind, k)
		}
	}
	if c := ev.Chat[1]; c.SenderID != "u2" || c.MessageID != "m2" || c.ReplyToID != "m1" || c.Message != "re" {
		t.Errorf("chat 1 %+v", c)
	}
	if c := ev.Chat[0]; c.Message != "Hi &lt;b&gt;" {
		t.Errorf("chat 0 message %q", c.Message)
	}
	if c := ev.Chat[2]; c.MessageID != "m1" || c.Message != "Hello" {
		t.Errorf("edit %+v", c)
	}
	if len(ev.Polls) != 4 {
		t.Fatalf("polls %+v", ev.Polls)
	}
	if p := ev.Polls[0]; p.Kind != EventPollStarted || p.PollID != "p1" || p.Question != "Q?" || p.Answers != `[{"id":0,"key":"A"}]` {
		t.Errorf("started %+v", p)
	}
	if p := ev.Polls[1]; p.Kind != EventPollResponded || p.UserID != "u1" || p.AnswerID != "0" || p.Answer != "A" {
		t.Errorf("responded %+v", p)
	}
	if ev.Polls[3].Kind != EventPollPublished {
		t.Errorf("published %+v", ev.Polls[3])
	}
}
