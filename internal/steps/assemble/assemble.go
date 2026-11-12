// Package assemble implements the assemble step (docs/reference/architecture.md
// §5.2): the normalized meeting and the per-track ASR outcomes of transcribe
// become the transcript of docs/reference/providers.md §4.2 and §4.3
// (schemas/transcript.schema.json).
//
// Assemble is a pure function shared by the queue handler and
// `recap tool assemble`. Its output is deterministic: the same input gives
// the same bytes (no map order, no clock; created_at is the job's).
//
// The transcript holds personal data (spoken words, display names). Nothing
// in this package logs it.
package assemble

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/steps/transcribe"
)

// SchemaVersion is the transcript schema version written.
const SchemaVersion = "1.0.0"

// Track statuses (§4.2).
const (
	StatusTranscribed = "transcribed"
	StatusEmpty       = "empty"
	StatusFailed      = "failed"
)

// gapReasons are the README gap reasons a transcribe outcome may carry.
var gapReasons = map[string]bool{
	transcribe.GapASRFailed:          true,
	transcribe.GapASRRejectedInput:   true,
	transcribe.GapASRInvalidResponse: true,
}

// maxDisplayName is the schema's maxLength of display_name (characters).
const maxDisplayName = 256

// Transcript is the transcript document (§4.2). Field order follows the
// schema.
type Transcript struct {
	SchemaVersion    string           `json:"schema_version"`
	JobID            string           `json:"job_id"`
	CreatedAt        string           `json:"created_at"`
	Source           string           `json:"source"`
	Language         *string          `json:"language"`
	TimeBase         TimeBase         `json:"time_base"`
	RecordingPeriods []meeting.Period `json:"recording_periods"`
	Speakers         []Speaker        `json:"speakers"`
	Tracks           []Track          `json:"tracks"`
	Segments         []Segment        `json:"segments"`
	Gaps             []Gap            `json:"gaps"`
}

// TimeBase is the origin of the meeting axis (§4.3 item 1).
type TimeBase struct {
	Clock         string `json:"clock"`
	Origin        string `json:"origin"`
	OriginEventTS int64  `json:"origin_event_ts"`
	OriginUTC     string `json:"origin_utc"`
}

// Speaker is one participant with at least one track.
type Speaker struct {
	Ref         string `json:"ref"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Track is one audio track on the meeting axis.
type Track struct {
	Ref        string    `json:"ref"`
	SpeakerRef string    `json:"speaker_ref"`
	Kind       string    `json:"kind"`
	BlobSHA256 string    `json:"blob_sha256"`
	StartMS    int64     `json:"start_ms"`
	EndMS      int64     `json:"end_ms"`
	Status     string    `json:"status"`
	Language   string    `json:"language,omitempty"`
	Provider   *Provider `json:"provider,omitempty"`
}

// Provider is the provenance of a track's ASR result.
type Provider struct {
	Profile string `json:"profile"`
	Dialect string `json:"dialect"`
	Model   string `json:"model"`
}

// Segment is one transcribed stretch of a track on the meeting axis.
type Segment struct {
	ID         string `json:"id"`
	TrackRef   string `json:"track_ref"`
	SpeakerRef string `json:"speaker_ref"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	Text       string `json:"text"`
}

// Gap is the span of a track that has no text.
type Gap struct {
	TrackRef   string `json:"track_ref"`
	SpeakerRef string `json:"speaker_ref"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	Reason     string `json:"reason"`
}

// TrackASR is the transcribe outcome of one track, keyed by the track's path
// (an opaque file reference): a result with its provenance, or a gap reason
// (contracts/transcript/README.md "Assemble form").
type TrackASR struct {
	Path    string               `json:"path"`
	Profile string               `json:"profile,omitempty"`
	Dialect string               `json:"dialect,omitempty"`
	Result  *providers.AsrResult `json:"result,omitempty"`
	Gap     string               `json:"gap,omitempty"`
}

// Input is what Assemble needs.
type Input struct {
	JobID     string
	CreatedAt time.Time // the job's created_at (§4.3)
	Meeting   *meeting.Meeting
	// ASR has one entry per track that normalize could read; tracks that
	// normalize made a gap have none.
	ASR []TrackASR
}

// Assemble builds the transcript and returns its JSON. The result is checked
// against the schema and the §4.2 rules; a violation, or an input that does
// not fit the normalized meeting, is an error.
func Assemble(in Input) ([]byte, error) {
	tr, err := build(in)
	if err != nil {
		return nil, err
	}
	data, err := Marshal(tr)
	if err != nil {
		return nil, err
	}
	if err := Check(data); err != nil {
		return nil, fmt.Errorf("assemble: invalid transcript: %w", err)
	}
	return data, nil
}

// Marshal returns the JSON of a transcript, without HTML escaping.
func Marshal(tr *Transcript) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tr); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func build(in Input) (*Transcript, error) {
	m := in.Meeting
	if m == nil {
		return nil, fmt.Errorf("assemble: no normalized meeting")
	}
	byPath := map[string]TrackASR{}
	for _, a := range in.ASR {
		if _, dup := byPath[a.Path]; dup {
			return nil, fmt.Errorf("assemble: two ASR outcomes for one track")
		}
		byPath[a.Path] = a
	}

	tr := &Transcript{
		SchemaVersion: SchemaVersion,
		JobID:         in.JobID,
		CreatedAt:     in.CreatedAt.UTC().Format(time.RFC3339Nano),
		Source:        "asr",
		TimeBase: TimeBase{
			Clock:         "bbb-event-timestamp",
			Origin:        "first-recording-start",
			OriginEventTS: m.TimeBase.OriginEventTS,
			OriginUTC:     m.TimeBase.OriginUTC,
		},
		RecordingPeriods: m.RecordingPeriods,
		Speakers:         []Speaker{},
		Tracks:           []Track{},
		Segments:         []Segment{},
		Gaps:             []Gap{},
	}

	people := map[string]meeting.Participant{}
	for _, p := range m.Participants {
		people[p.Ref] = p
	}
	speakerOf := map[string]string{} // participant ref → speaker ref
	speech := map[string]int64{}     // language → transcribed ms
	type seg struct {
		Segment
		track int // track number, for sorting
	}
	var segs []seg
	used := 0

	for i, nt := range m.Tracks {
		ref := transcribe.TrackRef(i)
		spk, ok := speakerOf[nt.Participant]
		if !ok {
			spk = fmt.Sprintf("spk-%d", len(speakerOf)+1)
			speakerOf[nt.Participant] = spk
			s, err := speaker(spk, people[nt.Participant])
			if err != nil {
				return nil, err
			}
			tr.Speakers = append(tr.Speakers, s)
		}
		t := Track{Ref: ref, SpeakerRef: spk, Kind: nt.Kind, BlobSHA256: nt.SHA256, StartMS: nt.StartMS,
			EndMS: nt.EndMS}
		a, has := byPath[nt.Path]
		if has {
			used++
		}
		reason := ""
		switch {
		case nt.Gap != "":
			if has {
				return nil, fmt.Errorf("assemble: track %s is a normalize gap but has an ASR outcome", ref)
			}
			reason = nt.Gap
		case !has:
			return nil, fmt.Errorf("assemble: track %s has no ASR outcome", ref)
		case a.Gap != "":
			if !gapReasons[a.Gap] || a.Result != nil {
				return nil, fmt.Errorf("assemble: track %s: bad ASR gap outcome", ref)
			}
			reason = a.Gap
		case a.Result == nil:
			return nil, fmt.Errorf("assemble: track %s: ASR outcome has neither result nor gap", ref)
		}
		if reason != "" {
			t.Status = StatusFailed
			tr.Gaps = append(tr.Gaps, Gap{TrackRef: ref, SpeakerRef: spk, StartMS: t.StartMS, EndMS: t.EndMS,
				Reason: reason})
			tr.Tracks = append(tr.Tracks, t)
			continue
		}

		r := a.Result
		if r.Language != nil {
			t.Language = *r.Language
		}
		t.Provider = &Provider{Profile: a.Profile, Dialect: a.Dialect, Model: r.Model}
		kept := 0
		for _, s := range r.Segments {
			text := strings.TrimSpace(s.Text)
			if text == "" {
				continue
			}
			start, end := t.StartMS+s.StartMS, t.StartMS+s.EndMS // §4.3 item 4
			end = max(end, start)
			if !midpointInPeriods(m.RecordingPeriods, start, end) { // §4.3 item 5
				continue
			}
			kept++
			segs = append(segs, seg{Segment{TrackRef: ref, SpeakerRef: spk, StartMS: start, EndMS: end, Text: text},
				i + 1})
			if t.Language != "" {
				speech[t.Language] += end - start
			}
		}
		t.Status = StatusTranscribed
		if kept == 0 {
			t.Status = StatusEmpty
		}
		tr.Tracks = append(tr.Tracks, t)
	}
	if used != len(byPath) {
		return nil, fmt.Errorf("assemble: %d ASR outcomes match no track", len(byPath)-used)
	}

	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].StartMS != segs[j].StartMS {
			return segs[i].StartMS < segs[j].StartMS
		}
		return segs[i].track < segs[j].track
	})
	for i, s := range segs {
		s.ID = fmt.Sprintf("seg-%06d", i+1)
		tr.Segments = append(tr.Segments, s.Segment)
	}
	// Tracks are in ref order, so a stable sort by start keeps ref order on ties.
	sort.SliceStable(tr.Gaps, func(i, j int) bool { return tr.Gaps[i].StartMS < tr.Gaps[j].StartMS })
	tr.Language = language(tr.Tracks, speech)
	return tr, nil
}

// speaker returns the speaker entry of a participant: its display name and
// role, if known (§4.2). A role other than moderator or viewer does not fit
// the normalized meeting and is an error.
func speaker(ref string, p meeting.Participant) (Speaker, error) {
	s := Speaker{Ref: ref, DisplayName: strings.TrimSpace(p.DisplayName), Role: p.Role}
	if r := []rune(s.DisplayName); len(r) > maxDisplayName {
		s.DisplayName = string(r[:maxDisplayName])
	}
	switch p.Role {
	case "", meeting.RoleModerator, meeting.RoleViewer:
		return s, nil
	}
	return Speaker{}, fmt.Errorf("assemble: participant %s has an unknown role", p.Ref)
}

// midpointInPeriods reports whether the midpoint of [start, end] lies inside
// a recording period (bounds included). It compares doubled values, so no
// rounding is needed.
func midpointInPeriods(ps []meeting.Period, start, end int64) bool {
	mid2 := start + end
	for _, p := range ps {
		if 2*p.StartMS <= mid2 && mid2 <= 2*p.EndMS {
			return true
		}
	}
	return false
}

// language returns the language with the most transcribed speech ms over
// the tracks that reported one (§4.2); a tie goes to the code that sorts
// first. Nil if no track reported a language.
func language(tracks []Track, speech map[string]int64) *string {
	var langs []string
	seen := map[string]bool{}
	for _, t := range tracks {
		if t.Language != "" && !seen[t.Language] {
			seen[t.Language] = true
			langs = append(langs, t.Language)
		}
	}
	if len(langs) == 0 {
		return nil
	}
	sort.Strings(langs)
	best := langs[0]
	for _, l := range langs[1:] {
		if speech[l] > speech[best] {
			best = l
		}
	}
	return &best
}
