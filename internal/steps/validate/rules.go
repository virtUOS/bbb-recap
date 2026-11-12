// Package validate implements the validate step (docs/reference/architecture.md
// §5.2): the package rules V-S01…V-S05 of meeting-package.md §7, then the
// metadata and registration checks of metadata-interface.md §3, rows 1–9a, in
// table order. The first failing rule or check sets the outcome.
//
// Rules works on an fs.FS of package paths, so `recap tool validate-package`
// (a directory) and the queue handler (the blob store) share it.
package validate

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/events"
)

// Failure reasons of the V-S rules (docs/reference/README.md "States and enums").
const (
	ReasonEventsMismatch    = "events_mismatch"
	ReasonUnsupportedBridge = "unsupported_audio_bridge"
	ReasonMetadataMismatch  = "manifest_metadata_mismatch"
	ReasonAllTracksFailed   = "all_tracks_failed"
	jobStateFailed          = "failed"
	eventsPath              = "events.xml"
	originMetaKey           = "bbb-origin"
	originServerNameMetaKey = "bbb-origin-server-name"
)

// Violation is the first V-S rule a package breaks. Every V-S01…V-S05
// violation fails the job.
type Violation struct {
	Rule   string // V-S01 … V-S05
	Reason string // failure reason
}

// JobState is the job state the violation leads to.
func (v *Violation) JobState() string { return jobStateFailed }

// Package is what the rules check besides the files.
type Package struct {
	RecordID string
	// MeetingID is the manifest's meeting_id; empty (raw form) skips the
	// externalId part of V-S01.
	MeetingID string
	// Metadata is the manifest's metadata; nil (raw form) skips V-S04.
	Metadata   map[string]string
	MetaPrefix string
	// Microphones are the package paths of role audio-microphone.
	Microphones []string
}

// Rules applies V-S01…V-S05 to the package in fsys, in that order, and stops
// at the first violation. It returns the microphone files without a matching
// AudioTrackPublishedEvent (V-S05: each becomes a gap) when there is no
// violation. err is an I/O error of fsys only.
func Rules(fsys fs.FS, p Package) (gaps []string, v *Violation, err error) {
	ev, err := readEvents(fsys)
	if errors.Is(err, events.ErrFormat) || errors.Is(err, fs.ErrNotExist) {
		return nil, &Violation{"V-S01", ReasonEventsMismatch}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if v := checkEvents(ev, p); v != nil {
		return nil, v, nil
	}
	gaps, v = matchMicrophones(ev, p.Microphones)
	return gaps, v, nil
}

func readEvents(fsys fs.FS) (*events.Events, error) {
	f, err := fsys.Open(eventsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return events.Parse(f)
}

// checkEvents applies V-S01…V-S04 to a well-formed events.xml.
func checkEvents(ev *events.Events, p Package) *Violation {
	// V-S01: /recording/@meeting_id = record_id and, if present,
	// <meeting id> = record_id; externalId = meeting_id; a recording start
	// exists (providers.md §4.3 item 6). The attributes were not checked
	// against an events.xml from a packaged BBB.
	if ev.RecordingMeetingID != p.RecordID || (ev.MeetingID != "" && ev.MeetingID != p.RecordID) ||
		(p.MeetingID != "" && ev.ExternalID != p.MeetingID) {
		return &Violation{"V-S01", ReasonEventsMismatch}
	}
	// The start needs its timestampUTC: it is origin_utc of the time base,
	// so normalize can use every package that passes here.
	if start, ok := ev.FirstStart(); !ok || start.UTC == 0 {
		return &Violation{"V-S01", ReasonEventsMismatch}
	}
	// V-S02: at least one microphone publish event.
	if len(ev.MicPublished()) == 0 {
		return &Violation{"V-S02", ReasonUnsupportedBridge}
	}
	// V-S03: no VOICE StartRecordingEvent (legacy mixed audio).
	if ev.VoiceStarts > 0 {
		return &Violation{"V-S03", ReasonUnsupportedBridge}
	}
	// V-S04: the manifest metadata equals the selection the exporter makes
	// from /recording/metadata (meeting-package.md §4).
	if p.Metadata != nil && !maps.Equal(p.Metadata, SelectMetadata(ev.Metadata, p.MetaPrefix)) {
		return &Violation{"V-S04", ReasonMetadataMismatch}
	}
	return nil
}

// SelectMetadata returns the keys of events.xml metadata that a manifest
// carries: those starting with "<prefix>-", plus bbb-origin and
// bbb-origin-server-name if present (meeting-package.md §4).
func SelectMetadata(meta map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, val := range meta {
		if strings.HasPrefix(k, prefix+"-") || k == originMetaKey || k == originServerNameMetaKey {
			out[k] = val
		}
	}
	return out
}

// matchMicrophones is V-S05: each microphone file needs an
// AudioTrackPublishedEvent with the same basename (generators/audio.rb:137-145
// @ 7d27e7e). Unmatched files are returned as gaps; if none matches, the job
// fails.
func matchMicrophones(ev *events.Events, mics []string) ([]string, *Violation) {
	published := ev.MicPublished()
	gaps := []string{}
	for _, p := range mics {
		if _, ok := published[path.Base(p)]; !ok {
			gaps = append(gaps, p)
		}
	}
	if len(gaps) == len(mics) {
		return nil, &Violation{"V-S05", ReasonAllTracksFailed}
	}
	return gaps, nil
}

// String is for tests and DEBUG logs; it holds codes only.
func (v *Violation) String() string { return fmt.Sprintf("%s %s/%s", v.Rule, v.JobState(), v.Reason) }
