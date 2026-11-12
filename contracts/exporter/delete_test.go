package exporter

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDeletion pins the deletion of raw data (docs/reference/bbb-exporter.md §7).
// Time is moved forward by backdating the state record while the daemon is
// stopped, instead of a clock seam. Completion
// markers come from markPublished.
func TestDeletion(t *testing.T) {
	t.Run("CT-EXP-05", func(t *testing.T) {
		t.Run("never deletes nothing", deleteNever)
		t.Run("after_ack waits for grace and BBB markers", deleteAfterAck)
		t.Run("failure expiry", deleteFailureExpiry)
		t.Run("refused when events.xml changed", deleteEventsChanged)
	})
}

// ackedF01 runs F-01 to acked with the given policy and stops the daemon.
func ackedF01(t *testing.T, e *exporterEnv, policy string) (recordID, raw string) {
	t.Helper()
	e.vars["RECAP_EXPORTER_DELETE_POLICY"] = policy
	recordID, raw = e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()
	s := e.waitState(d, recordID, "acked", func(s stateRecord) bool {
		return s.State == "acked" || s.State == "deleted" || s.State == "failed"
	})
	d.stop()
	if s.State != "acked" {
		t.Fatalf("got %s/%s right after the upload, want acked (a deletion here ignores the 24 h grace, §7)", s.State, s.reason())
	}
	return recordID, raw
}

func deleteNever(t *testing.T) {
	e := newExporterEnv(t, nil)
	recordID, raw := ackedF01(t, e, "never")
	e.backdate(recordID, 48*time.Hour)
	e.markPublished(recordID, "presentation")

	d := e.start()
	e.idle(d)
	d.stop()
	if !exists(t, filepath.Join(raw, "events.xml")) {
		t.Fatalf("policy never deleted the raw dir")
	}
	if s, _ := e.readState(recordID); s.State != "acked" {
		t.Errorf("state %s, want acked", s.State)
	}
}

func deleteAfterAck(t *testing.T) {
	e := newExporterEnv(t, nil)
	e.vars["RECAP_EXPORTER_RECORDING_FORMATS"] = "presentation,video"
	// A sibling recording and a symlink target outside raw/<record_id>/: both
	// must survive (deletion removes only that dir and follows no symlinks).
	sibling := filepath.Join(e.rawDir, "0000000000000000000000000000000000000000-1761732000000")
	outside := filepath.Join(e.dir, "outside", "elsewhere.webm")
	for _, p := range []string{filepath.Join(sibling, "events.xml"), outside} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// BBB is done with every listed format from the start, so only the grace
	// (§7 condition 3) can hold the deletion back in the first two steps.
	e.markPublished(f01RecordID, "presentation", "video")
	recordID, raw := ackedF01(t, e, "after_ack")
	link := filepath.Join(raw, "audio", "link-to-elsewhere.webm")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	// Just acked, markers complete: the 24 h grace has not passed.
	d := e.start()
	e.idle(d)
	d.stop()
	if !exists(t, raw) {
		t.Fatalf("raw dir deleted right after the ack, before the 24 h grace (§7 condition 3)")
	}

	// 23 h after the ack: still within the grace.
	e.backdate(recordID, 23*time.Hour)
	d = e.start()
	e.idle(d)
	d.stop()
	if !exists(t, raw) {
		t.Fatalf("raw dir deleted 23 h after the ack, before the 24 h grace (§7 condition 3)")
	}

	// 25 h after the ack, but one listed format has lost its marker.
	e.backdate(recordID, 2*time.Hour)
	videoMarker := filepath.Join(e.statusDir, "published", recordID+"-video.done")
	if err := os.Remove(videoMarker); err != nil {
		t.Fatal(err)
	}
	d = e.start()
	e.idle(d)
	d.stop()
	if !exists(t, raw) {
		t.Fatalf("raw dir deleted although format video has no completion marker (§7 condition 2)")
	}

	// 25 h after the ack and every listed format done: deleted.
	e.markPublished(recordID, "video")
	d = e.start()
	e.waitFinal(d, recordID, "deleted", "")
	d.stop()
	if exists(t, raw) {
		t.Errorf("state deleted, but %s still exists", raw)
	}
	if !exists(t, outside) {
		t.Errorf("symlink target outside the raw dir was deleted")
	}
	if !exists(t, filepath.Join(sibling, "events.xml")) {
		t.Errorf("another recording's raw dir was deleted")
	}
}

// deleteFailureExpiry: a record that stays failed, or keeps retrying in
// ready, for longer than RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS loses its
// raw dir with reason failure_expiry, but only once BBB is done with it
// (§7: condition 2 still applies).
func deleteFailureExpiry(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		slug          string
		state, reason string
	}{
		{"from failed", http.StatusUnauthorized, "unauthorized", "failed", "unauthorized"},
		{"from ready", http.StatusServiceUnavailable, "unavailable", "ready", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newExporterEnv(t, func(kind string, n int) *fault {
				if kind == kindManifest {
					return &fault{status: c.status, slug: c.slug}
				}
				return nil
			})
			e.vars["RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS"] = "1"
			recordID, raw := e.addRecording("F-01")
			e.spool(recordID)
			d := e.start()
			s := e.waitState(d, recordID, "the first failed upload", func(s stateRecord) bool {
				return s.State == "failed" || (s.Attempts >= 1 && s.NextAttemptAt != "")
			})
			d.stop()
			if s.State != c.state || s.reason() != c.reason {
				t.Fatalf("after the first upload: %s/%s, want %s/%s", s.State, s.reason(), c.state, c.reason)
			}

			// Expired, but BBB has not finished: kept.
			e.backdate(recordID, 49*time.Hour)
			d = e.start()
			e.idle(d)
			d.stop()
			if !exists(t, raw) {
				t.Fatalf("raw dir deleted by failure expiry without BBB completion markers (§7 condition 2)")
			}
			if s, _ := e.readState(recordID); s.State == "deleted" {
				t.Fatalf("state deleted without BBB completion markers")
			}

			// Expired and BBB done: deleted.
			e.markPublished(recordID, "presentation")
			d = e.start()
			e.waitFinal(d, recordID, "deleted", "failure_expiry")
			d.stop()
			if exists(t, raw) {
				t.Errorf("state deleted/failure_expiry, but %s still exists", raw)
			}
		})
	}
}

func deleteEventsChanged(t *testing.T) {
	e := newExporterEnv(t, nil)
	recordID, raw := ackedF01(t, e, "after_ack")
	e.backdate(recordID, 25*time.Hour)
	e.markPublished(recordID, "presentation")
	events := filepath.Join(raw, "events.xml")
	f, err := os.OpenFile(events, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("<!-- changed after ack -->\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	d := e.start()
	e.waitFinal(d, recordID, "failed", "source_changed")
	d.stop()
	if !exists(t, events) {
		t.Errorf("raw dir deleted although events.xml no longer matches the manifest")
	}
}
