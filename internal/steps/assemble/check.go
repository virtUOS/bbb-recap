package assemble

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/schemas"
)

// ErrSchema marks a transcript that does not match the schema. Its text may
// quote values of the document, so callers never log it.
var ErrSchema = errors.New("transcript schema")

// Check validates a transcript against the embedded schema and the rules of
// providers.md §4.2 as contracts/transcript/README.md "Check form" lists
// them. It returns nil for a valid transcript. Rule errors name refs and
// indices only, never text.
func Check(data []byte) error {
	if err := schemas.Validate(schemas.Transcript, data); err != nil {
		return fmt.Errorf("%w: %v", ErrSchema, err)
	}
	var tr Transcript
	if err := json.Unmarshal(data, &tr); err != nil {
		return fmt.Errorf("%w: %v", ErrSchema, err)
	}
	if errs := rules(&tr); len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// rules returns every §4.2 rule the transcript breaks.
func rules(tr *Transcript) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	// end_ms ≥ start_ms everywhere.
	for i, p := range tr.RecordingPeriods {
		if p.EndMS < p.StartMS {
			fail("recording_periods[%d]: end before start", i)
		}
		if i > 0 && p.StartMS < tr.RecordingPeriods[i-1].EndMS {
			fail("recording_periods[%d]: unsorted or overlapping", i)
		}
	}

	speakers := map[string]bool{}
	for i, s := range tr.Speakers {
		speakers[s.Ref] = true
		if i > 0 && refNum(s.Ref) <= refNum(tr.Speakers[i-1].Ref) {
			fail("speakers[%d]: not sorted by ref", i)
		}
	}
	tracks := map[string]Track{}
	withTrack := map[string]bool{}
	for i, t := range tr.Tracks {
		tracks[t.Ref] = t
		withTrack[t.SpeakerRef] = true
		if t.EndMS < t.StartMS {
			fail("track %s: end before start", t.Ref)
		}
		if !speakers[t.SpeakerRef] {
			fail("track %s: speaker_ref does not resolve", t.Ref)
		}
		if i > 0 && refNum(t.Ref) <= refNum(tr.Tracks[i-1].Ref) {
			fail("tracks[%d]: not sorted by ref", i)
		}
		if t.Status == StatusFailed && (t.Language != "" || t.Provider != nil) {
			fail("track %s: failed track has a language or provider", t.Ref)
		}
	}
	for _, s := range tr.Speakers {
		if !withTrack[s.Ref] {
			fail("speaker %s: no microphone track", s.Ref)
		}
	}

	// span checks a segment or gap: times, refs, its track's speaker.
	span := func(what string, i int, trackRef, speakerRef string, start, end int64) (Track, bool) {
		if end < start {
			fail("%s[%d]: end before start", what, i)
		}
		t, ok := tracks[trackRef]
		switch {
		case !ok:
			fail("%s[%d]: track_ref does not resolve", what, i)
		case !speakers[speakerRef]:
			fail("%s[%d]: speaker_ref does not resolve", what, i)
		case speakerRef != t.SpeakerRef:
			fail("%s[%d]: speaker_ref differs from its track's", what, i)
		}
		return t, ok
	}
	ids := map[string]bool{}
	for i, s := range tr.Segments {
		span("segments", i, s.TrackRef, s.SpeakerRef, s.StartMS, s.EndMS)
		if ids[s.ID] {
			fail("segments[%d]: duplicate id", i)
		}
		ids[s.ID] = true
		if i > 0 && before(s.StartMS, s.TrackRef, tr.Segments[i-1].StartMS, tr.Segments[i-1].TrackRef) {
			fail("segments[%d]: not sorted by start_ms, track_ref", i)
		}
	}
	for i, g := range tr.Gaps {
		if t, ok := span("gaps", i, g.TrackRef, g.SpeakerRef, g.StartMS, g.EndMS); ok && t.Status != StatusFailed {
			fail("gaps[%d]: track %s is not failed", i, g.TrackRef)
		}
		if i > 0 && before(g.StartMS, g.TrackRef, tr.Gaps[i-1].StartMS, tr.Gaps[i-1].TrackRef) {
			fail("gaps[%d]: not sorted by start_ms, track_ref", i)
		}
	}

	// docs/reference/README.md "Units and time": RFC 3339 UTC.
	if !isUTC(tr.CreatedAt) {
		fail("created_at is not UTC")
	}
	if !isUTC(tr.TimeBase.OriginUTC) {
		fail("time_base.origin_utc is not UTC")
	}
	return errs
}

// before reports whether (start, track) sorts before (prevStart, prevTrack).
func before(start int64, track string, prevStart int64, prevTrack string) bool {
	if start != prevStart {
		return start < prevStart
	}
	return refNum(track) < refNum(prevTrack)
}

// refNum returns n of a ref "<kind>-<n>"; the schema guarantees the form.
func refNum(ref string) int {
	n, _ := strconv.Atoi(ref[strings.IndexByte(ref, '-')+1:])
	return n
}

// isUTC reports whether an RFC 3339 time has the offset Z or +00:00.
func isUTC(s string) bool {
	return strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "+00:00")
}
