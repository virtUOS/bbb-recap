package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	exporterServerID = "bbb-fixture-a"
	exporterDeadline = 3 * time.Minute // covers the 10 s loop plus one 40 s Retry-After
	fixedTime        = "2025-10-29T10:30:00Z"
)

var packagePathRE = regexp.MustCompile(`^/api/v1/ingest/packages/([0-9a-f]{64})$`)

// runExporter runs `recap-exporter run` once per case against a TLS fake
// server that answers with the golden responses, then checks the requests it
// saw and the exporter's state record (bbb-exporter.md §5.3).
func runExporter(t *testing.T, bin string, cases []exchangeCase) {
	for _, c := range cases {
		if c.Exporter == nil {
			continue // service side only
		}
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()
			exporterCase(t, bin, c)
		})
	}
}

func exporterCase(t *testing.T, bin string, c exchangeCase) {
	steps := c.stepsOn("exporter")
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	spoolDir := filepath.Join(dir, "spool")
	stateDir := filepath.Join(dir, "state")
	copyTree(t, filepath.Join(testdata, "meetings", c.Fixture, "raw", c.RecordID), filepath.Join(rawDir, c.RecordID))
	for _, cp := range c.RawCopies {
		if strings.Contains(cp.From+cp.To, "..") || path.IsAbs(cp.From) || path.IsAbs(cp.To) {
			t.Fatalf("bad raw_copies entry %+v", cp)
		}
		data, err := os.ReadFile(filepath.Join(testdata, "meetings", c.Fixture, "raw", c.RecordID, filepath.FromSlash(cp.From)))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(rawDir, c.RecordID, filepath.FromSlash(cp.To)), data, 0o644)
	}
	for _, d := range []string{spoolDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Spool entry as the post_archive hook writes it (bbb-exporter.md §3.4).
	spool, _ := json.Marshal(map[string]any{
		"spool_version": 1, "record_id": c.RecordID, "hooked_at": "2025-10-29T10:05:00Z", "hook_version": "0.0.0-fixture",
	})
	writeFile(t, filepath.Join(spoolDir, c.RecordID+".json"), spool, 0o644)
	tok := token(t, "bbb_server."+exporterServerID)
	tokenFile := filepath.Join(dir, "ingest-token")
	writeFile(t, tokenFile, []byte(tok+"\n"), 0o400)

	f := &fakeIngest{steps: steps, token: tok, golden: readBody(t, steps[0].Request.Body)}
	srv := httptest.NewUnstartedServer(f)
	srv.StartTLS()
	defer srv.Close()
	caFile := filepath.Join(dir, "ca.pem")
	writeFile(t, caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)

	cmd := exec.Command(bin, "run")
	cmd.Env = append(envWithout("RECAP_EXPORTER_", "SSL_CERT_"),
		"RECAP_EXPORTER_SERVICE_URL="+srv.URL,
		"RECAP_EXPORTER_BBB_SERVER_ID="+exporterServerID,
		"RECAP_EXPORTER_TOKEN_FILE="+tokenFile,
		"RECAP_EXPORTER_RAW_DIR="+rawDir,
		"RECAP_EXPORTER_SPOOL_DIR="+spoolDir,
		"RECAP_EXPORTER_STATE_DIR="+stateDir,
		"RECAP_EXPORTER_LISTEN_ADDR="+freeAddr(t),
		// The exporter has no CA setting; Go reads extra roots from SSL_CERT_FILE on Linux.
		"SSL_CERT_FILE="+caFile,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	defer stop(cmd, exited)

	statePath := filepath.Join(stateDir, c.RecordID, "state.json")
	st, err := waitForState(statePath, *c.Exporter, exited)
	if err != nil {
		select {
		case <-exited:
			err = fmt.Errorf("%v (exit: %v)", err, waitErr)
		default:
		}
		t.Errorf("%v\nexporter output:\n%s", err, out.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.errs {
		t.Errorf("fake ingest server: %s", e)
	}
	if f.next != len(f.steps) {
		t.Errorf("exporter made %d of %d golden requests", f.next, len(f.steps))
	}
	if err == nil && c.Exporter.State == "acked" && st.JobID != f.jobID {
		t.Errorf("state.json job_id %q, want the acked job_id %q", st.JobID, f.jobID)
	}
}

type stateRecord struct {
	State  string  `json:"state"`
	Reason *string `json:"reason"`
	JobID  string  `json:"job_id"`
}

// waitForState polls state.json until it shows the wanted state and reason.
func waitForState(p string, want outcome, exited <-chan struct{}) (stateRecord, error) {
	deadline := time.Now().Add(exporterDeadline)
	var st stateRecord
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return st, fmt.Errorf("exporter exited early")
		case <-time.After(500 * time.Millisecond):
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		st = stateRecord{}
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		if st.State == want.State && reflect.DeepEqual(st.Reason, want.Reason) {
			return st, nil
		}
	}
	return st, fmt.Errorf("state.json after %v: state %q reason %v, want %q %v",
		exporterDeadline, st.State, deref(st.Reason), want.State, deref(want.Reason))
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func stop(cmd *exec.Cmd, exited <-chan struct{}) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(25 * time.Second): // the exporter must stop within 20 s (bbb-exporter.md §4.3)
		_ = cmd.Process.Kill()
		<-exited
	}
}

// fakeIngest serves the golden steps of one case in order. The golden
// package_id is replaced by the one the exporter computed, because the
// exporter's manifest has its own created_at and version.
type fakeIngest struct {
	mu       sync.Mutex
	steps    []exchangeStep
	next     int
	token    string
	golden   []byte // golden manifest of the case
	manifest []byte // first manifest the exporter sent
	pkg      string // its package_id
	goldenID string
	last     time.Time
	jobID    string
	errs     []string
}

func (f *fakeIngest) fail(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next >= len(f.steps) {
		f.fail("unexpected request after the last golden step: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	n := f.next + 1
	s := f.steps[f.next]
	if err := f.checkRequest(r, s); err != nil {
		f.fail("step %d: %v", n, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if s.MinDelayS > 0 {
		if d := time.Since(f.last); d < time.Duration(s.MinDelayS*float64(time.Second))-100*time.Millisecond {
			f.fail("step %d: request came %v after the previous one, want at least %vs", n, d.Round(time.Millisecond), s.MinDelayS)
		}
	}
	f.next++
	defer func() { f.last = time.Now() }()

	if s.Request.Fault == "abort_body" {
		// Read about half of the file, then cut the connection.
		_, _ = io.CopyN(io.Discard, r.Body, int64(s.Request.Body.Size/2))
		closeConn(w)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.fail("step %d: reading body: %v", n, err)
		closeConn(w)
		return
	}
	if err := f.checkBody(r, s, body); err != nil {
		f.fail("step %d: %v", n, err)
	}
	if s.Response.Fault == "drop" {
		closeConn(w)
		return
	}
	for k, v := range s.Response.Headers {
		w.Header().Set(k, v)
	}
	var out []byte
	if s.Response.hasBody() {
		var v any
		_ = json.Unmarshal(s.Response.Body, &v)
		v = f.fill(v)
		if m, ok := v.(map[string]any); ok {
			if id, ok := m["job_id"].(string); ok {
				f.jobID = id
			}
		}
		out, _ = json.Marshal(v)
	}
	w.WriteHeader(s.Response.Status)
	_, _ = w.Write(out)
}

func (f *fakeIngest) checkRequest(r *http.Request, s exchangeStep) error {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		return fmt.Errorf("%s %s: Authorization header is not the fixture bearer token", r.Method, r.URL.Path)
	}
	isManifest := s.Request.Method == http.MethodPut && packagePathRE.MatchString(s.Request.Path)
	if f.pkg == "" {
		// The first request must be the manifest PUT; it fixes the package_id.
		// Every retry also starts with a manifest PUT or GET (bbb-exporter.md §6.3 step 4),
		// which the golden steps after a fault encode.
		m := packagePathRE.FindStringSubmatch(r.URL.Path)
		if !isManifest || r.Method != http.MethodPut || m == nil {
			return fmt.Errorf("got %s %s, want the manifest PUT first", r.Method, r.URL.Path)
		}
		f.pkg = m[1]
		f.goldenID = packagePathRE.FindStringSubmatch(s.Request.Path)[1]
		return nil
	}
	want := strings.ReplaceAll(s.Request.Path, f.goldenID, f.pkg)
	method := r.Method
	if isManifest && method == http.MethodGet {
		method = http.MethodPut // a re-check may use GET instead of PUT (bbb-exporter.md §6.3 steps 1 and 4)
	}
	if method != s.Request.Method || r.URL.Path != want {
		return fmt.Errorf("got %s %s, want %s %s", r.Method, r.URL.Path, s.Request.Method, want)
	}
	return nil
}

func (f *fakeIngest) checkBody(r *http.Request, s exchangeStep, body []byte) error {
	if r.Method == http.MethodGet {
		return nil
	}
	switch {
	case packagePathRE.MatchString(r.URL.Path):
		if f.manifest == nil {
			if sha256hex(body) != f.pkg {
				return fmt.Errorf("package_id in the path is not the SHA-256 of the manifest bytes")
			}
			f.manifest = body
			return sameManifest(f.golden, body)
		}
		if !bytes.Equal(body, f.manifest) {
			return fmt.Errorf("manifest bytes changed between PUTs; the exporter builds the manifest once")
		}
	case strings.HasSuffix(r.URL.Path, "/commit"):
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil || len(v) != 0 {
			return fmt.Errorf("commit body %q, want {}", body)
		}
	default: // file upload
		if r.ContentLength < 0 {
			return fmt.Errorf("file upload without Content-Length")
		}
		if got, want := sha256hex(body), path.Base(r.URL.Path); got != want {
			return fmt.Errorf("uploaded bytes hash to %s, URL names %s", got, want)
		}
	}
	return nil
}

// sameManifest compares the exporter's manifest with the golden one, except
// for created_at and exporter.version, which the test cannot inject.
func sameManifest(golden, got []byte) error {
	var g, m map[string]any
	if err := json.Unmarshal(golden, &g); err != nil {
		return err
	}
	if err := json.Unmarshal(got, &m); err != nil {
		return fmt.Errorf("manifest is not JSON: %v", err)
	}
	for _, k := range []string{"schema_version", "bbb_server_id", "record_id", "meeting_id", "breakout",
		"meta_prefix", "metadata", "files", "totals"} {
		if !reflect.DeepEqual(g[k], m[k]) {
			return fmt.Errorf("manifest %s: got %v, want %v", k, m[k], g[k])
		}
	}
	if e, _ := m["exporter"].(map[string]any); e == nil || e["name"] != "recap-exporter" {
		return fmt.Errorf("manifest exporter: got %v, want name recap-exporter", m["exporter"])
	}
	return nil
}

// fill turns a golden response body into what the fake sends: the golden
// package_id becomes the exporter's, and placeholders get fixed values.
func (f *fakeIngest) fill(v any) any {
	return mapStrings(v, func(x string) string {
		if m := placeholderRE.FindStringSubmatch(x); m != nil {
			if m[1] == "time" {
				return fixedTime
			}
			return fixedUUIDv7(m[2])
		}
		return strings.ReplaceAll(x, f.goldenID, f.pkg)
	})
}

// fixedUUIDv7 derives a stable UUIDv7-shaped value from a name.
func fixedUUIDv7(name string) string {
	h := sha256.Sum256([]byte(name))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// closeConn drops the connection without an HTTP response.
func closeConn(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

// copyTree copies a fixture raw dir; symlinks are recreated, not followed.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0o644)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
}

// envWithout returns the environment minus variables with the given prefixes.
func envWithout(prefixes ...string) []string {
	var out []string
next:
	for _, kv := range os.Environ() {
		for _, p := range prefixes {
			if strings.HasPrefix(kv, p) {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}
