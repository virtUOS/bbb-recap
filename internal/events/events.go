// Package events reads the parts of a BBB events.xml that the validate and
// normalize steps use (docs/reference/meeting-package.md §7, providers.md §4.3).
// It streams the document with encoding/xml, keeps only the events it knows
// and skips the rest, so whiteboard and other bulky events cost no memory.
//
// Shapes (bigbluebutton/bigbluebutton @ 7d27e7e unless noted; the sources are
// collected in docs/reference/upstream-facts.md):
//   - root <recording meeting_id bbb_version>, children <meeting id externalId
//     name breakout> and <metadata …> (events_archiver.rb:260-317);
//   - every <event> has the attributes timestamp (monotonic ms), module and
//     eventname, and its fields as child elements, among them timestampUTC
//     (RecordEvent.scala:34-41,85-91; events_archiver.rb:323-340);
//   - AudioTrackPublishedEvent / AudioTrackUnpublishedEvent of module
//     bbb-webrtc-sfu with userId, filename, source
//     (bbb-webrtc-sfu lib/bbb/messages/recording/AudioTrackPublishedEvent.js:4-14 @ c25fec6;
//     the child order in a real file was not checked);
//   - module CHAT: PublicChatEvent with senderId, messageId, message (HTML),
//     replyToMessageId (PublicChatRecordEvent.scala:25-58,
//     RedisRecorderActor.scala:166-181); EditPublicChatMessageRecordEvent with
//     messageId, message (EditPublicChatMessageRecordEvent.scala:25-38,
//     RedisRecorderActor.scala:183-191); DeletePublicChatMessageRecordEvent with
//     messageId (DeletePublicChatMessageRecordEvent.scala:25-38); and
//     ClearPublicChatEvent without fields (ClearPublicChatRecordEvent.scala:22-23);
//   - module POLL, every event with pollId (AbstractPollRecordEvent.scala:25-33):
//     PollStartedRecordEvent with question and answers (JSON of
//     SimpleAnswerOutVO {id, key}) (PollStartedRecordEvent.scala:28-56;
//     bbb-common-message …/common2/domain/Meeting2x.scala:125);
//     UserRespondedToPollRecordEvent with userId (empty for secret polls),
//     answerId, answer (UserRespondedToPollRecordEvent.scala:25-43,
//     RedisRecorderActor.scala:755-767); PollStoppedRecordEvent; and
//     PollPublishedRecordEvent with question and answers (JSON of
//     SimpleVoteOutVO {id, key, numVotes, isCorrectAnswer})
//     (PollPublishedRecordEvent.scala:28-62; Meeting2x.scala:127).
//     The event names are cited; the values in the F-01 fixture are synthetic.
//
// The document contains personal data (names, user IDs, chat). Callers must
// not log what this package returns.
package events

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
)

// ErrFormat marks a document that is not well-formed or lacks a field this
// package needs. Errors of the underlying reader are returned unwrapped.
var ErrFormat = errors.New("events.xml")

// Module and event names this package reads.
const (
	ModuleSFU   = "bbb-webrtc-sfu"
	ModuleVoice = "VOICE"

	EventRecordStatus   = "RecordStatusEvent"
	EventTrackPublished = "AudioTrackPublishedEvent"
	EventTrackUnpub     = "AudioTrackUnpublishedEvent"
	EventJoin           = "ParticipantJoinEvent"
	EventVoiceRecStart  = "StartRecordingEvent"
	EventPublicChat     = "PublicChatEvent"
	EventChatEdit       = "EditPublicChatMessageRecordEvent"
	EventChatDelete     = "DeletePublicChatMessageRecordEvent"
	EventChatClear      = "ClearPublicChatEvent"

	ModulePoll         = "POLL"
	EventPollStarted   = "PollStartedRecordEvent"
	EventPollResponded = "UserRespondedToPollRecordEvent"
	EventPollStopped   = "PollStoppedRecordEvent"
	EventPollPublished = "PollPublishedRecordEvent"

	SourceMicrophone = "microphone"
)

// Events is what the steps need from events.xml, in document order.
type Events struct {
	RecordingMeetingID string            // <recording meeting_id>
	MeetingID          string            // <meeting id>
	ExternalID         string            // <meeting externalId>
	MeetingName        string            // <meeting name>; personal data possible
	Metadata           map[string]string // attributes of <metadata>; nil if absent

	Marks       []Mark  // RecordStatusEvent
	Tracks      []Track // AudioTrack(Un)PublishedEvent of module bbb-webrtc-sfu
	Joins       []Join  // ParticipantJoinEvent
	Chat        []Chat  // public chat events (sent, edited, deleted, cleared)
	Polls       []Poll  // events of module POLL
	VoiceStarts int     // StartRecordingEvent of module VOICE (legacy bridge)

	// LastTS is the largest event timestamp in the document; HasEvents
	// reports whether there was any event with a timestamp.
	LastTS    int64
	HasEvents bool
}

// Mark is a RecordStatusEvent: status "true" starts, anything else stops
// (providers.md §4.3).
type Mark struct {
	TS    int64
	UTC   int64 // timestampUTC, epoch ms; 0 if absent
	Start bool
}

// Track is an AudioTrackPublishedEvent (Published) or an
// AudioTrackUnpublishedEvent.
type Track struct {
	TS        int64
	Published bool
	UserID    string
	Filename  string // full path on the BBB server
	Source    string // microphone, screen_share_audio, …
}

// Basename returns the file name part of Filename; BBB matches files by it
// (generators/audio.rb:137-145 @ 7d27e7e).
func (t Track) Basename() string { return path.Base(t.Filename) }

// Join is a ParticipantJoinEvent.
type Join struct {
	TS     int64
	UserID string
	Name   string // display name: personal data
	Role   string // MODERATOR or VIEWER
}

// Chat is one public chat event; Kind is its event name. Message is HTML
// (PublicChatRecordEvent.scala:25-58 @ 7d27e7e). Edits carry MessageID and Message, deletions MessageID, a
// clear nothing.
type Chat struct {
	TS        int64
	Kind      string // EventPublicChat, EventChatEdit, EventChatDelete or EventChatClear
	SenderID  string
	MessageID string
	ReplyToID string
	Message   string
}

// Poll is one event of module POLL; Kind is its event name. Answers is the
// raw JSON of the answers field. UserID is personal data and empty for
// secret polls.
type Poll struct {
	TS       int64
	Kind     string
	PollID   string
	Question string
	Answers  string
	UserID   string
	AnswerID string
	Answer   string
}

// rejoinSuffix is the suffix BBB adds to a userId on rejoin; BBB strips it
// with /_\d*$/ (generators/events.rb:36-43 @ 7d27e7e).
var rejoinSuffix = regexp.MustCompile(`_\d*$`)

// UserKey returns the userId without its rejoin suffix, so all sessions of
// one participant share a key.
func UserKey(userID string) string { return rejoinSuffix.ReplaceAllString(userID, "") }

// field is one child element of an <event>.
type field struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type rawEvent struct {
	Fields []field `xml:",any"`
}

func (e rawEvent) get(name string) string {
	for _, f := range e.Fields {
		if f.XMLName.Local == name {
			return f.Value
		}
	}
	return ""
}

// errReader remembers the first error of the underlying reader, so Parse can
// tell an I/O error from a malformed document.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// Parse reads events.xml. A malformed document or a known event with an
// unreadable timestamp is an error wrapping ErrFormat; an error of r is
// returned as it is.
func Parse(r io.Reader) (*Events, error) {
	er := &errReader{r: r}
	ev, err := parse(xml.NewDecoder(er))
	if er.err != nil {
		return nil, er.err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return ev, nil
}

func parse(dec *xml.Decoder) (*Events, error) {
	ev := &Events{}
	depth := 0
	sawRoot := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if sawRoot || t.Name.Local != "recording" {
					return nil, fmt.Errorf("root element is not one <recording>")
				}
				sawRoot = true
				ev.RecordingMeetingID = attr(t, "meeting_id")
				depth++
				continue
			}
			// Children of <recording>: read or skip each one whole.
			if err := readChild(dec, t, ev); err != nil {
				return nil, err
			}
		case xml.EndElement:
			depth--
		}
	}
	if !sawRoot {
		return nil, fmt.Errorf("no <recording> element")
	}
	return ev, nil
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// readChild consumes one child element of <recording>, including its end.
func readChild(dec *xml.Decoder, t xml.StartElement, ev *Events) error {
	switch t.Name.Local {
	case "meeting":
		ev.MeetingID = attr(t, "id")
		ev.ExternalID = attr(t, "externalId")
		ev.MeetingName = attr(t, "name")
		return dec.Skip()
	case "metadata":
		ev.Metadata = map[string]string{}
		for _, a := range t.Attr {
			ev.Metadata[a.Name.Local] = a.Value
		}
		return dec.Skip()
	case "event":
		return readEvent(dec, t, ev)
	}
	return dec.Skip()
}

func readEvent(dec *xml.Decoder, t xml.StartElement, ev *Events) error {
	name, module := attr(t, "eventname"), attr(t, "module")
	ts, tsErr := strconv.ParseInt(attr(t, "timestamp"), 10, 64)
	if tsErr == nil {
		if !ev.HasEvents || ts > ev.LastTS {
			ev.LastTS = ts
		}
		ev.HasEvents = true
	}
	known := name == EventRecordStatus || name == EventJoin || isChat(name) ||
		(module == ModuleSFU && (name == EventTrackPublished || name == EventTrackUnpub)) ||
		(module == ModulePoll && isPoll(name))
	if module == ModuleVoice && name == EventVoiceRecStart {
		ev.VoiceStarts++
	}
	if !known {
		return dec.Skip()
	}
	var raw rawEvent
	if err := dec.DecodeElement(&raw, &t); err != nil {
		return err
	}
	if tsErr != nil {
		return fmt.Errorf("%s: timestamp is not an integer", name)
	}
	switch name {
	case EventRecordStatus:
		utc, _ := strconv.ParseInt(raw.get("timestampUTC"), 10, 64)
		ev.Marks = append(ev.Marks, Mark{TS: ts, UTC: utc, Start: raw.get("status") == "true"})
	case EventTrackPublished, EventTrackUnpub:
		ev.Tracks = append(ev.Tracks, Track{
			TS:        ts,
			Published: name == EventTrackPublished,
			UserID:    raw.get("userId"),
			Filename:  raw.get("filename"),
			Source:    raw.get("source"),
		})
	case EventJoin:
		ev.Joins = append(ev.Joins, Join{TS: ts, UserID: raw.get("userId"), Name: raw.get("name"), Role: raw.get("role")})
	case EventPublicChat, EventChatEdit, EventChatDelete, EventChatClear:
		ev.Chat = append(ev.Chat, Chat{TS: ts, Kind: name, SenderID: raw.get("senderId"),
			MessageID: raw.get("messageId"), ReplyToID: raw.get("replyToMessageId"), Message: raw.get("message")})
	default: // poll events
		ev.Polls = append(ev.Polls, Poll{TS: ts, Kind: name, PollID: raw.get("pollId"), Question: raw.get("question"),
			Answers: raw.get("answers"), UserID: raw.get("userId"), AnswerID: raw.get("answerId"),
			Answer: raw.get("answer")})
	}
	return nil
}

func isChat(name string) bool {
	return name == EventPublicChat || name == EventChatEdit || name == EventChatDelete || name == EventChatClear
}

func isPoll(name string) bool {
	return name == EventPollStarted || name == EventPollResponded || name == EventPollStopped ||
		name == EventPollPublished
}

// MicPublished returns the first AudioTrackPublishedEvent with source
// microphone for each file basename.
func (ev *Events) MicPublished() map[string]Track {
	m := map[string]Track{}
	for _, t := range ev.Tracks {
		if t.Published && t.Source == SourceMicrophone {
			if _, ok := m[t.Basename()]; !ok {
				m[t.Basename()] = t
			}
		}
	}
	return m
}

// Unpublished returns the first AudioTrackUnpublishedEvent for basename at
// or after ts.
func (ev *Events) Unpublished(basename string, ts int64) (Track, bool) {
	for _, t := range ev.Tracks {
		if !t.Published && t.TS >= ts && t.Basename() == basename {
			return t, true
		}
	}
	return Track{}, false
}

// FirstStart returns the first RecordStatusEvent with status "true".
func (ev *Events) FirstStart() (Mark, bool) {
	for _, m := range ev.Marks {
		if m.Start {
			return m, true
		}
	}
	return Mark{}, false
}
