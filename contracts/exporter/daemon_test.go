// Package exporter holds the black-box contract tests for the BBB exporter
// daemon and the post_archive hook (docs/reference/bbb-exporter.md §10).
//
// The daemon tests run the binary from RECAP_EXPORTER_BIN as `recap-exporter
// run` with its configuration in the environment (§8.1: the environment
// overrides the env file; keys in docs/reference/deployment.md §5.4) and
// RECAP_EXPORTER_ENV_FILE pointing at an empty file in the temp dir, against a
// local TLS fake ingest service whose certificate is passed in SSL_CERT_FILE.
// Each test writes the spool entry itself (§3.4) and observes the daemon only
// through files the reference (bbb-exporter.md) defines: the state record (§5.3), the raw dir (§7)
// and the requests the fake service receives.
package exporter

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Fixture values (docs/reference/fixtures.md §3, testdata/README.md).
const (
	fixtureServerID  = "bbb-fixture-a"
	fixtureCreatedAt = "2025-10-29T10:10:00Z"
	fixtureVersion   = "0.0.0-fixture"
	f01RecordID      = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
)

// Timing. The daemon polls every 10 s (§4.1), so one loop fits in loopWait.
const (
	loopWait     = 12 * time.Second
	stateTimeout = 60 * time.Second
	stopTimeout  = 25 * time.Second // §4.3: exit within 20 s of SIGTERM
)

// testdataDir is testdata/ at the module root; go test runs in contracts/exporter.
func testdataDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// exporterEnv is one isolated exporter installation: dirs, token, config and
// a fake ingest service.
type exporterEnv struct {
	t         *testing.T
	bin       string
	dir       string
	rawDir    string
	spoolDir  string
	stateDir  string
	statusDir string
	token     string
	vars      map[string]string
	ingest    *fakeIngest
}

// newExporterEnv skips without RECAP_EXPORTER_BIN. r injects faults into the
// fake ingest service (nil = always answer normally).
func newExporterEnv(t *testing.T, r rule) *exporterEnv {
	t.Helper()
	bin := harness.ExporterBin(t)
	dir := t.TempDir()
	e := &exporterEnv{
		t:         t,
		bin:       bin,
		dir:       dir,
		rawDir:    filepath.Join(dir, "raw"),
		spoolDir:  filepath.Join(dir, "spool"),
		stateDir:  filepath.Join(dir, "state"),
		statusDir: filepath.Join(dir, "status"),
	}
	for _, d := range []string{e.rawDir, e.spoolDir, e.stateDir, e.statusDir, filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Token file: the fixture credential of bbb-fixture-a, mode 0400 (§8.5).
	tok, err := os.ReadFile(filepath.Join(testdataDir(t), "secrets", "clients", "bbb_server."+fixtureServerID))
	if err != nil {
		t.Fatal(err)
	}
	e.token = strings.TrimSpace(string(tok))
	tokenFile := filepath.Join(dir, "ingest-token")
	if err := os.WriteFile(tokenFile, tok, 0o400); err != nil {
		t.Fatal(err)
	}

	e.ingest = newFakeIngest(t, e.token, r)
	certFile := filepath.Join(dir, "service-ca.pem")
	e.ingest.writeCert(certFile)

	// An empty env file in the temp dir, so a host /etc/recap-exporter/exporter.env
	// is never read (§8.1); every key comes from the environment below.
	envFile := filepath.Join(dir, "exporter.env")
	if err := os.WriteFile(envFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	e.vars = map[string]string{
		"RECAP_EXPORTER_ENV_FILE":             envFile,
		"PATH":                                os.Getenv("PATH"),
		"HOME":                                dir,
		"TMPDIR":                              filepath.Join(dir, "tmp"),
		"SSL_CERT_FILE":                       certFile,
		"RECAP_EXPORTER_SERVICE_URL":          e.ingest.URL(),
		"RECAP_EXPORTER_BBB_SERVER_ID":        fixtureServerID,
		"RECAP_EXPORTER_TOKEN_FILE":           tokenFile,
		"RECAP_EXPORTER_META_PREFIX":          "recap",
		"RECAP_EXPORTER_RAW_DIR":              e.rawDir,
		"RECAP_EXPORTER_SPOOL_DIR":            e.spoolDir,
		"RECAP_EXPORTER_STATE_DIR":            e.stateDir,
		"RECAP_EXPORTER_DELETE_POLICY":        "never",
		"RECAP_EXPORTER_LISTEN_ADDR":          freeAddr(t),
		"RECAP_EXPORTER_LOG_LEVEL":            "info",
		"RECAP_EXPORTER_RECORDING_STATUS_DIR": e.statusDir,
		"RECAP_EXPORTER_RECORDING_FORMATS":    "presentation",
	}
	return e
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// environ returns the process environment: only e.vars, so no RECAP_EXPORTER_*
// key of the caller leaks in (unknown keys are config errors, §8.1).
func (e *exporterEnv) environ() []string {
	var out []string
	for k, v := range e.vars {
		out = append(out, k+"="+v)
	}
	return out
}

// addRecording copies the raw dir of a fixture into the raw dir and returns
// its record_id and path. It does not spool it.
func (e *exporterEnv) addRecording(fixture string) (recordID, dir string) {
	e.t.Helper()
	src := filepath.Join(testdataDir(e.t), "meetings", fixture, "raw")
	entries, err := os.ReadDir(src)
	if err != nil || len(entries) != 1 {
		e.t.Fatalf("%s: want one record dir: %v", src, err)
	}
	recordID = entries[0].Name()
	dir = filepath.Join(e.rawDir, recordID)
	copyTree(e.t, filepath.Join(src, recordID), dir)
	return recordID, dir
}

// spool writes the spool entry the hook would write (§3.4).
func (e *exporterEnv) spool(recordID string) {
	e.t.Helper()
	b, err := json.Marshal(map[string]any{
		"spool_version": 1,
		"record_id":     recordID,
		"hooked_at":     "2025-10-29T10:05:00Z",
		"hook_version":  fixtureVersion,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	tmp := filepath.Join(e.spoolDir, ".tmp-"+recordID+"-1")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(e.spoolDir, recordID+".json")); err != nil {
		e.t.Fatal(err)
	}
}

// claimed leaves the record as a daemon that crashed right after the claim
// would: the spool entry renamed to <STATE_DIR>/<record_id>/spool.json and no
// state.json yet (§4.1 step 1).
func (e *exporterEnv) claimed(recordID string) {
	e.t.Helper()
	e.spool(recordID)
	dir := filepath.Join(e.stateDir, recordID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.spoolDir, recordID+".json"), filepath.Join(dir, "spool.json")); err != nil {
		e.t.Fatal(err)
	}
}

// copyTree copies regular files, dirs and symlinks (as links, not followed).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a bytes.Buffer safe for the copy goroutines of os/exec.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// daemon is one running `recap-exporter run`.
type daemon struct {
	t              *testing.T
	cmd            *exec.Cmd
	stdout, stderr *syncBuffer
	exited         chan struct{}
	err            error
}

// start runs `recap-exporter run` and stops it at test end.
func (e *exporterEnv) start() *daemon {
	e.t.Helper()
	d := &daemon{t: e.t, stdout: &syncBuffer{}, stderr: &syncBuffer{}, exited: make(chan struct{})}
	d.cmd = exec.Command(e.bin, "run")
	d.cmd.Env = e.environ()
	d.cmd.Dir = e.dir
	d.cmd.Stdout = d.stdout
	d.cmd.Stderr = d.stderr
	if err := d.cmd.Start(); err != nil {
		e.t.Fatalf("start %s run: %v", e.bin, err)
	}
	go func() {
		d.err = d.cmd.Wait()
		close(d.exited)
	}()
	e.t.Cleanup(d.kill)
	return d
}

func (d *daemon) output() string {
	return "stdout:\n" + d.stdout.String() + "\nstderr:\n" + d.stderr.String()
}

func (d *daemon) running() bool {
	select {
	case <-d.exited:
		return false
	default:
		return true
	}
}

// kill sends SIGKILL (a crash) and waits for the exit.
func (d *daemon) kill() {
	if d.running() {
		_ = d.cmd.Process.Kill()
	}
	<-d.exited
}

// stop sends SIGTERM and expects an exit within stopTimeout (§4.3).
func (d *daemon) stop() {
	d.t.Helper()
	if !d.running() {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.exited:
	case <-time.After(stopTimeout):
		d.kill()
		d.t.Errorf("exporter did not exit within %s of SIGTERM", stopTimeout)
	}
}

// stateRecord is <STATE_DIR>/<record_id>/state.json (§5.3).
type stateRecord struct {
	RecordID       string  `json:"record_id"`
	State          string  `json:"state"`
	Reason         *string `json:"reason"`
	PackageID      string  `json:"package_id"`
	JobID          string  `json:"job_id"`
	Attempts       int     `json:"attempts"`
	FirstAttemptAt string  `json:"first_attempt_at"`
	LastAttemptAt  string  `json:"last_attempt_at"`
	NextAttemptAt  string  `json:"next_attempt_at"`
	History        []struct {
		State string `json:"state"`
		At    string `json:"at"`
	} `json:"history"`
}

func (s stateRecord) reason() string {
	if s.Reason == nil {
		return ""
	}
	return *s.Reason
}

func (e *exporterEnv) statePath(recordID string) string {
	return filepath.Join(e.stateDir, recordID, "state.json")
}

// readState returns the state record, or ok=false if it does not exist (yet)
// or is not valid JSON.
func (e *exporterEnv) readState(recordID string) (stateRecord, bool) {
	var s stateRecord
	b, err := os.ReadFile(e.statePath(recordID))
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false
	}
	return s, true
}

// waitState polls the state record until cond holds. It fails when the
// daemon exits or the timeout passes.
func (e *exporterEnv) waitState(d *daemon, recordID string, what string, cond func(stateRecord) bool) stateRecord {
	e.t.Helper()
	deadline := time.Now().Add(stateTimeout)
	for {
		s, ok := e.readState(recordID)
		if ok && cond(s) {
			return s
		}
		if !d.running() {
			e.t.Fatalf("exporter exited (%v) before %s; last state %+v\n%s", d.err, what, s, d.output())
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no %s within %s; last state %+v; requests %v\n%s",
				what, stateTimeout, s, e.ingest.requests(), d.output())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitFinal waits for a state (and failure reason, if not empty).
func (e *exporterEnv) waitFinal(d *daemon, recordID, state, reason string) stateRecord {
	e.t.Helper()
	what := "state " + state
	if reason != "" {
		what += "/" + reason
	}
	// A record that turns failed on the way ends the wait early, unless it was
	// failed already (failure expiry starts from failed).
	before, _ := e.readState(recordID)
	s := e.waitState(d, recordID, what, func(s stateRecord) bool {
		return s.State == state || (s.State == "failed" && state != "failed" && before.State != "failed")
	})
	if s.State != state || (reason != "" && s.reason() != reason) {
		e.t.Fatalf("got %s/%s, want %s\nrequests %v\n%s", s.State, s.reason(), what, e.ingest.requests(), d.output())
	}
	return s
}

// idle lets the daemon run for one more loop and checks it is still alive.
func (e *exporterEnv) idle(d *daemon) {
	e.t.Helper()
	time.Sleep(loopWait)
	if !d.running() {
		e.t.Fatalf("exporter exited (%v)\n%s", d.err, d.output())
	}
}

// backdate shifts every timestamp in the state record into the past, as if
// that much time had passed (the daemon must be stopped). The reference has no
// clock seam; the state file is the only place that records when a state
// was entered (§5.3).
func (e *exporterEnv) backdate(recordID string, by time.Duration) {
	e.t.Helper()
	path := e.statePath(recordID)
	b, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatal(err)
	}
	shift := func(v any) any {
		s, ok := v.(string)
		if !ok {
			return v
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return v
		}
		return ts.Add(-by).UTC().Format(time.RFC3339Nano)
	}
	for _, k := range []string{"first_attempt_at", "last_attempt_at", "next_attempt_at"} {
		if v, ok := m[k]; ok {
			m[k] = shift(v)
		}
	}
	if h, ok := m["history"].([]any); ok {
		for _, entry := range h {
			if em, ok := entry.(map[string]any); ok {
				em["at"] = shift(em["at"])
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	tmp := path + ".backdate"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		e.t.Fatal(err)
	}
}

// markPublished writes BBB's completion marker
// <RECAP_EXPORTER_RECORDING_STATUS_DIR>/published/<record_id>-<format>.done
// for each format (§7, deployment.md §5.4; the marker path mirrors
// bigbluebutton-config/cron.daily/bigbluebutton:94-123 @ 7d27e7e).
func (e *exporterEnv) markPublished(recordID string, formats ...string) {
	e.t.Helper()
	dir := filepath.Join(e.statusDir, "published")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for _, f := range formats {
		if err := os.WriteFile(filepath.Join(dir, recordID+"-"+f+".done"), nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return false
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
