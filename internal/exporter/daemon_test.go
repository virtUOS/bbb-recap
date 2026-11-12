package exporter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const f01 = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"

// fakeService is a small ingest service: manifest PUT, file PUT, commit.
type fakeService struct {
	mu       sync.Mutex
	files    map[string]bool // sha256 -> received
	count    int
	size     int64
	recordID string
	commits  int
	badAck   bool
	hang     chan struct{} // if set, every request blocks until it is closed or the client goes away
	requests []string
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.hang != nil {
		select {
		case <-f.hang:
		case <-r.Context().Done():
			return
		}
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer FIXTURE-NOT-A-SECRET-test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/ingest/packages/"), "/")
	pkg := parts[0]
	switch {
	case r.Method == http.MethodPut && len(parts) == 1:
		var m struct {
			RecordID string `json:"record_id"`
			Files    []struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"files"`
		}
		_ = json.Unmarshal(body, &m)
		if f.files == nil {
			f.files = map[string]bool{}
			f.recordID = m.RecordID
			for _, x := range m.Files {
				f.files[x.SHA256] = false
				f.count++
				f.size += x.Size
			}
		}
		missing := []string{}
		for sha, ok := range f.files {
			if !ok {
				missing = append(missing, sha)
			}
		}
		sort.Strings(missing)
		state := "receiving"
		if f.commits > 0 {
			state = "committed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"package_id": pkg, "state": state, "missing": missing})
	case r.Method == http.MethodPut && len(parts) == 3:
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != parts[2] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.files[parts[2]] = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost:
		f.commits++
		ack := map[string]any{"package_id": pkg, "state": "committed", "job_id": "019a3000-0000-7000-8000-000000000001",
			"record_id": f.recordID, "file_count": f.count, "size": f.size}
		if f.badAck {
			ack["size"] = f.size + 1
		}
		_ = json.NewEncoder(w).Encode(ack)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type testEnv struct {
	d       *Daemon
	cfg     *Config
	svc     *fakeService
	logs    *bytes.Buffer
	clock   time.Time
	rawDir  string
	recDir  string
	spool   string
	markers string
}

// newTestEnv builds a daemon with F-01 in its raw dir, a spool entry for it
// and a TLS fake service. The clock starts now and moves with advance.
func newTestEnv(t *testing.T, svc *fakeService) *testEnv {
	t.Helper()
	dir := t.TempDir()
	e := &testEnv{svc: svc, logs: &bytes.Buffer{}, clock: time.Now().UTC()}
	srv := httptest.NewTLSServer(svc)
	t.Cleanup(srv.Close)
	if svc.hang != nil {
		t.Cleanup(func() { // unblock before srv.Close waits for handlers
			select {
			case <-svc.hang:
			default:
				close(svc.hang)
			}
		})
	}
	e.cfg = &Config{
		ServiceURL: srv.URL, BBBServerID: "bbb-fixture-a", MetaPrefix: "recap",
		RawDir: filepath.Join(dir, "raw"), SpoolDir: filepath.Join(dir, "spool"), StateDir: filepath.Join(dir, "state"),
		StatusDir: filepath.Join(dir, "status"), Formats: []string{"presentation"},
		DeletePolicy: PolicyAfterAck, ListenAddr: "127.0.0.1:0",
	}
	for _, d := range []string{e.cfg.SpoolDir, e.cfg.StateDir, filepath.Join(e.cfg.StatusDir, "published")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.rawDir = filepath.Join(e.cfg.RawDir, f01)
	copyDir(t, filepath.Join("..", "..", "testdata", "meetings", "F-01", "raw", f01), e.rawDir)
	e.recDir = filepath.Join(e.cfg.StateDir, f01)
	e.markers = filepath.Join(e.cfg.StatusDir, "published", f01+"-presentation.done")
	e.spool = filepath.Join(e.cfg.SpoolDir, f01+".json")
	if err := os.WriteFile(e.spool, []byte(`{"spool_version":1,"record_id":"`+f01+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.d = NewDaemon(e.cfg, "FIXTURE-NOT-A-SECRET-test", slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	e.d.client.http = srv.Client()
	e.d.client.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	e.d.now = func() time.Time { return e.clock }
	return e
}

func (e *testEnv) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *testEnv) state(t *testing.T) *Record {
	t.Helper()
	r, err := e.d.store.Load(f01)
	if err != nil || r == nil {
		t.Fatalf("state: %v %v", r, err)
	}
	return r
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		}
		return nil // symlinks are left out here
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoopToAckedThenDeleteAndPrune(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	ctx := context.Background()
	e.d.loop(ctx)
	if r := e.state(t); r.State != StateAcked || r.JobID == "" {
		t.Fatalf("after one loop: %+v", r)
	}
	if fileExists(e.spool) {
		t.Error("spool entry not claimed")
	}

	// Markers present, but within the 24 h grace.
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.advance(23 * time.Hour)
	e.d.loop(ctx)
	if !fileExists(e.rawDir) {
		t.Fatal("raw dir deleted within the grace")
	}
	e.advance(2 * time.Hour)
	e.d.loop(ctx)
	if r := e.state(t); r.State != StateDeleted || r.ReasonString() != ReasonAfterAck {
		t.Fatalf("after the grace: %s/%s", r.State, r.ReasonString())
	}
	if fileExists(e.rawDir) {
		t.Fatal("raw dir still there")
	}
	if !fileExists(e.cfg.RawDir) {
		t.Fatal("RAW_DIR itself was removed")
	}

	// Prune 30 days after entering deleted.
	e.advance(29 * 24 * time.Hour)
	e.d.loop(ctx)
	if !fileExists(e.recDir) {
		t.Fatal("pruned before 30 days")
	}
	e.advance(25 * time.Hour)
	e.d.loop(ctx)
	if fileExists(e.recDir) {
		t.Fatal("not pruned after 30 days")
	}
	if strings.Contains(e.logs.String(), `"level":"INFO","msg":"raw dir deleted"`) == false {
		t.Error("no INFO line for the deletion")
	}
}

func TestPruneAcked(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	e.cfg.DeletePolicy = PolicyNever
	e.d.loop(context.Background())
	e.advance(stateRetention + time.Minute)
	e.d.loop(context.Background())
	if fileExists(e.recDir) {
		t.Fatal("acked record not pruned after 30 days")
	}
	if !fileExists(e.rawDir) {
		t.Fatal("policy never deleted raw data")
	}
}

func TestAckMismatchStaysDelivered(t *testing.T) {
	e := newTestEnv(t, &fakeService{badAck: true})
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.d.loop(context.Background())
	r := e.state(t)
	if r.State != StateDelivered || r.JobID != "" || r.Attempts != 1 || r.NextAttemptAt.IsZero() {
		t.Fatalf("after a bad ack: %+v", r)
	}
	if !strings.Contains(e.logs.String(), `"level":"ERROR","msg":"ack does not match the manifest, not trusted"`) {
		t.Errorf("no ERROR log:\n%s", e.logs)
	}
	e.advance(72 * time.Hour)
	e.d.loop(context.Background())
	if r := e.state(t); r.State != StateDelivered || !fileExists(e.rawDir) {
		t.Fatalf("later: %s, raw dir exists %v", r.State, fileExists(e.rawDir))
	}
	// Manual delete-raw refuses as well.
	if err := e.d.DeleteRaw(f01); err == nil {
		t.Fatal("delete-raw accepted an unverified ack")
	}
}

func TestShutdownAbortsRequest(t *testing.T) {
	e := newTestEnv(t, &fakeService{hang: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.d.Run(ctx) }()
	time.Sleep(time.Second) // the loop is now waiting for the manifest PUT
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(ShutdownLimit):
		t.Fatal("Run did not return within 20 s of cancellation")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	if r := e.state(t); r.State != StateReady || r.Attempts != 0 {
		t.Errorf("an aborted request is not an attempt: %+v", r)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: %v, want ErrLocked", err)
	}
	_ = l.Close()
	l, err = Lock(dir)
	if err != nil {
		t.Errorf("lock after release: %v", err)
	}
	_ = l.Close()
}

func TestStatusRequeueDeleteRaw(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	// Make the build fail: no microphone files.
	if err := os.RemoveAll(filepath.Join(e.rawDir, "audio")); err != nil {
		t.Fatal(err)
	}
	e.d.loop(context.Background())
	if r := e.state(t); r.State != StateFailed || r.ReasonString() != ReasonNoMicrophone {
		t.Fatalf("build: %s/%s", r.State, r.ReasonString())
	}

	var out bytes.Buffer
	if err := e.d.Status(&out, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"state": "failed"`) || !strings.Contains(out.String(), `"reason": "no_microphone_tracks"`) {
		t.Errorf("status:\n%s", out.String())
	}
	if strings.Contains(out.String(), "fixture-meeting-01") || strings.Contains(out.String(), "microphone-") {
		t.Errorf("status leaks meeting_id or file names:\n%s", out.String())
	}
	if err := e.d.Status(io.Discard, "../etc"); !errors.Is(err, ErrUsage) {
		t.Errorf("status with a bad record_id: %v", err)
	}

	// delete-raw on a failed record without failure expiry: refused.
	if err := e.d.DeleteRaw(f01); err == nil {
		t.Fatal("delete-raw on a failed record accepted")
	}

	// The lock: requeue refuses while the daemon holds it.
	l, err := Lock(e.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.Requeue(f01); !errors.Is(err, ErrLocked) {
		t.Errorf("requeue under the daemon's lock: %v", err)
	}
	_ = l.Close()

	// Fix the source, requeue, rebuild and push.
	copyDir(t, filepath.Join("..", "..", "testdata", "meetings", "F-01", "raw", f01, "audio"), filepath.Join(e.rawDir, "audio"))
	if err := e.d.Requeue(f01); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := e.d.Status(&out, f01); err != nil || !strings.Contains(out.String(), `"state": "spooled"`) {
		t.Errorf("status after requeue: %v\n%s", err, out.String())
	}
	if err := e.d.Requeue(f01); err == nil {
		t.Error("requeue of a record that is not failed accepted")
	}
	e.d.loop(context.Background())
	if r := e.state(t); r.State != StateAcked {
		t.Fatalf("after requeue: %s/%s", r.State, r.ReasonString())
	}

	// delete-raw: grace and markers apply, policy does not.
	e.cfg.DeletePolicy = PolicyNever
	if err := e.d.DeleteRaw(f01); err == nil {
		t.Fatal("delete-raw within the grace accepted")
	}
	e.advance(25 * time.Hour)
	if err := e.d.DeleteRaw(f01); err == nil {
		t.Fatal("delete-raw without BBB markers accepted")
	}
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.d.DeleteRaw(f01); err != nil {
		t.Fatal(err)
	}
	if r := e.state(t); r.State != StateDeleted || r.ReasonString() != ReasonManual || fileExists(e.rawDir) {
		t.Fatalf("after delete-raw: %s/%s, raw exists %v", r.State, r.ReasonString(), fileExists(e.rawDir))
	}
}

func TestDeleteRefusedWhenEventsChanged(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	e.d.loop(context.Background())
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(e.rawDir, "events.xml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("<!-- changed -->")
	_ = f.Close()
	e.advance(25 * time.Hour)
	e.d.loop(context.Background())
	if r := e.state(t); r.State != StateFailed || r.ReasonString() != ReasonSourceChanged {
		t.Fatalf("got %s/%s", r.State, r.ReasonString())
	}
	if !fileExists(filepath.Join(e.rawDir, "events.xml")) {
		t.Fatal("raw dir deleted")
	}
}

func TestDeleteDoesNotFollowSymlinks(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	outside := filepath.Join(t.TempDir(), "keep.webm")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(e.rawDir, "presentation", "linkdir")); err != nil {
		t.Fatal(err)
	}
	e.d.loop(context.Background())
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.advance(25 * time.Hour)
	e.d.loop(context.Background())
	if r := e.state(t); r.State != StateDeleted {
		t.Fatalf("got %s/%s", r.State, r.ReasonString())
	}
	if !fileExists(outside) {
		t.Fatal("symlink target deleted")
	}
}

// A record that failed because its raw dir no longer matches the
// manifest is not re-checked every loop under failure expiry: one ERROR,
// one history entry.
func TestFailedSourceChangedNotRepeated(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	e.cfg.FailureDays = 1
	e.d.loop(context.Background())
	if err := os.WriteFile(e.markers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(e.rawDir, "events.xml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("<!-- changed -->")
	_ = f.Close()
	e.advance(25 * time.Hour)
	e.d.loop(context.Background())
	r := e.state(t)
	if r.State != StateFailed || r.ReasonString() != ReasonSourceChanged {
		t.Fatalf("got %s/%s", r.State, r.ReasonString())
	}
	historyLen := len(r.History)
	for range 5 {
		e.advance(48 * time.Hour)
		e.d.loop(context.Background())
	}
	if r := e.state(t); len(r.History) != historyLen || r.State != StateFailed {
		t.Errorf("history grew from %d to %d entries (%s)", historyLen, len(r.History), r.State)
	}
	if n := strings.Count(e.logs.String(), `"level":"ERROR"`); n != 1 {
		t.Errorf("%d ERROR lines, want 1:\n%s", n, e.logs)
	}
	if !fileExists(filepath.Join(e.rawDir, "events.xml")) {
		t.Error("raw dir deleted")
	}
}

// A record whose claim was interrupted (no spool.json) can still
// be requeued.
func TestRequeueWithoutSpoolFile(t *testing.T) {
	e := newTestEnv(t, &fakeService{})
	if err := os.RemoveAll(filepath.Join(e.rawDir, "audio")); err != nil {
		t.Fatal(err)
	}
	e.d.loop(context.Background())
	if err := os.Remove(filepath.Join(e.recDir, spoolFile)); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Requeue(f01); err != nil {
		t.Fatalf("requeue without spool.json: %v", err)
	}
	if fileExists(filepath.Join(e.recDir, stateFile)) || !fileExists(e.recDir) {
		t.Error("requeue did not leave a record to rebuild")
	}
}
