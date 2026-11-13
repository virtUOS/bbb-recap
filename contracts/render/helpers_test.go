package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Paths relative to this package directory (go test runs tests there).
const (
	testdataDir = "../../testdata"
	renderDir   = "../../testdata/render"
	formatsDir  = "../../formats" // the repo's formats, used when RECAP_FORMATS_DIR is unset
)

// meetingTypesYAML is the default and fixture meeting-types file (templates.md §1).
const meetingTypesYAML = `schema_version: "1.0.0"
meeting_types:
  - name: meeting-summary    # value of meta_recap-type
    format: meeting-summary  # format ID under RECAP_FORMATS_DIR
`

// Environment variables the tests set for every run. Everything else, in
// particular RECAP_FORMATS_DIR, is passed through from the test environment;
// an unset or empty RECAP_FORMATS_DIR becomes the repo's formats/ (execBin).
var ownEnv = []string{
	"RECAP_MEETING_TYPES_FILE=", "RECAP_DISPLAY_TZ=", "RECAP_DEFAULT_LANGUAGE=",
	"RECAP_PROVIDERS_FILE=", "RECAP_ASR_LANGUAGE=", "SSL_CERT_FILE=",
}

// result is the JSON object `recap tool render` writes to stdout (README.md).
type result struct {
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason"`
	FormatID      string `json:"format_id"`
	FormatVersion string `json:"format_version"`
	FormatSHA256  string `json:"format_sha256"`
	Language      string `json:"language"`
	Title         string `json:"title"`
}

// rendered is one run of the tool: its verdict and the files it wrote.
type rendered struct {
	result
	outDir     string
	summaryMD  []byte
	transcript []byte // transcript.vtt
}

// run is one invocation of `recap tool render`.
type run struct {
	args  []string          // extra arguments, e.g. --summarize
	env   map[string]string // overrides of the defaults below
	input any               // marshalled to stdin
}

// renderTool runs `recap tool render --out DIR [args]` and returns the
// decoded stdout. Defaults: the fixture meeting-types file,
// RECAP_DISPLAY_TZ=UTC, RECAP_DEFAULT_LANGUAGE=en (fixtures.md §3).
func renderTool(t *testing.T, bin string, r run) rendered {
	t.Helper()
	tmp := t.TempDir()
	types := filepath.Join(tmp, "meeting-types.yaml")
	writeFile(t, types, []byte(meetingTypesYAML))
	env := map[string]string{
		"RECAP_MEETING_TYPES_FILE": types,
		"RECAP_DISPLAY_TZ":         "UTC",
		"RECAP_DEFAULT_LANGUAGE":   "en",
	}
	for k, v := range r.env {
		env[k] = v
	}
	stdin, err := json.Marshal(r.input)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"tool", "render", "--out", out}, r.args...)
	stdout, stderr, code := execBin(t, bin, env, stdin, args...)
	if code != 0 {
		t.Fatalf("recap %s: exit %d (want 0 for every outcome)\nstderr: %s", strings.Join(args, " "), code, stderr)
	}
	var res rendered
	if err := strictUnmarshal(stdout, &res.result); err != nil {
		t.Fatalf("recap tool render: stdout is not one JSON object: %v\nstdout: %q\nstderr: %s", err, stdout, stderr)
	}
	res.outDir = out
	return res
}

// renderOK runs the tool, expects outcome ok, reads both outputs and checks
// transcript.vtt with the WebVTT validator (every rendered .vtt is checked).
func renderOK(t *testing.T, bin string, r run) rendered {
	t.Helper()
	res := renderTool(t, bin, r)
	if res.Outcome != "ok" {
		t.Fatalf("outcome = %q (reason %q), want ok", res.Outcome, res.Reason)
	}
	res.summaryMD = readFile(t, filepath.Join(res.outDir, "summary.md"))
	res.transcript = readFile(t, filepath.Join(res.outDir, "transcript.vtt"))
	for _, e := range validateVTT(res.transcript) {
		t.Errorf("rendered transcript.vtt breaks WebVTT syntax (templates.md §6): %s", e)
	}
	return res
}

// execBin runs bin with the given environment overrides and stdin.
func execBin(t *testing.T, bin string, env map[string]string, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = envWithout(ownEnv)
	if os.Getenv("RECAP_FORMATS_DIR") == "" && env["RECAP_FORMATS_DIR"] == "" {
		dir, err := filepath.Abs(formatsDir)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "RECAP_FORMATS_DIR="+dir)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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
	t.Fatalf("recap %s: %v\nstderr: %s", strings.Join(args, " "), err, errOut.String())
	return nil, nil, -1
}

// envWithout returns the test environment without the given KEY= entries.
func envWithout(drop []string) []string {
	var out []string
next:
	for _, kv := range os.Environ() {
		for _, p := range drop {
			if strings.HasPrefix(kv, p) {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// ---------------------------------------------------------------- fixtures

// object is a JSON object read from a fixture, with numbers kept exact.
type object = map[string]any

// renderCase is one input directory below testdata/render/.
type renderCase struct {
	context, transcript, summary object
}

// loadCase reads testdata/render/<name>/{context,transcript,summary}.json.
// A missing summary.json leaves summary nil.
func loadCase(t *testing.T, name string) renderCase {
	t.Helper()
	c := renderCase{
		context:    loadObject(t, renderDir, name, "context.json"),
		transcript: loadObject(t, renderDir, name, "transcript.json"),
	}
	if _, err := os.Stat(filepath.Join(renderDir, name, "summary.json")); err == nil {
		c.summary = loadObject(t, renderDir, name, "summary.json")
	}
	return c
}

// input builds the tool's stdin: the context fields plus transcript and,
// unless nil, summary (README.md "Input").
func (c renderCase) input() object {
	in := object{}
	for k, v := range c.context {
		in[k] = v
	}
	in["transcript"] = c.transcript
	if c.summary != nil {
		in["summary"] = c.summary
	}
	return in
}

// clone returns a deep copy, so a test can change it freely.
func (c renderCase) clone(t *testing.T) renderCase {
	t.Helper()
	return renderCase{context: deepCopy(t, c.context), transcript: deepCopy(t, c.transcript), summary: deepCopy(t, c.summary)}
}

func deepCopy(t *testing.T, o object) object {
	t.Helper()
	if o == nil {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return decodeObject(t, b, "copy")
}

func loadObject(t *testing.T, elem ...string) object {
	t.Helper()
	p := filepath.Join(elem...)
	return decodeObject(t, readFile(t, p), p)
}

func decodeObject(t *testing.T, b []byte, name string) object {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o object
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return o
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// golden reads testdata/render/<elem...>.
func golden(t *testing.T, elem ...string) []byte {
	t.Helper()
	return readFile(t, filepath.Join(append([]string{renderDir}, elem...)...))
}

// wantBytes fails with a line diff when got differs from want.
func wantBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl || i >= len(g) || i >= len(w) {
			t.Errorf("%s differs from the golden at line %d:\ngot  %q\nwant %q\n(got %d bytes, want %d)", what, i+1, gl, wl, len(got), len(want))
			return
		}
	}
	t.Errorf("%s differs from the golden (got %d bytes, want %d)", what, len(got), len(want))
}

// lines returns the lines of b (without the final empty one).
func lines(b []byte) []string {
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
