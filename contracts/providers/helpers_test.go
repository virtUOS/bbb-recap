// Package providers holds the black-box contract tests for the provider
// adapters (docs/reference/providers.md §6). The test IDs are the subtest names;
// the I/O of `recap tool asr` and `recap tool llm` is defined in README.md in
// this directory.
package providers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testdataDir is relative to this package directory (go test runs tests there).
const testdataDir = "../../testdata"

// toolTimeout bounds one tool run.
const toolTimeout = 2 * time.Minute

// testdataPath returns the absolute path of a file below testdata/.
func testdataPath(t *testing.T, elem ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{testdataDir}, elem...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// fixtureKey returns a fixture provider credential, trimmed of the trailing
// newline as the service trims secret files (deployment.md §5).
func fixtureKey(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", name))))
}

// --- fake provider server -------------------------------------------------

// reply is one scripted answer of a fake provider.
type reply struct {
	status int
	header map[string]string
	body   []byte
	hold   time.Duration // wait this long before answering, or until the client gives up
}

// exchange is one request a fake provider received.
type exchange struct {
	method, path string
	header       http.Header
	body         []byte
}

// fakeProvider is an HTTPS server on 127.0.0.1 that answers with scripted
// replies in order (the last one repeats) and records every request.
type fakeProvider struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []reply
	got     []exchange
}

func startFake(t *testing.T, replies ...reply) *fakeProvider {
	t.Helper()
	if len(replies) == 0 {
		t.Fatal("test helper: fake provider needs at least one reply")
	}
	f := &fakeProvider{replies: replies}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // TLS handshake failures are expected in one test
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	n := len(f.got)
	f.got = append(f.got, exchange{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	rep := f.replies[min(n, len(f.replies)-1)]
	f.mu.Unlock()
	if rep.hold > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(rep.hold):
		}
	}
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)
}

// requests returns the requests received so far.
func (f *fakeProvider) requests() []exchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exchange(nil), f.got...)
}

// url is the server's base URL, https://127.0.0.1:<port>.
func (f *fakeProvider) url() string { return f.srv.URL }

// certFile writes the server's certificate as PEM for SSL_CERT_FILE.
func (f *fakeProvider) certFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// otherCertFile writes a freshly generated self-signed certificate as PEM.
// Every httptest server uses the same built-in certificate, so a second fake
// server cannot stand in for an untrusted CA.
func otherCertFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "untrusted.recap.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other-ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// closedURL returns an https URL on 127.0.0.1 where nothing listens.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "https://" + addr
}

// --- provider profiles ----------------------------------------------------

// profiles is the text of testdata/providers/profiles/providers.yaml, one key
// per line, with helpers to change single keys.
type profiles struct{ lines []string }

func loadProfiles(t *testing.T) profiles {
	t.Helper()
	text := string(readFile(t, testdataPath(t, "providers", "profiles", "providers.yaml")))
	// Credentials: the fixture names the in-container path; use testdata/secrets.
	text = strings.ReplaceAll(text, "/run/secrets/recap/providers/", testdataPath(t, "secrets")+"/")
	return profiles{lines: strings.Split(text, "\n")}
}

// find returns the index of the n-th line (1-based) whose key is key.
func (p profiles) find(t *testing.T, key string, n int) int {
	t.Helper()
	seen := 0
	for i, l := range p.lines {
		if strings.HasPrefix(strings.TrimSpace(l), key+":") {
			if seen++; seen == n {
				return i
			}
		}
	}
	t.Fatalf("test helper: key %q (occurrence %d) not in profiles fixture", key, n)
	return -1
}

func indent(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// set returns a copy with the n-th key line's value replaced.
func (p profiles) set(t *testing.T, key string, n int, value string) profiles {
	t.Helper()
	i := p.find(t, key, n)
	out := append([]string(nil), p.lines...)
	out[i] = strings.Repeat(" ", indent(out[i])) + key + ": " + value
	return profiles{lines: out}
}

// without returns a copy with the n-th key line and its nested lines removed.
func (p profiles) without(t *testing.T, key string, n int) profiles {
	t.Helper()
	i := p.find(t, key, n)
	j := i + 1
	for j < len(p.lines) && strings.TrimSpace(p.lines[j]) != "" && indent(p.lines[j]) > indent(p.lines[i]) {
		j++
	}
	out := append(append([]string(nil), p.lines[:i]...), p.lines[j:]...)
	return profiles{lines: out}
}

// with returns a copy with a key line inserted after the n-th key line, at
// that line's indentation.
func (p profiles) with(t *testing.T, after string, n int, line string) profiles {
	t.Helper()
	i := p.find(t, after, n)
	out := append([]string(nil), p.lines[:i+1]...)
	out = append(out, strings.Repeat(" ", indent(p.lines[i]))+line)
	out = append(out, p.lines[i+1:]...)
	return profiles{lines: out}
}

// write stores the profiles in a temp file and returns its path.
func (p profiles) write(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(p.lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Occurrences of repeated keys in the profiles fixture.
const (
	asrProfile = 1 // first profile: whisperx-lab
	llmProfile = 2 // second profile: vllm-lab
)

// --- running the tool -----------------------------------------------------

// toolEnv is the environment of one tool run: the variables of README.md
// on top of the test's environment without any RECAP_*, SSL_CERT_FILE or
// *_PROXY variable, plus NO_PROXY=* (and no_proxy=*).
type toolEnv struct {
	providersFile string
	certFile      string
	asrLanguage   string
}

func (e toolEnv) list() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		// Proxy variables are dropped, so the local fakes are always reached
		// directly, whatever language the tool is written in.
		if strings.HasPrefix(name, "RECAP_") || name == "SSL_CERT_FILE" || strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "NO_PROXY=*", "no_proxy=*")
	env = append(env, "RECAP_LOG_LEVEL=debug", "RECAP_PROVIDERS_FILE="+e.providersFile)
	if e.certFile != "" {
		env = append(env, "SSL_CERT_FILE="+e.certFile)
	}
	if e.asrLanguage != "" {
		env = append(env, "RECAP_ASR_LANGUAGE="+e.asrLanguage)
	}
	return env
}

// execTool runs `recap tool <args...>` with stdin and returns stdout, stderr
// and the exit code.
func execTool(t *testing.T, bin string, env toolEnv, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"tool"}, args...)...)
	cmd.Env = env.list()
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), errOut.Bytes(), 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() > 0:
		return out.Bytes(), errOut.Bytes(), exitErr.ExitCode()
	}
	t.Fatalf("recap tool %s: %v\nstderr: %s", strings.Join(args, " "), err, errOut.String())
	return nil, nil, -1
}

// runJSON runs the tool, requires exit 0 and decodes stdout into out. It
// returns stdout and stderr for further checks.
func runJSON(t *testing.T, bin string, env toolEnv, in any, out any, args ...string) (stdout, stderr []byte) {
	t.Helper()
	stdin, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := execTool(t, bin, env, stdin, args...)
	if code != 0 {
		t.Fatalf("recap tool %s: exit %d, want 0\nstderr: %s", strings.Join(args, " "), code, stderr)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("recap tool %s: stdout is not the expected JSON object: %v\nstdout: %q", strings.Join(args, " "), err, stdout)
	}
	return stdout, stderr
}

// adapterError is an adapter error in the tool output (README.md).
type adapterError struct {
	Class        string `json:"class"`
	Code         string `json:"code"`
	RetryAfterMS *int64 `json:"retry_after_ms"`
}

// expectNotLeaked fails if any secret or canary appears in the tool's stdout
// or stderr (logs) (privacy.md §6; providers.md §1.4).
func expectNotLeaked(t *testing.T, stdout, stderr []byte, values ...string) {
	t.Helper()
	for _, v := range values {
		if bytes.Contains(stdout, []byte(v)) {
			t.Errorf("tool output contains %q", v)
		}
		if bytes.Contains(stderr, []byte(v)) {
			t.Errorf("tool log (stderr) contains %q", v)
		}
	}
}

// expectNoContentAtInfo scans the tool's log (stderr, JSON lines) and fails
// if a line at INFO or above contains one of the content values (README
// "Logging"; providers.md §6). A line is skipped only when it is a JSON
// object whose "level" is debug; any other line counts as INFO or above.
func expectNoContentAtInfo(t *testing.T, stderr []byte, values ...string) {
	t.Helper()
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(line, &rec) == nil && strings.EqualFold(rec.Level, "debug") {
			continue
		}
		for _, v := range values {
			if bytes.Contains(line, []byte(v)) {
				t.Errorf("log line at INFO or above contains content %q", v)
			}
		}
	}
}

// jsonEqual compares two JSON values semantically.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
