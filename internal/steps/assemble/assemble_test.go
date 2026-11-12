package assemble_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
	"github.com/virtUOS/bbb-recap/internal/steps/validate/validatetest"
)

const transcripts = "../../../testdata/transcripts"

// f01Case is the assemble part of testdata/transcripts/F-01/case.json.
type f01Case struct {
	Assemble struct {
		JobID     string              `json:"job_id"`
		CreatedAt string              `json:"created_at"`
		ASR       []assemble.TrackASR `json:"asr"`
	} `json:"assemble"`
}

func loadF01Case(t *testing.T) f01Case {
	t.Helper()
	var c f01Case
	if err := json.Unmarshal(readFile(t, filepath.Join(transcripts, "F-01", "case.json")), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// f01Meeting normalizes F-01's raw directory.
func f01Meeting(t *testing.T) *meeting.Meeting {
	t.Helper()
	sel, err := manifest.Select(validatetest.F01Dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := normalize.Normalize(os.DirFS(validatetest.F01Dir), sel.Files)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func golden(t *testing.T, name string) assemble.Transcript {
	t.Helper()
	var g assemble.Transcript
	if err := json.Unmarshal(readFile(t, filepath.Join(transcripts, name, "transcript.json")), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func decode(t *testing.T, b []byte) assemble.Transcript {
	t.Helper()
	var tr assemble.Transcript
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestAssembleF01(t *testing.T) {
	c := loadF01Case(t)
	created, err := time.Parse(time.RFC3339, c.Assemble.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	in := assemble.Input{JobID: c.Assemble.JobID, CreatedAt: created, Meeting: f01Meeting(t), ASR: c.Assemble.ASR}
	out, err := assemble.Assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := assemble.Assemble(in)
	if err != nil || string(again) != string(out) {
		t.Fatalf("not byte-identical on a re-run: %v", err)
	}
	if got, want := decode(t, out), golden(t, "F-01"); !reflect.DeepEqual(got, want) {
		t.Errorf("transcript differs from the golden\n got %+v\nwant %+v", got, want)
	}
}

// The goldens pass Check; every file in invalid/ fails it.
func TestCheckFixtures(t *testing.T) {
	for _, name := range []string{"F-01", "F-01-rejoin", "F-01-gaps", "F-01-clip"} {
		if err := assemble.Check(readFile(t, filepath.Join(transcripts, name, "transcript.json"))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	invalid, err := filepath.Glob(filepath.Join(transcripts, "invalid", "*.json"))
	if err != nil || len(invalid) == 0 {
		t.Fatalf("no invalid fixtures: %v", err)
	}
	for _, p := range invalid {
		if assemble.Check(readFile(t, p)) == nil {
			t.Errorf("%s: Check accepts it", filepath.Base(p))
		}
	}
	if assemble.Check([]byte("not json")) == nil {
		t.Error("Check accepts non-JSON")
	}
}

// fixtureMeeting is a small normalized meeting: one period 0–10000, two tracks of
// two participants.
func fixtureMeeting() *meeting.Meeting {
	return &meeting.Meeting{
		Version:          meeting.Version,
		TimeBase:         meeting.TimeBase{OriginEventTS: 1000, OriginUTC: "2025-10-29T10:00:00.000Z"},
		RecordingPeriods: []meeting.Period{{StartMS: 0, EndMS: 10000}},
		Participants:     []meeting.Participant{{Ref: "p-1", DisplayName: "Una", Role: "moderator"}, {Ref: "p-2"}},
		Tracks: []meeting.Track{
			{Path: "audio/a.webm", SHA256: strings.Repeat("a", 64), MediaType: "audio/webm", Kind: "microphone",
				Participant: "p-2", StartMS: 100, EndMS: 9000},
			{Path: "audio/b.webm", SHA256: strings.Repeat("b", 64), MediaType: "audio/webm", Kind: "microphone",
				Participant: "p-1", StartMS: 100, EndMS: 9000},
		},
	}
}

func result(lang string, segs ...providers.Segment) *providers.AsrResult {
	r := &providers.AsrResult{Model: "m", Segments: segs}
	if lang != "" {
		r.Language = &lang
	}
	return r
}

func asr(path string, r *providers.AsrResult) assemble.TrackASR {
	return assemble.TrackASR{Path: path, Profile: "p", Dialect: "d", Result: r}
}

const jobID = "019a2f8c-3b40-7000-8000-000000000001"

// Speakers by first appearance in track order; a participant without a
// join event has no name; equal starts sort by track; a language tie goes to
// the code that sorts first; text is trimmed and empty text dropped.
func TestAssembleSmall(t *testing.T) {
	in := assemble.Input{JobID: jobID, CreatedAt: time.Date(2025, 10, 29, 12, 0, 0, 0, time.FixedZone("x", 7200)),
		Meeting: fixtureMeeting(), ASR: []assemble.TrackASR{
			asr("audio/a.webm", result("en", providers.Segment{StartMS: 0, EndMS: 1000, Text: " one "},
				providers.Segment{StartMS: 2000, EndMS: 2500, Text: "  "})),
			asr("audio/b.webm", result("de", providers.Segment{StartMS: 0, EndMS: 1000, Text: "zwei"})),
		}}
	out, err := assemble.Assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	tr := decode(t, out)
	if tr.CreatedAt != "2025-10-29T10:00:00Z" {
		t.Errorf("created_at %s", tr.CreatedAt)
	}
	wantSpk := []assemble.Speaker{{Ref: "spk-1"}, {Ref: "spk-2", DisplayName: "Una", Role: "moderator"}}
	if !reflect.DeepEqual(tr.Speakers, wantSpk) {
		t.Errorf("speakers %+v", tr.Speakers)
	}
	if len(tr.Segments) != 2 || tr.Segments[0].TrackRef != "trk-1" || tr.Segments[0].Text != "one" ||
		tr.Segments[1].TrackRef != "trk-2" || tr.Segments[1].ID != "seg-000002" {
		t.Errorf("segments %+v", tr.Segments)
	}
	if tr.Language == nil || *tr.Language != "de" {
		t.Errorf("language %v, want de (tie)", tr.Language)
	}
	for _, trk := range tr.Tracks {
		if trk.Kind != "microphone" {
			t.Errorf("track %s: kind %q", trk.Ref, trk.Kind)
		}
	}
}

// kind and role are copied from the normalized meeting: a track without a
// kind fails the schema check, and a role outside moderator and viewer is
// an error, not dropped.
func TestAssembleCopiesKindAndRole(t *testing.T) {
	ok := []assemble.TrackASR{asr("audio/a.webm", result("en")), asr("audio/b.webm", result("en"))}
	m := fixtureMeeting()
	m.Participants[1].Role = "viewer"
	out, err := assemble.Assemble(assemble.Input{JobID: jobID, Meeting: m, ASR: ok})
	if err != nil {
		t.Fatal(err)
	}
	wantSpk := []assemble.Speaker{{Ref: "spk-1", Role: "viewer"}, {Ref: "spk-2", DisplayName: "Una", Role: "moderator"}}
	if tr := decode(t, out); !reflect.DeepEqual(tr.Speakers, wantSpk) {
		t.Errorf("speakers %+v", tr.Speakers)
	}

	m = fixtureMeeting()
	m.Tracks[0].Kind = ""
	if _, err := assemble.Assemble(assemble.Input{JobID: jobID, Meeting: m, ASR: ok}); err == nil {
		t.Error("track without kind: no error")
	}
	for _, role := range []string{"MODERATOR", "presenter"} {
		m := fixtureMeeting()
		m.Participants[0].Role = role
		_, err := assemble.Assemble(assemble.Input{JobID: jobID, Meeting: m, ASR: ok})
		if err == nil || !strings.Contains(err.Error(), "unknown role") {
			t.Errorf("role %q: err = %v, want unknown role", role, err)
		}
	}
}

// Clipping by midpoint, empty and failed tracks, gaps with the track span,
// and language null when no track reported one.
func TestAssembleClipAndGaps(t *testing.T) {
	m := fixtureMeeting()
	m.RecordingPeriods = []meeting.Period{{StartMS: 0, EndMS: 3000}, {StartMS: 6000, EndMS: 9000, RecordedOffsetMS: 3000}}
	in := assemble.Input{JobID: jobID, CreatedAt: time.Unix(0, 0), Meeting: m, ASR: []assemble.TrackASR{
		// on the axis: 2100–3900 (midpoint 3000, kept) and 3200–5800 (midpoint 4500, dropped)
		asr("audio/a.webm", result("", providers.Segment{StartMS: 2000, EndMS: 3800, Text: "kept"},
			providers.Segment{StartMS: 3100, EndMS: 5700, Text: "dropped"})),
		{Path: "audio/b.webm", Gap: "asr_rejected_input"},
	}}
	out, err := assemble.Assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	tr := decode(t, out)
	if len(tr.Segments) != 1 || tr.Segments[0].Text != "kept" {
		t.Errorf("segments %+v", tr.Segments)
	}
	if tr.Tracks[0].Status != assemble.StatusTranscribed || tr.Tracks[0].Language != "" ||
		tr.Tracks[1].Status != assemble.StatusFailed || tr.Tracks[1].Provider != nil {
		t.Errorf("tracks %+v", tr.Tracks)
	}
	want := []assemble.Gap{{TrackRef: "trk-2", SpeakerRef: "spk-2", StartMS: 100, EndMS: 9000, Reason: "asr_rejected_input"}}
	if !reflect.DeepEqual(tr.Gaps, want) {
		t.Errorf("gaps %+v", tr.Gaps)
	}
	if tr.Language != nil {
		t.Errorf("language %v, want null", *tr.Language)
	}

	// Only dropped segments: the track is empty but keeps language and provider.
	in.ASR[0] = asr("audio/a.webm", result("en", providers.Segment{StartMS: 3100, EndMS: 5700, Text: "dropped"}))
	out, err = assemble.Assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	if tr := decode(t, out); tr.Tracks[0].Status != assemble.StatusEmpty || tr.Tracks[0].Language != "en" ||
		tr.Tracks[0].Provider == nil || *tr.Language != "en" {
		t.Errorf("empty track %+v, language %v", tr.Tracks[0], tr.Language)
	}
}

// Inputs that do not fit the normalized meeting are errors.
func TestAssembleBadInput(t *testing.T) {
	ok := []assemble.TrackASR{asr("audio/a.webm", result("en")), asr("audio/b.webm", result("en"))}
	for name, list := range map[string][]assemble.TrackASR{
		"missing track": ok[:1],
		"unknown path":  append(append([]assemble.TrackASR{}, ok...), asr("audio/c.webm", result("en"))),
		"duplicate":     {ok[0], ok[0], ok[1]},
		"bad gap":       {ok[0], {Path: "audio/b.webm", Gap: "track_unreadable"}},
		"neither":       {ok[0], {Path: "audio/b.webm"}},
	} {
		if _, err := assemble.Assemble(assemble.Input{JobID: jobID, Meeting: fixtureMeeting(), ASR: list}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	m := fixtureMeeting()
	m.Tracks[1].Gap = meeting.GapTrackUnreadable
	if _, err := assemble.Assemble(assemble.Input{JobID: jobID, Meeting: m, ASR: ok}); err == nil {
		t.Error("ASR outcome for a normalize gap: no error")
	}
	// A bad job ID is caught by the schema check.
	if _, err := assemble.Assemble(assemble.Input{JobID: "nope", Meeting: fixtureMeeting(), ASR: ok}); err == nil {
		t.Error("bad job_id: no error")
	}
}
