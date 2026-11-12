// Package normalize implements the normalize step (docs/reference/architecture.md
// §5.2): it turns events.xml and the microphone files of a package into the
// normalized meeting, one JSON object that assemble, transcribe and
// summarize read instead of the raw package. The format is
// defined in internal/meeting.
package normalize

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/events"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
)

// ErrEvents means events.xml cannot give a time base: it is malformed or
// has no recording start. validate rejects such packages first (V-S01).
var ErrEvents = errors.New("normalize: unusable events.xml")

// Normalize builds the normalized meeting from the package in fsys; files
// are the package files with their hashes (manifest entries). A malformed
// events.xml or one without a recording start gives an error wrapping
// ErrEvents; other errors are I/O errors of fsys.
func Normalize(fsys fs.FS, files []manifest.File) (*meeting.Meeting, error) {
	ev, err := readEvents(fsys)
	if err != nil {
		return nil, err
	}
	origin, ok := ev.FirstStart()
	if !ok || origin.UTC == 0 {
		return nil, fmt.Errorf("%w: no recording start with timestampUTC", ErrEvents)
	}
	m := &meeting.Meeting{
		Version:     meeting.Version,
		MeetingName: ev.MeetingName,
		TimeBase: meeting.TimeBase{
			OriginEventTS: origin.TS,
			OriginUTC:     time.UnixMilli(origin.UTC).UTC().Format("2006-01-02T15:04:05.000Z"),
		},
		RecordingPeriods: periods(ev, origin.TS),
	}
	people := newParticipants(ev)

	published := ev.MicPublished()
	type owned struct {
		meeting.Track
		key string // user key of the owner
	}
	var tracks []owned
	for _, f := range files {
		if f.Role != manifest.RoleMicrophone {
			continue
		}
		t, key, err := track(fsys, ev, published, f, origin)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, owned{t, key})
	}
	// Owners without a join event get their refs in track order.
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].StartMS != tracks[j].StartMS {
			return tracks[i].StartMS < tracks[j].StartMS
		}
		return tracks[i].Path < tracks[j].Path
	})
	m.Tracks = make([]meeting.Track, 0, len(tracks))
	for _, t := range tracks {
		t.Participant = people.ref(t.key)
		m.Tracks = append(m.Tracks, t.Track)
	}

	m.Chat = chat(ev, origin.TS, m.RecordingPeriods, people)
	m.Polls = polls(ev, origin.TS, m.RecordingPeriods)
	m.Participants = people.list
	m.Notes = notes(files)
	return m, nil
}

// Marshal returns the canonical JSON of m.
func Marshal(m *meeting.Meeting) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return manifest.Canonicalize(b)
}

func readEvents(fsys fs.FS) (*events.Events, error) {
	f, err := fsys.Open("events.xml")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no events.xml", ErrEvents)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ev, err := events.Parse(f)
	if errors.Is(err, events.ErrFormat) {
		return nil, fmt.Errorf("%w: %v", ErrEvents, err)
	}
	return ev, err
}

// periods pairs RecordStatusEvent starts and stops (§4.3 item 2). A start
// while recording and a stop while paused are ignored; a start without a
// stop ends at the last event.
func periods(ev *events.Events, origin int64) []meeting.Period {
	marks := slices.Clone(ev.Marks)
	sort.SliceStable(marks, func(i, j int) bool { return marks[i].TS < marks[j].TS })
	out := []meeting.Period{}
	var offset, start int64
	open := false
	add := func(end int64) {
		p := meeting.Period{StartMS: start - origin, EndMS: end - origin, RecordedOffsetMS: offset}
		offset += p.EndMS - p.StartMS
		out = append(out, p)
	}
	for _, mk := range marks {
		switch {
		case mk.Start && !open:
			start, open = mk.TS, true
		case !mk.Start && open:
			add(mk.TS)
			open = false
		}
	}
	if open {
		add(max(ev.LastTS, start))
	}
	return out
}

func inPeriods(ps []meeting.Period, t int64) bool {
	for _, p := range ps {
		if t >= p.StartMS && t <= p.EndMS {
			return true
		}
	}
	return false
}

// micMediaType is the media type of BBB's LiveKit microphone files
// (meeting-package.md: audio/microphone-*.webm).
const micMediaType = "audio/webm"

// track builds the track of one microphone file (§4.3 item 3) and returns
// the user key of its owner.
func track(fsys fs.FS, ev *events.Events, published map[string]events.Track, f manifest.File,
	origin events.Mark) (meeting.Track, string, error) {
	t := meeting.Track{Path: f.Path, SHA256: f.SHA256, Size: f.Size, MediaType: micMediaType,
		Kind: meeting.KindMicrophone}
	dur, durOK, readable, err := decode(fsys, f.Path)
	if err != nil {
		return meeting.Track{}, "", err
	}
	if durOK {
		t.DurationMS = &dur
	}
	pub, ok := published[path.Base(f.Path)]
	if !ok {
		key := unmatchedTrack(&t, origin)
		return t, key, nil
	}
	t.StartMS = pub.TS - origin.TS
	switch unpub, ok := ev.Unpublished(path.Base(f.Path), pub.TS); {
	case ok:
		t.EndMS, t.Span = unpub.TS-origin.TS, meeting.SpanEvents
	case durOK:
		t.EndMS, t.Span = t.StartMS+dur, meeting.SpanDuration
	default:
		t.EndMS, t.Span = t.StartMS, meeting.SpanNone
		readable = false // no end where one is needed
	}
	if !readable {
		t.Gap = meeting.GapTrackUnreadable
	}
	return t, events.UserKey(pub.UserID), nil
}

// unmatchedTrack handles a microphone file without a matching
// AudioTrackPublishedEvent (V-S05 single file, fixture M-11; providers.md §7):
//   - speaker: the userId in the file name, microphone-<userId>-<trackId>-<suffix>.webm
//     (bbb-webrtc-sfu lib/livekit/livekit-recorder-manager.js:264-285 @ c25fec6);
//   - start: the suffix (Date.now() when recording started,
//     livekit-recorder-manager.js:400 @ c25fec6) minus origin_utc; end: start
//     plus the decoded duration, else start;
//   - the track is a gap track_unreadable; it is not transcribed.
//
// The name is parsed from the right: the epoch suffix is last, the trackId
// second to last, and the rest is the userId, which may contain "-". A name
// that does not parse gets a participant of its own (keyed by its path) and
// starts at 0.
func unmatchedTrack(t *meeting.Track, origin events.Mark) string {
	name := strings.TrimSuffix(strings.TrimPrefix(path.Base(t.Path), "microphone-"), ".webm")
	key := "\x00unparsed:" + t.Path // never a BBB userId
	rest, epoch, ok1 := cutLast(name)
	user, _, ok2 := cutLast(rest)
	if suffix, err := strconv.ParseInt(epoch, 10, 64); ok1 && ok2 && user != "" && err == nil {
		key = events.UserKey(user)
		t.StartMS = max(suffix-origin.UTC, 0)
	}
	t.EndMS = t.StartMS
	if t.DurationMS != nil {
		t.EndMS = t.StartMS + *t.DurationMS
	}
	t.Span = meeting.SpanFilename
	t.Gap = meeting.GapTrackUnreadable
	return key
}

// cutLast splits s at its last "-".
func cutLast(s string) (before, after string, ok bool) {
	i := strings.LastIndex(s, "-")
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}

// decode reads the WebM header of a file. readable is false when the file
// is not WebM; err is an I/O error only.
func decode(fsys fs.FS, p string) (ms int64, durOK, readable bool, err error) {
	f, err := fsys.Open(p)
	if err != nil {
		return 0, false, false, err
	}
	defer func() { _ = f.Close() }()
	ms, durOK, err = webmDuration(f)
	if errors.Is(err, errNotWebM) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	return ms, durOK, true, nil
}

// notesFiles are the shared notes exports in order of preference
// (templates.md §3) and their media types.
var notesFiles = []struct{ path, mediaType string }{
	{"notes/notes.md", meeting.MediaTypeMarkdown},
	{"notes/notes.html", meeting.MediaTypeHTML},
}

// notes picks notes/notes.md, else notes/notes.html (templates.md §3);
// export_failed means absent.
func notes(files []manifest.File) *meeting.FileRef {
	byPath := map[string]manifest.File{}
	for _, f := range files {
		if f.Role == manifest.RoleNotes {
			byPath[f.Path] = f
		}
	}
	if _, failed := byPath["notes/export_failed"]; failed {
		return nil
	}
	for _, n := range notesFiles {
		if f, ok := byPath[n.path]; ok {
			return &meeting.FileRef{Path: f.Path, SHA256: f.SHA256, MediaType: n.mediaType}
		}
	}
	return nil
}

// participants assigns refs p-<n> to user keys.
type participants struct {
	refs map[string]string // user key → ref
	list []meeting.Participant
}

func newParticipants(ev *events.Events) *participants {
	p := &participants{refs: map[string]string{}}
	for _, j := range ev.Joins {
		key := events.UserKey(j.UserID)
		if _, ok := p.refs[key]; ok {
			continue
		}
		p.add(key, meeting.Participant{DisplayName: j.Name, Role: role(j.Role)})
	}
	return p
}

// role maps a BBB role (MODERATOR, VIEWER; case-insensitive, as assemble
// matched them before format version 3) to the neutral role; any other value
// gives "" (unknown).
func role(bbb string) string {
	switch strings.ToUpper(bbb) {
	case "MODERATOR":
		return meeting.RoleModerator
	case "VIEWER":
		return meeting.RoleViewer
	}
	return ""
}

func (p *participants) add(key string, pt meeting.Participant) string {
	pt.Ref = "p-" + strconv.Itoa(len(p.list)+1)
	p.refs[key] = pt.Ref
	p.list = append(p.list, pt)
	return pt.Ref
}

// ref returns the ref of a user key, adding a participant without a name
// if the key has no join event.
func (p *participants) ref(key string) string {
	if r, ok := p.refs[key]; ok {
		return r
	}
	return p.add(key, meeting.Participant{})
}
