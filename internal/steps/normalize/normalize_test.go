package normalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
)

const transcripts = "../../../testdata/transcripts"

// caseDir builds the input of a testdata/transcripts case in a temp dir, as
// contracts/transcript does: F-01's raw files, the case's events.xml,
// renames and replaced files. testdata/ is only read.
func caseDir(t *testing.T, name string) string {
	t.Helper()
	var c struct {
		Events *string `json:"events"`
		Rename []struct {
			From, To string
		} `json:"rename"`
		Replace []struct {
			Path, With string
		} `json:"replace"`
	}
	b, err := os.ReadFile(filepath.Join(transcripts, name, "case.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(f01Raw))
	err = filepath.WalkDir(f01Raw, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f01Raw, p)
		switch {
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	copyIn := func(from, to string) {
		b, err := os.ReadFile(filepath.Join(transcripts, name, from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.FromSlash(to)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if c.Events != nil {
		copyIn(*c.Events, "events.xml")
	}
	for _, r := range c.Rename {
		if err := os.Rename(filepath.Join(dst, filepath.FromSlash(r.From)), filepath.Join(dst, filepath.FromSlash(r.To))); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range c.Replace {
		copyIn(r.With, r.Path)
	}
	return dst
}

// normalizeDir runs the allow-list and Normalize on a directory.
func normalizeDir(t *testing.T, dir string) (*meeting.Meeting, []byte) {
	t.Helper()
	sel, err := manifest.Select(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Normalize(os.DirFS(dir), sel.Files)
	if err != nil {
		t.Fatal(err)
	}
	b, err := meeting.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, b
}

type golden struct {
	TimeBase struct {
		OriginEventTS int64  `json:"origin_event_ts"`
		OriginUTC     string `json:"origin_utc"`
	} `json:"time_base"`
	RecordingPeriods []meeting.Period `json:"recording_periods"`
	Tracks           []struct {
		R          string `json:"ref"`
		S          string `json:"speaker_ref"`
		BlobSHA256 string `json:"blob_sha256"`
		StartMS    int64  `json:"start_ms"`
		EndMS      int64  `json:"end_ms"`
	} `json:"tracks"`
	Gaps []struct {
		TrackRef string `json:"track_ref"`
		Reason   string `json:"reason"`
	} `json:"gaps"`
}

const trackToleranceMS = 1000 // providers.md §4.3 item 4

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestNormalizeGoldens compares origin, recording periods, track spans,
// speaker grouping and track_unreadable gaps with the golden transcripts.
func TestNormalizeGoldens(t *testing.T) {
	for _, name := range []string{"F-01", "F-01-rejoin", "F-01-gaps", "F-01-clip"} {
		t.Run(name, func(t *testing.T) {
			var g golden
			b, err := os.ReadFile(filepath.Join(transcripts, name, "transcript.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &g); err != nil {
				t.Fatal(err)
			}
			dir := caseDir(t, name)
			m, out := normalizeDir(t, dir)
			if _, again := normalizeDir(t, dir); !bytes.Equal(out, again) {
				t.Fatal("not byte-identical on a re-run")
			}

			if m.TimeBase.OriginEventTS != g.TimeBase.OriginEventTS || m.TimeBase.OriginUTC != g.TimeBase.OriginUTC {
				t.Errorf("time base %+v, golden %+v", m.TimeBase, g.TimeBase)
			}
			if len(m.RecordingPeriods) != len(g.RecordingPeriods) {
				t.Fatalf("periods %+v, golden %+v", m.RecordingPeriods, g.RecordingPeriods)
			}
			for i := range g.RecordingPeriods {
				if m.RecordingPeriods[i] != g.RecordingPeriods[i] {
					t.Errorf("period %d = %+v, golden %+v", i, m.RecordingPeriods[i], g.RecordingPeriods[i])
				}
			}

			gapOf := map[string]string{}
			for _, gp := range g.Gaps {
				gapOf[gp.TrackRef] = gp.Reason
			}
			if len(m.Tracks) != len(g.Tracks) {
				t.Fatalf("%d tracks, golden %d", len(m.Tracks), len(g.Tracks))
			}
			speakerOf := map[string]string{} // golden speaker → participant
			for i, gt := range g.Tracks {
				nt := m.Tracks[i] // both in start order
				if nt.SHA256 != gt.BlobSHA256 {
					t.Fatalf("track %d: sha256 %s, golden %s", i, nt.SHA256, gt.BlobSHA256)
				}
				if abs(nt.StartMS-gt.StartMS) > trackToleranceMS || abs(nt.EndMS-gt.EndMS) > trackToleranceMS {
					t.Errorf("track %s: span %d–%d, golden %d–%d", gt.R, nt.StartMS, nt.EndMS, gt.StartMS, gt.EndMS)
				}
				// Only track_unreadable gaps come from normalize.
				want := ""
				if gapOf[gt.R] == meeting.GapTrackUnreadable {
					want = meeting.GapTrackUnreadable
				}
				if nt.Gap != want {
					t.Errorf("track %s: gap %q, want %q", gt.R, nt.Gap, want)
				}
				if p, ok := speakerOf[gt.S]; ok && p != nt.Participant {
					t.Errorf("track %s: participant %s, but %s has %s", gt.R, nt.Participant, gt.S, p)
				}
				speakerOf[gt.S] = nt.Participant
			}
			if len(speakerOf) != 2 || speakerOf["spk-1"] == speakerOf["spk-2"] {
				t.Errorf("speaker grouping %v", speakerOf)
			}
			for _, p := range m.Participants {
				if p.DisplayName == "" {
					t.Errorf("participant %s without a name", p.Ref)
				}
			}
		})
	}
}

// TestNormalizeUnmatched pins the handling of an unmatched microphone file
// (providers.md §7) on an
// M-11-like mutation: Ada's A1 file renamed so no publish event matches.
func TestNormalizeUnmatched(t *testing.T) {
	dir := caseDir(t, "F-01")
	from := filepath.Join(dir, "audio", "microphone-w_fixada0001-TR_fixA1-1761732005200.webm")
	to := filepath.Join(dir, "audio", "microphone-w_fixada0001-TR_fixM11-1761732005200.webm")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	m, _ := normalizeDir(t, dir)
	tr := m.Tracks[0]
	if tr.Path != "audio/microphone-w_fixada0001-TR_fixM11-1761732005200.webm" || tr.Gap != meeting.GapTrackUnreadable ||
		tr.Span != meeting.SpanFilename || tr.StartMS != 200 || tr.EndMS != 200+*tr.DurationMS || tr.Participant != "p-1" {
		t.Fatalf("unmatched track %+v", tr)
	}
	if len(m.Participants) != 2 {
		t.Fatalf("participants %+v", m.Participants)
	}
}

func TestNormalizeNoRecordingStart(t *testing.T) {
	dir := caseDir(t, "F-01")
	b, err := os.ReadFile(filepath.Join(dir, "events.xml"))
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte(`eventname="RecordStatusEvent"`), []byte(`eventname="Other"`))
	if err := os.WriteFile(filepath.Join(dir, "events.xml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	sel, err := manifest.Select(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(os.DirFS(dir), sel.Files); !errors.Is(err, ErrEvents) {
		t.Fatalf("err = %v, want ErrEvents", err)
	}
}

func TestPeriods(t *testing.T) {
	// start, start (ignored), stop, stop (ignored), start without stop
	// (ends at the last event).
	ev := eventsWith([]int64{10, 20, 30, 40, 50}, []bool{true, true, false, false, true}, 70)
	got := periods(ev, 10)
	want := []meeting.Period{
		{StartMS: 0, EndMS: 20, RecordedOffsetMS: 0},
		{StartMS: 40, EndMS: 60, RecordedOffsetMS: 20},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("periods %+v, want %+v", got, want)
	}
}

// unmatchedTrack parses the file name from the right; names that do not
// parse get a participant each.
func TestUnmatchedTrackNames(t *testing.T) {
	start := func(p string) (meeting.Track, string) {
		tr := meeting.Track{Path: "audio/" + p}
		key := unmatchedTrack(&tr, eventsMark(1761732005000))
		return tr, key
	}
	tr, key := start("microphone-w_a-b_1-TR_x-1761732005200.webm")
	if key != "w_a-b" || tr.StartMS != 200 || tr.Gap != meeting.GapTrackUnreadable || tr.Span != meeting.SpanFilename {
		t.Errorf("dashed userId: key %q, track %+v", key, tr)
	}
	_, k1 := start("microphone-garbage.webm")
	_, k2 := start("microphone-other-garbage.webm")
	if k1 == k2 || k1 == "" {
		t.Errorf("unparsable names share key %q / %q", k1, k2)
	}
	if tr, _ := start("microphone-u-t-notanumber.webm"); tr.StartMS != 0 {
		t.Errorf("bad epoch: %+v", tr)
	}
}

// notes sets the media type from the export it picks.
func TestNotesMediaType(t *testing.T) {
	md := manifest.File{Path: "notes/notes.md", SHA256: "a", Role: manifest.RoleNotes}
	html := manifest.File{Path: "notes/notes.html", SHA256: "b", Role: manifest.RoleNotes}
	failed := manifest.File{Path: "notes/export_failed", SHA256: "c", Role: manifest.RoleNotes}
	for _, c := range []struct {
		name  string
		files []manifest.File
		want  *meeting.FileRef
	}{
		{"md", []manifest.File{html, md}, &meeting.FileRef{Path: md.Path, SHA256: "a", MediaType: "text/markdown"}},
		{"html", []manifest.File{html}, &meeting.FileRef{Path: html.Path, SHA256: "b", MediaType: "text/html"}},
		{"none", nil, nil},
		{"export_failed", []manifest.File{md, failed}, nil},
	} {
		got := notes(c.files)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: notes = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Every track carries its media type and kind, and the JSON of F-01 has
// them; roles are the neutral ones.
func TestTrackMediaTypeAndKind(t *testing.T) {
	m, b := normalizeDir(t, caseDir(t, "F-01"))
	if m.Version != 3 {
		t.Errorf("version %d, want 3", m.Version)
	}
	for _, tr := range m.Tracks {
		if tr.MediaType != "audio/webm" || tr.Kind != "microphone" {
			t.Errorf("track %s: media_type %q, kind %q", tr.Path, tr.MediaType, tr.Kind)
		}
	}
	if !bytes.Contains(b, []byte(`"kind":"microphone","media_type":"audio/webm"`)) {
		t.Errorf("JSON lacks kind and media_type: %.300s", b)
	}
	// F-01's events.xml has one MODERATOR and one VIEWER.
	roles := map[string]int{}
	for _, p := range m.Participants {
		roles[p.Role]++
	}
	if len(roles) != 2 || roles["moderator"] != 1 || roles["viewer"] != 1 {
		t.Errorf("roles %v, want one moderator and one viewer", roles)
	}
}

func TestRole(t *testing.T) {
	for in, want := range map[string]string{
		"MODERATOR": "moderator",
		"VIEWER":    "viewer",
		"PRESENTER": "",
		"":          "",
	} {
		if got := role(in); got != want {
			t.Errorf("role(%q) = %q, want %q", in, got, want)
		}
	}
}

// span is a source diagnostic: it is left out of the JSON when empty.
func TestSpanOmittedWhenEmpty(t *testing.T) {
	m := &meeting.Meeting{Tracks: []meeting.Track{{Path: "a.webm"}}}
	b, err := meeting.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"span"`)) {
		t.Errorf("empty span in JSON: %s", b)
	}
	m.Tracks[0].Span = meeting.SpanEvents
	if b, _ = meeting.Marshal(m); !bytes.Contains(b, []byte(`"span":"events"`)) {
		t.Errorf("span missing: %s", b)
	}
}
