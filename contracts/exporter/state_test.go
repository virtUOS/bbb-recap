package exporter

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// TestStateMachine pins the transitions of docs/reference/bbb-exporter.md §5.1 and
// the ack verification of §6.3, against the fake ingest service.
func TestStateMachine(t *testing.T) {
	t.Run("CT-EXP-03", func(t *testing.T) {
		t.Run("F-01 without kills", func(t *testing.T) { stateHappyPath(t) })
		t.Run("F-01 killed at every persisted step", stateKillRestart)
		t.Run("transient 503", func(t *testing.T) {
			stateTransient(t, http.StatusServiceUnavailable, "", 24*time.Second, 36*time.Second)
		})
		t.Run("transient 429 with Retry-After", func(t *testing.T) {
			stateTransient(t, http.StatusTooManyRequests, "300", 300*time.Second, 0)
		})
		t.Run("401 unauthorized", func(t *testing.T) { statePermanent(t, http.StatusUnauthorized, "unauthorized", "unauthorized") })
		t.Run("403 server-mismatch", func(t *testing.T) { statePermanent(t, http.StatusForbidden, "server-mismatch", "unauthorized") })
		t.Run("409 record-already-ingested", func(t *testing.T) {
			statePermanent(t, http.StatusConflict, "record-already-ingested", "conflict")
		})
		t.Run("422 manifest-invalid", func(t *testing.T) { statePermanent(t, http.StatusUnprocessableEntity, "manifest-invalid", "rejected") })
		for _, field := range []string{"package_id", "record_id", "file_count", "size", "job_id"} {
			t.Run("ack with wrong "+field+" stays delivered", func(t *testing.T) { stateBadAck(t, field) })
		}
	})
}

// stateHappyPath runs F-01 to acked and returns the distinct uploads and
// commits (method and path). Status reads (GET) are left out: §6.3 allows a
// PUT or a GET to learn what is missing, so a restart may add GETs.
func stateHappyPath(t *testing.T) []string {
	e := newExporterEnv(t, nil)
	recordID, _ := e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()
	s := e.waitFinal(d, recordID, "acked", "")
	d.stop()
	checkAcked(t, e, recordID, s)
	return distinctRequests(withoutGET(e.ingest.requests()))
}

func withoutGET(log []logEntry) []logEntry {
	var out []logEntry
	for _, e := range log {
		if e.Method != http.MethodGet {
			out = append(out, e)
		}
	}
	return out
}

// checkAcked checks the state record and the fake service after an ack.
func checkAcked(t *testing.T, e *exporterEnv, recordID string, s stateRecord) {
	t.Helper()
	if s.RecordID != recordID {
		t.Errorf("state.json record_id %q, want %q", s.RecordID, recordID)
	}
	if s.JobID != fakeJobID {
		t.Errorf("state.json job_id %q, want the ack's %q (§6.3)", s.JobID, fakeJobID)
	}
	var states []string
	for _, h := range s.History {
		if len(states) == 0 || states[len(states)-1] != h.State {
			states = append(states, h.State)
		}
	}
	if want := []string{"ready", "delivered", "acked"}; !reflect.DeepEqual(states, want) {
		t.Errorf("history states %v, want %v", states, want)
	}
	pkg, done, all := e.ingest.committed(recordID)
	if len(all) != 1 || !done || pkg != s.PackageID {
		t.Errorf("fake service: packages %v, committed %v; state.json package_id %q", all, done, s.PackageID)
	}
}

// stateKillRestart starts from a crash right after the claim (the spool
// entry already renamed to <STATE_DIR>/<record_id>/spool.json, no state.json;
// §4.1 step 1), then crashes the daemon (SIGKILL) at every persisted step and
// restarts it: before the manifest upload (ready), during a file upload
// (ready), before the commit (delivered) and after the ack (acked). The final
// state, the package and the set of requests equal those of a run without
// kills, which this subtest makes itself.
func stateKillRestart(t *testing.T) {
	harness.ExporterBin(t) // skip here, not as a failed baseline
	var baseline []string
	t.Run("baseline without kills", func(t *testing.T) { baseline = stateHappyPath(t) })
	if baseline == nil {
		t.Fatal("the baseline run without kills failed, so there is no request log to compare with")
	}

	e := newExporterEnv(t, func(kind string, n int) *fault {
		if n == 1 && (kind == kindManifest || kind == kindFile || kind == kindCommit) {
			return &fault{hold: true}
		}
		return nil
	})
	recordID, _ := e.addRecording("F-01")
	e.claimed(recordID)

	var packageID string
	for _, step := range []struct{ kind, state string }{
		{kindManifest, "ready"},
		{kindFile, "ready"},
		{kindCommit, "delivered"},
	} {
		d := e.start()
		e.ingest.waitArrived(step.kind, stateTimeout, d.running)
		s, ok := e.readState(recordID)
		if !ok || s.State != step.state {
			t.Fatalf("at the first %s request the persisted state is %+v, want %s (§5.1: persisted before the side effect)",
				step.kind, s, step.state)
		}
		if packageID == "" {
			packageID = s.PackageID
		} else if s.PackageID != packageID {
			t.Fatalf("package_id changed after a restart: %s → %s (the manifest is built once)", packageID, s.PackageID)
		}
		d.kill()
	}

	d := e.start()
	e.waitFinal(d, recordID, "acked", "")
	d.kill()

	d = e.start()
	e.idle(d)
	d.stop()

	s, _ := e.readState(recordID)
	if s.State != "acked" || s.PackageID != packageID {
		t.Fatalf("final state %s package_id %s, want acked %s", s.State, s.PackageID, packageID)
	}
	checkAcked(t, e, recordID, s)
	got := distinctRequests(withoutGET(e.ingest.requests()))
	if !reflect.DeepEqual(got, baseline) {
		t.Errorf("requests differ from the run without kills:\n got %v\nwant %v", got, baseline)
	}
}

// stateTransient: a transient error keeps the state, counts the attempt and
// schedules the next one (§6.5: 30 s × 2^(attempts−1) ±20 %, at least
// Retry-After). maxDelay 0 means no upper bound is checked.
func stateTransient(t *testing.T, status int, retryAfter string, minDelay, maxDelay time.Duration) {
	e := newExporterEnv(t, func(kind string, n int) *fault {
		if kind == kindManifest {
			return &fault{status: status, slug: "unavailable", retryAfter: retryAfter}
		}
		return nil
	})
	recordID, _ := e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()
	s := e.waitState(d, recordID, "a counted attempt", func(s stateRecord) bool {
		return (s.Attempts >= 1 && s.NextAttemptAt != "") || s.State == "failed"
	})
	d.stop()

	if s.State != "ready" || s.Attempts != 1 {
		t.Fatalf("after one %d: state %s/%s attempts %d, want ready attempts 1", status, s.State, s.reason(), s.Attempts)
	}
	last, err1 := time.Parse(time.RFC3339Nano, s.LastAttemptAt)
	next, err2 := time.Parse(time.RFC3339Nano, s.NextAttemptAt)
	if err1 != nil || err2 != nil {
		t.Fatalf("last_attempt_at %q / next_attempt_at %q are not RFC 3339", s.LastAttemptAt, s.NextAttemptAt)
	}
	delay := next.Sub(last)
	if delay < minDelay-time.Second || (maxDelay > 0 && delay > maxDelay+time.Second) {
		t.Errorf("next attempt after %s, want %s…%s", delay, minDelay, maxDelay)
	}
	if n := e.ingest.count(kindManifest); n != 1 {
		t.Errorf("manifest sent %d times before next_attempt_at, want 1", n)
	}
}

// statePermanent: a permanent ingest error ends in failed with a reason.
func statePermanent(t *testing.T, status int, slug, reason string) {
	e := newExporterEnv(t, func(kind string, n int) *fault {
		if kind == kindManifest {
			return &fault{status: status, slug: slug}
		}
		return nil
	})
	recordID, raw := e.addRecording("F-01")
	e.spool(recordID)
	d := e.start()
	e.waitFinal(d, recordID, "failed", reason)
	d.stop()
	if !exists(t, raw) {
		t.Errorf("raw dir deleted after a failure")
	}
}

// stateBadAck: an ack that does not match the manifest is not trusted: the
// record stays delivered, gets no job_id, an ERROR is logged, and the raw dir
// is never deleted, even with policy after_ack, BBB markers and time passed
// (§6.3 step 2, §7).
func stateBadAck(t *testing.T, field string) {
	e := newExporterEnv(t, func(kind string, n int) *fault {
		if kind == kindCommit {
			return &fault{badAck: field}
		}
		return nil
	})
	e.vars["RECAP_EXPORTER_DELETE_POLICY"] = "after_ack"
	recordID, raw := e.addRecording("F-01")
	e.markPublished(recordID, "presentation")
	e.spool(recordID)
	d := e.start()
	e.waitState(d, recordID, "delivered", func(s stateRecord) bool { return s.State == "delivered" })
	deadline := time.Now().Add(stateTimeout)
	for e.ingest.count(kindCommit) == 0 {
		if time.Now().After(deadline) || !d.running() {
			t.Fatalf("no commit request\n%s", d.output())
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	d.stop()
	st, _ := e.readState(recordID)
	if !loggedError(d.stdout.String(), recordID, st.PackageID) {
		t.Errorf("no ERROR log line after a mismatching ack\n%s", d.output())
	}

	// A day later, the record must still be delivered and the raw dir kept.
	e.backdate(recordID, 48*time.Hour)
	d = e.start()
	e.idle(d)
	d.stop()
	s, _ := e.readState(recordID)
	if s.State != "delivered" || s.JobID != "" {
		t.Errorf("after a mismatching %s: state %s job_id %q, want delivered without job_id", field, s.State, s.JobID)
	}
	if !exists(t, raw) {
		t.Errorf("raw dir deleted on an unverified ack")
	}
}

// loggedError reports whether a stdout line is a JSON object whose "level"
// is "error", compared case-insensitively, and that names the record or the
// package (§6.3 step 2, §8.2, README "Logging": keys time, level, msg).
func loggedError(stdout, recordID, packageID string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		level, _ := obj["level"].(string)
		if !strings.EqualFold(level, "error") {
			continue
		}
		for _, v := range obj {
			if v == recordID || (packageID != "" && v == packageID) {
				return true
			}
		}
	}
	return false
}
