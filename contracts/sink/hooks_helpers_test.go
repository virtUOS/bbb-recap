package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Helpers for the webhook and Matrix sink tests (sinks.md §4b, §4c; ADR-016).
// They keep their own stderr buffer, so the log scan of CT-SINK-10 sees only
// the Docs runs it was written for.

// link is one entry of the delivery request's links (sinks.md §1.2).
type link struct {
	SinkProfile string `json:"sink_profile"`
	DocumentURL string `json:"document_url"`
}

// hookRequest is the delivery request of sinks.md §1.2 with the fields that
// ADR-016 adds: links and matrix_room.
type hookRequest struct {
	deliveryRequest
	Links      []link  `json:"links"`
	MatrixRoom *string `json:"matrix_room"`
}

// hookInput is the stdin of `recap tool sink` for the webhook and Matrix sinks.
type hookInput struct {
	Request          hookRequest `json:"request"`
	Delivery         *record     `json:"delivery"`
	RequestTimeoutMS *int64      `json:"request_timeout_ms,omitempty"`
}

// docsLink is the link the webhook and Matrix tests pass in links: a Docs
// document of the same job (fixtures.md §5 host).
var docsLink = link{SinkProfile: "docs-main", DocumentURL: "https://docs.recap.test/docs/0b8f2a3e-5c41-4d7a-9e26-3f1c7a9d2b10/"}

// hookReq is testdata/sink/request.json with summary.md for the profile name,
// one Docs link and the room (nil for none).
func hookReq(t *testing.T, profile string, room *string) hookRequest {
	t.Helper()
	r := baseRequest(t)
	r.SinkProfile = profile
	return hookRequest{deliveryRequest: r, Links: []link{docsLink}, MatrixRoom: room}
}

func strp(s string) *string { return &s }

// hookStderr collects the stderr of the webhook and Matrix tool runs.
var hookStderr struct {
	sync.Mutex
	buf bytes.Buffer
}

// newProfileEnv writes testdata/sink/<file> with the key directory set to
// testdata/secrets/ and key (url or homeserver_url) set to value, and the
// mock's certificate for SSL_CERT_FILE.
func newProfileEnv(t *testing.T, file string, srv *httptest.Server, key, value string) sinkEnv {
	t.Helper()
	bin := harness.Bin(t)
	dir := t.TempDir()
	text := string(readFile(t, testdataPath(t, "sink", file)))
	text = strings.ReplaceAll(text, "/run/secrets/recap/sinks/", testdataPath(t, "secrets")+"/")
	text = replaceKey(t, text, key, value)
	e := sinkEnv{bin: bin, sinksFile: filepath.Join(dir, "sinks.yaml"), certFile: filepath.Join(dir, "ca.pem"), text: text}
	writeFile(t, e.sinksFile, []byte(text))
	writeFile(t, e.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return e
}

// withoutDeclaration returns a copy of e whose profile has no declaration
// block (the fixture files keep it last).
func (e sinkEnv) withoutDeclaration(t *testing.T) sinkEnv {
	t.Helper()
	i := strings.Index(e.text, "\n    declaration:")
	if i < 0 {
		t.Fatal("sinks file has no declaration block")
	}
	c := e
	c.text = e.text[:i+1]
	c.sinksFile = filepath.Join(t.TempDir(), "sinks.yaml")
	writeFile(t, c.sinksFile, []byte(c.text))
	return c
}

// runHookRaw runs `recap tool sink` with in and returns the exit code,
// stdout and stderr.
func (e sinkEnv) runHookRaw(t *testing.T, in hookInput) (int, []byte, []byte) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "tool", "sink")
	cmd.Env = append(withoutRecapVars(os.Environ()),
		"RECAP_SINKS_FILE="+e.sinksFile, "RECAP_LOG_LEVEL=debug", "SSL_CERT_FILE="+e.certFile)
	cmd.Stdin = bytes.NewReader(b)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		// A usage or configuration error need not be a log line (README.md
		// "Output"), so it stays out of the log scan.
		return exitErr.ExitCode(), out.Bytes(), errOut.Bytes()
	case err != nil:
		t.Fatalf("recap tool sink: %v", err)
	}
	hookStderr.Lock()
	hookStderr.buf.Write(errOut.Bytes())
	hookStderr.buf.WriteByte('\n')
	hookStderr.Unlock()
	return 0, out.Bytes(), errOut.Bytes()
}

// runHook runs the tool and expects exit 0 with one JSON object on stdout.
// The output never holds owner values (the webhook and Matrix sinks do not
// use the owner).
func (e sinkEnv) runHook(t *testing.T, in hookInput) output {
	t.Helper()
	code, stdout, stderr := e.runHookRaw(t, in)
	if code != 0 {
		t.Fatalf("recap tool sink: exit %d, want 0\nstderr: %s", code, stderr)
	}
	var o output
	d := json.NewDecoder(bytes.NewReader(stdout))
	if err := d.Decode(&o); err != nil || d.More() {
		t.Fatalf("recap tool sink: stdout is not one JSON object: %v\nstdout: %q", err, stdout)
	}
	o.raw = stdout
	expectNoCanary(t, "tool stdout", o.raw, identityCanaries)
	return o
}

// expectSentNoDocument checks an ok result with an empty document_id.
func expectSentNoDocument(t *testing.T, o output, req hookRequest) {
	t.Helper()
	if o.Outcome != "ok" || o.Error != nil {
		t.Fatalf("got %s, want outcome ok without error", o.raw)
	}
	r := o.Delivery
	if r == nil || r.State != "sent" || r.JobID != req.JobID || r.SinkProfile != req.SinkProfile || str(r.DocumentID) != "" {
		t.Fatalf("delivery record %s, want state sent, job_id %s, sink_profile %s, empty document_id", o.raw, req.JobID, req.SinkProfile)
	}
}

// expectDeclarationRequired checks that a profile without declaration stops
// the tool at the configuration with exit 2 and names the declaration (privacy.md §7, §11; sinks.md §4b.4,
// §4c.5). A control run with the complete profile must pass first, so the
// exit 2 cannot come from an unknown sink type.
func expectDeclarationRequired(t *testing.T, e sinkEnv, in hookInput) {
	t.Helper()
	e.runHook(t, in) // control: the complete profile works
	code, stdout, stderr := e.withoutDeclaration(t).runHookRaw(t, in)
	if code != 2 || len(bytes.TrimSpace(stdout)) != 0 {
		t.Fatalf("profile without declaration: exit %d, stdout %q; want exit 2 and empty stdout", code, stdout)
	}
	if !strings.Contains(strings.ToLower(string(stderr)), "declaration") {
		t.Errorf("profile without declaration: stderr does not name the missing declaration:\n%s", stderr)
	}
}

// scanHookLogs checks the collected stderr of the webhook and Matrix runs:
// README "Logging" lines and none of the canaries.
func scanHookLogs(t *testing.T, canaries []string) {
	t.Helper()
	hookStderr.Lock()
	logs := append([]byte(nil), hookStderr.buf.Bytes()...)
	hookStderr.Unlock()
	expectLogLines(t, "tool stderr", logs)
	expectNoCanary(t, "tool stderr", logs, canaries)
}
