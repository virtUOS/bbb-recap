package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

const (
	testdata    = "../../../testdata"
	f01RecordID = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	f02RecordID = "31a34943556bbcf808018ef8494d5ff77c514840-1761732000000"
)

// f01 returns F-01's golden manifest and an in-memory package with its
// events.xml and (empty) microphone files: the rules read only events.xml.
func f01(t *testing.T) (*manifest.Manifest, fstest.MapFS) {
	t.Helper()
	var m manifest.Manifest
	b, err := os.ReadFile(filepath.Join(testdata, "meetings/F-01/expected/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m, packageFS(t, filepath.Join(testdata, "meetings/F-01/raw", f01RecordID), m.Files)
}

func packageFS(t *testing.T, dir string, files []manifest.File) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	ev, err := os.ReadFile(filepath.Join(dir, "events.xml"))
	if err != nil {
		t.Fatal(err)
	}
	fsys["events.xml"] = &fstest.MapFile{Data: ev}
	for _, f := range files {
		if f.Role == manifest.RoleMicrophone {
			fsys[f.Path] = &fstest.MapFile{}
		}
	}
	return fsys
}

func pkgOf(m *manifest.Manifest) Package {
	p := Package{RecordID: m.RecordID, MeetingID: m.MeetingID, Metadata: m.Metadata, MetaPrefix: m.MetaPrefix}
	for _, f := range m.Files {
		if f.Role == manifest.RoleMicrophone {
			p.Microphones = append(p.Microphones, f.Path)
		}
	}
	return p
}

func wantViolation(t *testing.T, v *Violation, err error, rule, reason string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || v.Rule != rule || v.Reason != reason || v.JobState() != "failed" {
		t.Fatalf("violation = %v, want %s failed/%s", v, rule, reason)
	}
}

func TestRulesF01(t *testing.T) {
	m, fsys := f01(t)
	gaps, v, err := Rules(fsys, pkgOf(m))
	if err != nil || v != nil || len(gaps) != 0 || gaps == nil {
		t.Fatalf("gaps %q, violation %v, err %v", gaps, v, err)
	}
	// Raw form: no meeting_id, no metadata.
	gaps, v, err = Rules(fsys, Package{RecordID: f01RecordID, Microphones: pkgOf(m).Microphones})
	if err != nil || v != nil || len(gaps) != 0 {
		t.Fatalf("raw form: gaps %q, violation %v, err %v", gaps, v, err)
	}
}

func TestRulesF02(t *testing.T) {
	fsys := packageFS(t, filepath.Join(testdata, "meetings/F-02/raw", f02RecordID), nil)
	_, v, err := Rules(fsys, Package{RecordID: f02RecordID})
	wantViolation(t, v, err, "V-S02", ReasonUnsupportedBridge)
}

func TestRulesV01(t *testing.T) {
	m, fsys := f01(t)
	// M-10: externalId differs from the manifest's meeting_id.
	p := pkgOf(m)
	p.MeetingID = "fixture-meeting-m10"
	_, v, err := Rules(fsys, p)
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)

	// <meeting id> differs from record_id.
	p = pkgOf(m)
	p.RecordID = "other"
	_, v, err = Rules(fsys, p)
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)

	// /recording/@meeting_id must equal record_id; <meeting id> only if present.
	orig := string(fsys["events.xml"].Data)
	withEvents := func(doc string) fstest.MapFS {
		return fstest.MapFS{"events.xml": {Data: []byte(doc)}}
	}
	rootID := `<recording meeting_id="` + f01RecordID + `"`
	meetingID := `<meeting id="` + f01RecordID + `"`
	_, v, err = Rules(withEvents(strings.Replace(orig, rootID, `<recording meeting_id="other"`, 1)), pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)
	_, v, err = Rules(withEvents(strings.Replace(orig, meetingID, `<meeting id="other"`, 1)), pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)
	if !strings.Contains(orig, rootID) || !strings.Contains(orig, meetingID) {
		t.Fatal("fixture: attributes not found")
	}
	if _, v, err = Rules(withEvents(strings.Replace(orig, meetingID, `<meeting`, 1)), pkgOf(m)); err != nil || v != nil {
		t.Fatalf("<meeting> without id: violation %v, err %v", v, err)
	}

	// A recording start without timestampUTC (no origin_utc for normalize).
	noUTC := strings.Replace(orig, "<timestampUTC>1761732005000</timestampUTC>", "", 1)
	if noUTC == orig {
		t.Fatal("fixture: start mark timestampUTC not found")
	}
	_, v, err = Rules(withEvents(noUTC), pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)

	// Malformed XML and a missing events.xml.
	bad := fstest.MapFS{"events.xml": {Data: []byte("<recording>")}}
	_, v, err = Rules(bad, pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)
	_, v, err = Rules(fstest.MapFS{}, pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)

	// No recording start (providers.md §4.3 item 6).
	noMarks := strings.ReplaceAll(string(fsys["events.xml"].Data), `eventname="RecordStatusEvent"`, `eventname="X"`)
	_, v, err = Rules(fstest.MapFS{"events.xml": {Data: []byte(noMarks)}}, pkgOf(m))
	wantViolation(t, v, err, "V-S01", ReasonEventsMismatch)
}

func TestRulesV03(t *testing.T) {
	m, fsys := f01(t)
	withVoice := strings.Replace(string(fsys["events.xml"].Data), "</recording>",
		`<event timestamp="5005000" module="VOICE" eventname="StartRecordingEvent"><filename>x.opus</filename></event></recording>`, 1)
	fsys["events.xml"] = &fstest.MapFile{Data: []byte(withVoice)}
	_, v, err := Rules(fsys, pkgOf(m))
	wantViolation(t, v, err, "V-S03", ReasonUnsupportedBridge)
}

func TestRulesV04(t *testing.T) {
	// M-09: manifest metadata differs from events.xml.
	m, fsys := f01(t)
	p := pkgOf(m)
	p.Metadata = map[string]string{}
	for k, v := range m.Metadata {
		p.Metadata[k] = v
	}
	p.Metadata["recap-sink"] = "docs-m09"
	_, v, err := Rules(fsys, p)
	wantViolation(t, v, err, "V-S04", ReasonMetadataMismatch)

	// A key missing from the manifest is a mismatch too.
	delete(p.Metadata, "recap-sink")
	_, v, err = Rules(fsys, p)
	wantViolation(t, v, err, "V-S04", ReasonMetadataMismatch)
}

func TestRulesV05(t *testing.T) {
	// M-11: one renamed microphone file becomes a gap.
	m, fsys := f01(t)
	p := pkgOf(m)
	const renamed = "audio/microphone-w_fixada0001-TR_fixM11-1761732005200.webm"
	p.Microphones[0] = renamed
	gaps, v, err := Rules(fsys, p)
	if err != nil || v != nil || !slices.Equal(gaps, []string{renamed}) {
		t.Fatalf("gaps %q, violation %v, err %v", gaps, v, err)
	}
	// None matches: all_tracks_failed.
	for i := range p.Microphones {
		p.Microphones[i] = strings.Replace(p.Microphones[i], "-TR_fix", "-TR_nomatch", 1)
	}
	_, v, err = Rules(fsys, p)
	wantViolation(t, v, err, "V-S05", ReasonAllTracksFailed)
}

func TestSelectMetadata(t *testing.T) {
	got := SelectMetadata(map[string]string{
		"recap-v": "1", "recapx": "no", "bbb-origin": "PILOS", "bbb-origin-server-name": "s", "meetingName": "n",
		"other-v": "1",
	}, "recap")
	if len(got) != 3 || got["recap-v"] != "1" || got["bbb-origin"] != "PILOS" || got["bbb-origin-server-name"] != "s" {
		t.Fatalf("got %v", got)
	}
}
