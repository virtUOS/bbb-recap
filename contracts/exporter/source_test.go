package exporter

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSourceChanges pins §4.2 of docs/reference/bbb-exporter.md: files are read in
// place, and a file that changes (fixtures.md M-01) or disappears (M-02)
// between build and upload fails the record.
func TestSourceChanges(t *testing.T) {
	t.Run("CT-EXP-06", func(t *testing.T) {
		t.Run("M-01 byte flipped", func(t *testing.T) {
			sourceMutation(t, "source_changed", func(path string) {
				b := readFile(t, path)
				b[len(b)/2] ^= 0xff
				if err := os.WriteFile(path, b, 0o644); err != nil {
					t.Fatal(err)
				}
			})
		})
		t.Run("M-02 file deleted", func(t *testing.T) {
			sourceMutation(t, "source_missing", func(path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			})
		})
	})
}

// sourceMutation holds the first manifest upload (the manifest is built and
// persisted by then), changes Ada's first microphone file, then lets the
// upload continue.
func sourceMutation(t *testing.T, reason string, mutate func(path string)) {
	e := newExporterEnv(t, func(kind string, n int) *fault {
		if kind == kindManifest && n == 1 {
			return &fault{hold: true}
		}
		return nil
	})
	recordID, raw := e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()
	e.ingest.waitArrived(kindManifest, stateTimeout, d.running)
	mutate(filepath.Join(raw, "audio", "microphone-w_fixada0001-TR_fixA1-1761732005200.webm"))
	e.ingest.releaseOne()

	e.waitFinal(d, recordID, "failed", reason)
	d.stop()
	if n := e.ingest.count(kindCommit); n != 0 {
		t.Errorf("commit sent %d times after the source changed", n)
	}
	if !exists(t, filepath.Join(raw, "events.xml")) {
		t.Errorf("raw dir deleted")
	}
}
