package meetingpackage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// f01RecordID is the record_id of fixture F-01 (docs/reference/fixtures.md §2).
const f01RecordID = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"

// fileEntry is one manifest files entry (meeting-package.md §4).
type fileEntry struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// violation is the first rule a package breaks (README.md in this directory).
type violation struct {
	Rule     string `json:"rule"`
	Problem  string `json:"problem"`
	Status   int    `json:"status"`
	JobState string `json:"job_state"`
	Reason   string `json:"reason"`
}

// verdict is the JSON object `recap tool validate-package` writes to stdout.
type verdict struct {
	Valid     *bool       `json:"valid"`
	PackageID *string     `json:"package_id"`
	Files     []fileEntry `json:"files"`
	Gaps      *[]string   `json:"gaps"`
	Violation *violation  `json:"violation"`
}

// ingestOutcome is the expected result of an ingest rule (meeting-package.md §7).
type ingestOutcome struct {
	rule, problem string
	status        int
}

// execTool runs `recap tool validate-package args...` and returns stdout,
// stderr and the exit code.
func execTool(t *testing.T, bin string, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"tool", "validate-package"}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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
	t.Fatalf("recap tool validate-package %s: %v\nstderr: %s", strings.Join(args, " "), err, errOut.String())
	return nil, nil, -1
}

// runTool runs the tool and returns stdout. A non-zero exit fails the test:
// the tool exits 0 for every verdict.
func runTool(t *testing.T, bin string, stdin []byte, args ...string) []byte {
	t.Helper()
	stdout, stderr, code := execTool(t, bin, stdin, args...)
	if code != 0 {
		t.Fatalf("recap tool validate-package %s: exit %d\nstderr: %s", strings.Join(args, " "), code, stderr)
	}
	return stdout
}

// validate runs the tool and decodes its verdict.
func validate(t *testing.T, bin string, args ...string) verdict {
	t.Helper()
	out := runTool(t, bin, nil, args...)
	var v verdict
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("validate-package %s: stdout is not one JSON object: %v\nstdout: %q", strings.Join(args, " "), err, out)
	}
	if v.Valid == nil {
		t.Fatalf("validate-package %s: no \"valid\" field in %s", strings.Join(args, " "), out)
	}
	if *v.Valid != (v.Violation == nil) {
		t.Fatalf("validate-package %s: valid=%v but violation=%+v", strings.Join(args, " "), *v.Valid, v.Violation)
	}
	return v
}

// canonicalize runs the tool's JCS form on input.
func canonicalize(t *testing.T, bin string, input []byte) []byte {
	t.Helper()
	return runTool(t, bin, input, "--jcs")
}

func wantValid(t *testing.T, v verdict, wantGaps []string) {
	t.Helper()
	if !*v.Valid {
		t.Fatalf("want valid, got violation %+v", *v.Violation)
	}
	if v.Gaps == nil {
		t.Fatalf("valid verdict without a \"gaps\" array")
	}
	if !slices.Equal(*v.Gaps, wantGaps) { // nil and empty are equal
		t.Fatalf("gaps = %q, want %q", *v.Gaps, wantGaps)
	}
}

func wantIngestViolation(t *testing.T, v verdict, want ingestOutcome) {
	t.Helper()
	if *v.Valid {
		t.Fatalf("want violation %s (%d %s), got valid", want.rule, want.status, want.problem)
	}
	got := *v.Violation
	if got.Rule != want.rule || got.Problem != want.problem || got.Status != want.status {
		t.Fatalf("violation = {rule %q, problem %q, status %d}, want {%q, %q, %d}",
			got.Rule, got.Problem, got.Status, want.rule, want.problem, want.status)
	}
}

func wantStepViolation(t *testing.T, v verdict, rules []string, jobState, reason string) {
	t.Helper()
	if *v.Valid {
		t.Fatalf("want violation %v (%s/%s), got valid", rules, jobState, reason)
	}
	got := *v.Violation
	if !slices.Contains(rules, got.Rule) || got.JobState != jobState || got.Reason != reason {
		t.Fatalf("violation = {rule %q, job_state %q, reason %q}, want rule in %v, %q, %q",
			got.Rule, got.JobState, got.Reason, rules, jobState, reason)
	}
}

// testdataPath returns the absolute path of a file below testdata/. go test
// runs in contracts/package, two levels below the module root.
func testdataPath(t *testing.T, elem ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{"..", "..", "testdata"}, elem...)...))
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

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatalf("no fixture matches %s", pattern)
	}
	return m
}

// rawDirFor finds the raw directory raw/<record_id> of any fixture; F-07 has
// none of its own and uses F-01's (testdata/meetings/F-07/README.md).
func rawDirFor(t *testing.T, recordID string) string {
	t.Helper()
	return glob(t, testdataPath(t, "meetings", "*", "raw", recordID))[0]
}

// goldenManifests returns testdata/meetings/*/expected/manifest.json.
func goldenManifests(t *testing.T) []string {
	t.Helper()
	return glob(t, testdataPath(t, "meetings", "*", "expected", "manifest.json"))
}

// fixtureName returns "F-NN" for a path below testdata/meetings/F-NN/.
func fixtureName(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(path)))
}

// manifest is a golden manifest parsed for mutation, with integers kept exact.
type manifest map[string]any

// loadManifest parses a canonical golden manifest and checks that the test's
// own JCS helper reproduces its bytes, so mutations built with the helper are
// canonical too.
func loadManifest(t *testing.T, path string) manifest {
	t.Helper()
	raw := readFile(t, path)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if got := canonicalJSON(t, map[string]any(m)); !bytes.Equal(got, raw) {
		t.Fatalf("test helper: canonical form of %s differs from its bytes", path)
	}
	return m
}

// files returns the manifest's files entries as a typed copy.
func (m manifest) files(t *testing.T) []fileEntry {
	t.Helper()
	b, err := json.Marshal(m["files"])
	if err != nil {
		t.Fatal(err)
	}
	var entries []fileEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// setFiles replaces files (sorted by path bytewise) and recomputes totals.
func (m manifest) setFiles(entries []fileEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var list []any
	var size int64
	for _, f := range entries {
		list = append(list, map[string]any{
			"path": f.Path, "role": f.Role, "sha256": f.SHA256, "size": json.Number(fmt.Sprint(f.Size)),
		})
		size += f.Size
	}
	m["files"] = list
	m["totals"] = map[string]any{"file_count": json.Number(fmt.Sprint(len(entries))), "size": json.Number(fmt.Sprint(size))}
}

// write stores the canonical manifest in a temp file and returns its path and
// package_id.
func (m manifest) write(t *testing.T) (path, packageID string) {
	t.Helper()
	b := canonicalJSON(t, map[string]any(m))
	path = filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, sha256Hex(b)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// copyRegularFiles copies the regular files below src into a new temp
// directory named like src (symlinks and other types are left out) and
// returns it.
func copyRegularFiles(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	return dst
}

// canonicalJSON is a minimal RFC 8785 (JCS) encoder for the value types a
// manifest uses: null, booleans, integers (json.Number), strings, arrays and
// objects. It exists only to build mutated manifests; the implementation's
// JCS is what the tests check.
func canonicalJSON(t *testing.T, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		t.Fatalf("test helper JCS: %v", err)
	}
	return b.Bytes()
}

var integerRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		fmt.Fprint(b, x)
	case json.Number:
		if !integerRE.MatchString(string(x)) || len(x) > 16 {
			return fmt.Errorf("number %s: only small integers are supported", x)
		}
		b.WriteString(string(x))
	case string:
		writeCanonicalString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// RFC 8785 §3.2.3: sort by UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool {
			return slices.Compare(utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))) < 0
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported value %T", v)
	}
	return nil
}

// writeCanonicalString follows RFC 8785 §3.2.2.2: escape only the quote, the
// backslash and U+0000..U+001F; write everything else as UTF-8.
func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
