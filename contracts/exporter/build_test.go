package exporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuild pins the build step of the daemon (docs/reference/bbb-exporter.md §4.1
// steps 1-2, meeting-package.md §4-§8).
func TestBuild(t *testing.T) {
	t.Run("CT-EXP-02", func(t *testing.T) {
		t.Run("F-01 golden manifest and package_id", buildGoldenF01)
		t.Run("F-02 unsupported_audio_bridge", func(t *testing.T) { buildFails(t, "F-02", "unsupported_audio_bridge") })
		t.Run("F-10 no_microphone_tracks", func(t *testing.T) { buildFails(t, "F-10", "no_microphone_tracks") })
		t.Run("F-09 limit_exceeded", buildOversize)
		t.Run("F-03 opt-in re-checked at build", buildNotOptedIn)
		t.Run("F-01 events.xml removed after spooling", buildNoEvents)
	})
}

// buildGoldenF01: the manifest the daemon keeps in the state record equals the
// golden manifest once created_at and exporter.version are set to the values
// the goldens were made with (testdata/README.md). The test normalizes those
// two fields instead of injecting a clock.
func buildGoldenF01(t *testing.T) {
	e := newExporterEnv(t, nil)
	checkCanonicalHelper(t)
	recordID, _ := e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()

	s := e.waitState(d, recordID, "a built manifest", func(s stateRecord) bool {
		return s.State == "ready" || s.State == "delivered" || s.State == "acked" || s.State == "failed"
	})
	if s.State == "failed" {
		t.Fatalf("F-01 failed: %s\n%s", s.reason(), d.output())
	}
	recDir := filepath.Join(e.stateDir, recordID)
	got := readFile(t, filepath.Join(recDir, "manifest.json"))
	if !exists(t, filepath.Join(recDir, "spool.json")) {
		t.Errorf("spool.json missing next to state.json (§5.3)")
	}
	if exists(t, filepath.Join(e.spoolDir, recordID+".json")) {
		t.Errorf("spool entry was not claimed (§4.1 step 1)")
	}

	// The file is canonical and hashes to the package_id (meeting-package.md §4).
	v, err := decodeJSON(got)
	if err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	canon, err := canonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canon, got) {
		t.Errorf("manifest.json is not JCS-canonical:\n got %s\nwant %s", got, canon)
	}
	sum := sha256.Sum256(got)
	if s.PackageID != hex.EncodeToString(sum[:]) {
		t.Errorf("state.json package_id %q, sha256(manifest.json) %x", s.PackageID, sum)
	}

	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("manifest is not an object")
	}
	created, _ := m["created_at"].(string)
	if ts, err := time.Parse(time.RFC3339Nano, created); err != nil || !strings.HasSuffix(created, "Z") || ts.Location() != time.UTC {
		t.Errorf("created_at %q is not RFC 3339 UTC", created)
	}
	exp, _ := m["exporter"].(map[string]any)
	if exp["name"] != "recap-exporter" {
		t.Errorf("exporter.name = %v, want recap-exporter", exp["name"])
	}
	if ver, _ := exp["version"].(string); ver == "" {
		t.Errorf("exporter.version is empty")
	}

	m["created_at"] = fixtureCreatedAt
	if exp != nil {
		exp["version"] = fixtureVersion
	}
	norm, err := canonicalJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	golden := readFile(t, filepath.Join(testdataDir(t), "meetings", "F-01", "expected", "manifest.json"))
	if !bytes.Equal(norm, golden) {
		t.Errorf("manifest differs from the golden (after setting created_at, exporter.version):\n got %s\nwant %s", norm, golden)
	}
	goldenID := strings.TrimSpace(string(readFile(t, filepath.Join(testdataDir(t), "meetings", "F-01", "expected", "package_id.txt"))))
	sum = sha256.Sum256(norm)
	if hex.EncodeToString(sum[:]) != goldenID {
		t.Errorf("package_id of the normalized manifest %x, golden %s", sum, goldenID)
	}
	d.stop()
}

// buildFails: V-E01…V-E03 → failed with a reason, nothing uploaded (§5.1).
func buildFails(t *testing.T, fixture, reason string) {
	e := newExporterEnv(t, nil)
	recordID, _ := e.addRecording(fixture)
	e.spool(recordID)
	d := e.start()
	e.waitFinal(d, recordID, "failed", reason)
	assertNoRequests(t, e)
	d.stop()
}

// buildOversize is F-09: F-01 plus a sparse 3 GiB file in presentation/,
// above the 2 GiB per-file limit (meeting-package.md §8, fixtures.md §3).
func buildOversize(t *testing.T) {
	e := newExporterEnv(t, nil)
	recordID, dir := e.addRecording("F-01")
	big := filepath.Join(dir, "presentation", "oversize.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(3 << 30); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	e.spool(recordID)
	d := e.start()
	e.waitFinal(d, recordID, "failed", "limit_exceeded")
	assertNoRequests(t, e)
	d.stop()
}

// buildNoEvents: V-E01 is checked before the opt-in re-check (§5.1), so a
// spooled recording whose events.xml is gone fails with no_events.
func buildNoEvents(t *testing.T) {
	e := newExporterEnv(t, nil)
	recordID, dir := e.addRecording("F-01")
	e.spool(recordID)
	if err := os.Remove(filepath.Join(dir, "events.xml")); err != nil {
		t.Fatal(err)
	}
	d := e.start()
	e.waitFinal(d, recordID, "failed", "no_events")
	assertNoRequests(t, e)
	d.stop()
}

// buildNotOptedIn: a spool entry for a recording that is not opted in (F-03)
// is dropped at build: the record is deleted, nothing fails, nothing is sent
// (§4.1 step 2, §5.2).
func buildNotOptedIn(t *testing.T) {
	e := newExporterEnv(t, nil)
	recordID, raw := e.addRecording("F-03")
	e.spool(recordID)
	d := e.start()

	entry := filepath.Join(e.spoolDir, recordID+".json")
	rec := filepath.Join(e.stateDir, recordID)
	deadline := time.Now().Add(stateTimeout)
	for exists(t, entry) || exists(t, rec) {
		if !d.running() {
			t.Fatalf("exporter exited (%v)\n%s", d.err, d.output())
		}
		if time.Now().After(deadline) {
			s, _ := e.readState(recordID)
			t.Fatalf("spool entry or state record still there after %s (state %+v)\n%s", stateTimeout, s, d.output())
		}
		time.Sleep(200 * time.Millisecond)
	}
	assertNoRequests(t, e)
	if !exists(t, filepath.Join(raw, "events.xml")) {
		t.Errorf("raw dir changed")
	}
	d.stop()
}

func assertNoRequests(t *testing.T, e *exporterEnv) {
	t.Helper()
	if reqs := e.ingest.requests(); len(reqs) > 0 {
		t.Errorf("nothing may be uploaded, got requests %v", reqs)
	}
}
