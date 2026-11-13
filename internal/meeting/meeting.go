// Package meeting defines the normalized meeting: the boundary between a
// source and the pipeline (ADR-013). A source step (for BBB:
// internal/steps/normalize) writes it; the pipeline steps read it. This
// package imports no source package; boundary_test.go checks that.
//
// # Format (internal, not pinned by a contract; field "version")
//
// version changes whenever a field changes its meaning, so a reader can tell
// old blobs apart; adding a field does not change it. Readers decode the
// blob with Decode, which rejects any version but Version. Version 2 made
// chat plain text with edits and deletions applied and added polls. Version
// 3 made participant roles neutral (moderator, viewer) and span optional,
// and added the media types and the track kind.
//
// The JSON is RFC 8785 canonical (sorted keys, no whitespace), so the same
// meeting always gives the same bytes. Times are integer ms on the meeting
// axis of providers.md §4.3: ms since the origin, the first start of
// recording. Files are referenced by path and SHA-256; a path is an opaque
// file reference that pipeline code does not interpret.
//
// Each field below says what it means; "BBB:" names what the BBB source
// (internal/steps/normalize) fills in.
//
//	version            3
//	meeting_name       the meeting's name (personal data possible).
//	                   BBB: <meeting name> of events.xml
//	time_base          {origin_event_ts, origin_utc}: the origin of the meeting
//	                   axis; origin_utc is its wall-clock time (RFC 3339 UTC, ms).
//	                   BBB: the first RecordStatusEvent with status "true";
//	                   origin_event_ts is its BBB event timestamp, origin_utc
//	                   its timestampUTC
//	recording_periods  [{start_ms, end_ms, recorded_offset_ms}]: the spans that
//	                   were recorded (§4.3 item 2)
//	participants       [{ref, display_name?, role?}]: one per person, ref p-<n>.
//	                   role is moderator or viewer, absent when unknown.
//	                   BBB: one per BBB user after stripping the rejoin suffix
//	                   _N; refs numbered by first ParticipantJoinEvent, then by
//	                   first track, then by first chat message; role mapped
//	                   from MODERATOR/VIEWER, other values omitted. BBB userIds
//	                   are not kept.
//	tracks             [{path, sha256, size, media_type, kind, participant,
//	                   start_ms, end_ms, duration_ms?, gap?, span?}] sorted by
//	                   start_ms, then path. media_type is the audio's media
//	                   type; kind is microphone: the track holds the
//	                   microphone of one person, participant. duration_ms is the decoded
//	                   length. gap is "track_unreadable" when the file cannot
//	                   be read. span is a source diagnostic (how end_ms was
//	                   found); pipeline code must not read it.
//	                   BBB: one per LiveKit microphone file, media_type
//	                   audio/webm; span is "events" (AudioTrackUnpublishedEvent),
//	                   "duration" (start + decoded WebM duration), "none" (no
//	                   end known: end_ms = start_ms) or "filename" (no publish
//	                   event, providers.md §7)
//	chat               [{t_ms, participant, text}]: public chat messages sent inside a
//	                   recording period, in order, as they stood at the end:
//	                   edits applied, deleted and cleared messages dropped,
//	                   plain text; messages with no text left are dropped.
//	                   BBB: replayed from the chat events, HTML turned into
//	                   text (internal/htmltext)
//	polls              [{t_ms, question, options: [{text, votes}]}]: polls
//	                   published inside a recording period, by publish time,
//	                   with aggregate counts per option. No user IDs,
//	                   names or per-user answers.
//	                   BBB: options in answer id order
//	notes              {path, sha256, media_type}: the shared notes, media_type
//	                   text/markdown or text/html; null if absent.
//	                   BBB: notes/notes.md, else notes/notes.html; null if
//	                   notes/export_failed exists
package meeting

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/virtUOS/bbb-recap/internal/jcs"
)

// Version is the format version of the normalized meeting.
const Version = 3

// ErrVersion means a normalized meeting blob has a version this build
// cannot read, for example one written before an upgrade.
var ErrVersion = errors.New("normalized meeting: unsupported version")

// GapTrackUnreadable is the README gap reason of a track that cannot be read.
const GapTrackUnreadable = "track_unreadable"

// Media types of shared notes.
const (
	MediaTypeMarkdown = "text/markdown"
	MediaTypeHTML     = "text/html"
)

// KindMicrophone is the kind of a track that holds one participant's
// microphone.
const KindMicrophone = "microphone"

// Participant roles.
const (
	RoleModerator = "moderator"
	RoleViewer    = "viewer"
)

// Span values of the BBB source: how it found a track's end. A source
// diagnostic; pipeline code must not read it.
const (
	SpanEvents   = "events"
	SpanDuration = "duration"
	SpanNone     = "none"
	SpanFilename = "filename"
)

// Output is the step output stored in steps.output: the SHA-256 of the
// normalized meeting blob in the job's prefix.
type Output struct {
	NormalizedSHA256 string `json:"normalized_sha256"`
}

// Meeting is the normalized meeting.
type Meeting struct {
	Version          int           `json:"version"`
	MeetingName      string        `json:"meeting_name"`
	TimeBase         TimeBase      `json:"time_base"`
	RecordingPeriods []Period      `json:"recording_periods"`
	Participants     []Participant `json:"participants"`
	Tracks           []Track       `json:"tracks"`
	Chat             []Chat        `json:"chat"`
	Polls            []Poll        `json:"polls"`
	Notes            *FileRef      `json:"notes"`
}

// TimeBase is the origin of the meeting axis (providers.md §4.3 item 1).
type TimeBase struct {
	OriginEventTS int64  `json:"origin_event_ts"`
	OriginUTC     string `json:"origin_utc"`
}

// Period is one recording period (§4.3 item 2).
type Period struct {
	StartMS          int64 `json:"start_ms"`
	EndMS            int64 `json:"end_ms"`
	RecordedOffsetMS int64 `json:"recorded_offset_ms"`
}

// Participant is one person in the meeting. Role is RoleModerator,
// RoleViewer or empty (unknown).
type Participant struct {
	Ref         string `json:"ref"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Track is one audio file and its span on the meeting axis. Span is a
// source diagnostic that pipeline code must not read.
type Track struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	MediaType   string `json:"media_type"`
	Kind        string `json:"kind"`
	Participant string `json:"participant"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Span        string `json:"span,omitempty"`
	DurationMS  *int64 `json:"duration_ms,omitempty"`
	Gap         string `json:"gap,omitempty"`
}

// FileRef points at a file of the meeting by path and SHA-256, with its
// media type.
type FileRef struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

// Chat is one public chat message inside a recording period, as it stood at
// the end of the meeting: edits applied, deleted and cleared messages
// dropped, plain text.
type Chat struct {
	TMS         int64  `json:"t_ms"`
	Participant string `json:"participant"`
	Text        string `json:"text"`
}

// Poll is a published poll: its question and the aggregate count per
// option. Who answered what is never kept (templates.md §3).
type Poll struct {
	TMS      int64        `json:"t_ms"`
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
}

// PollOption is one answer option and how many participants chose it.
type PollOption struct {
	Text  string `json:"text"`
	Votes int    `json:"votes"`
}

// Marshal returns the canonical JSON of m (RFC 8785, see the package
// documentation).
func Marshal(m *Meeting) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jcs.Canonicalize(b)
}

// Decode parses a normalized meeting blob. It returns an error wrapping
// ErrVersion when the blob's version is not Version, so a reader never
// takes an old or newer format for the current one. Every pipeline step
// reads the normalized meeting through Decode.
func Decode(data []byte) (*Meeting, error) {
	var m Meeting
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("normalized meeting: %w", err)
	}
	if m.Version != Version {
		return nil, fmt.Errorf("%w %d (want %d)", ErrVersion, m.Version, Version)
	}
	return &m, nil
}
