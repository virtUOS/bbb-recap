package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Paths relative to this package directory (go test runs tests there).
const (
	testdataDir    = "../../testdata"
	transcriptsDir = "../../testdata/transcripts"
	schemaFile     = "../../docs/reference/schemas/transcript.schema.json"
)

// The cases under testdata/transcripts/ (tools/ref/make_transcripts.py).
var caseNames = []string{"F-01", "F-01-rejoin", "F-01-gaps", "F-01-clip"}

// segmentToleranceMS is the allowed deviation of segment times until the skew
// and muted stretches are measured on a real BBB (providers.md §4.3 item 4, §7).
const segmentToleranceMS = 1000

// --- running the tool ---

// execTool runs `recap tool <name> args...` with extra environment variables
// and returns stdout, stderr and the exit code.
func execTool(t *testing.T, bin string, env []string, stdin []byte, name string, args ...string) (stdout, stderr []byte, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"tool", name}, args...)...)
	cmd.Env = append(os.Environ(), env...)
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
	t.Fatalf("recap tool %s %s: %v\nstderr: %s", name, strings.Join(args, " "), err, errOut.String())
	return nil, nil, -1
}

// runTool runs the tool, fails the test unless it exits 0, and returns stdout.
func runTool(t *testing.T, bin string, stdin []byte, name string, args ...string) []byte {
	t.Helper()
	stdout, stderr, code := execTool(t, bin, nil, stdin, name, args...)
	if code != 0 {
		t.Fatalf("recap tool %s %s: exit %d\nstderr: %s", name, strings.Join(args, " "), code, stderr)
	}
	return stdout
}

// check runs `recap tool assemble --check` on one transcript and returns its verdict.
func check(t *testing.T, bin string, transcript []byte, what string) bool {
	t.Helper()
	out := runTool(t, bin, transcript, "assemble", "--check")
	var v struct {
		Valid *bool `json:"valid"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Valid == nil {
		t.Fatalf("%s: stdout is not a verdict {\"valid\": bool}: %v\nstdout: %q", what, err, out)
	}
	return *v.Valid
}

// --- cases ---

// tcase is testdata/transcripts/<name>/case.json.
type tcase struct {
	RawDir  string  `json:"raw_dir"`
	Events  *string `json:"events"`
	Rename  []struct{ From, To string }
	Replace []struct {
		Path string `json:"path"`
		With string `json:"with"`
	} `json:"replace"`
	Assemble map[string]json.RawMessage `json:"assemble"`
}

func loadCase(t *testing.T, name string) tcase {
	t.Helper()
	var c tcase
	if err := json.Unmarshal(readFile(t, filepath.Join(transcriptsDir, name, "case.json")), &c); err != nil {
		t.Fatalf("case %s: %v", name, err)
	}
	if c.Assemble == nil {
		t.Fatalf("case %s: no assemble input", name)
	}
	return c
}

// inputDir returns the directory given to normalize: the raw directory of the
// fixture as it is, or a copy with the case's events.xml, renames and replaced files.
func inputDir(t *testing.T, name string, c tcase) string {
	t.Helper()
	src := filepath.Join(testdataDir, filepath.FromSlash(c.RawDir))
	if c.Events == nil && len(c.Rename) == 0 && len(c.Replace) == 0 {
		return src
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	copyTree(t, src, dst)
	caseDir := filepath.Join(transcriptsDir, name)
	if c.Events != nil {
		writeFile(t, filepath.Join(dst, "events.xml"), readFile(t, filepath.Join(caseDir, *c.Events)))
	}
	for _, r := range c.Rename {
		if err := os.Rename(filepath.Join(dst, filepath.FromSlash(r.From)), filepath.Join(dst, filepath.FromSlash(r.To))); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range c.Replace {
		writeFile(t, filepath.Join(dst, filepath.FromSlash(r.Path)), readFile(t, filepath.Join(caseDir, r.With)))
	}
	return dst
}

// copyTree copies regular files and recreates symlinks as they are.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(to, b, 0o644)
		}
	})
	if err != nil {
		t.Fatalf("copying %s: %v", src, err)
	}
}

// assembleInput is the case's assemble input plus the normalize output.
func assembleInput(t *testing.T, c tcase, normalized []byte) []byte {
	t.Helper()
	in := map[string]json.RawMessage{"normalized": normalized}
	for k, v := range c.Assemble {
		in[k] = v
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// decode parses JSON keeping numbers as json.Number, so floats stay visible.
func decode(t *testing.T, b []byte, what string) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("%s: not JSON: %v", what, err)
	}
	if d.More() {
		t.Fatalf("%s: more than one JSON value", what)
	}
	return v
}

// --- schema and rules (providers.md §4.2) ---

// schemaChecker validates against transcript.schema.json. It knows exactly the
// keywords that schema uses and fails on any other, so it never skips a rule.
type schemaChecker struct {
	defs map[string]any
}

func loadSchema(t *testing.T) (*schemaChecker, map[string]any) {
	t.Helper()
	s, ok := decode(t, readFile(t, schemaFile), "schema").(map[string]any)
	if !ok {
		t.Fatal("schema is not an object")
	}
	if s["$id"] != "urn:recap:schema:transcript:1" {
		t.Fatalf("schema $id %v, want urn:recap:schema:transcript:1", s["$id"])
	}
	defs, _ := s["$defs"].(map[string]any)
	return &schemaChecker{defs: defs}, s
}

var annotationKeys = map[string]bool{"$schema": true, "$id": true, "$defs": true, "title": true, "description": true}

func isInteger(n json.Number) bool { return !strings.ContainsAny(string(n), ".eE") }

func (c *schemaChecker) errors(schema map[string]any, v any, path string) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, path+": "+fmt.Sprintf(format, args...)) }
	for key, kw := range schema {
		switch key {
		case "$ref":
			name := strings.TrimPrefix(kw.(string), "#/$defs/")
			def, ok := c.defs[name].(map[string]any)
			if !ok {
				fail("unknown $ref %v", kw)
				continue
			}
			errs = append(errs, c.errors(def, v, path)...)
		case "type":
			if !hasType(kw.(string), v) {
				fail("want type %s, got %v", kw, v)
			}
		case "const":
			if !reflect.DeepEqual(kw, v) {
				fail("want %v", kw)
			}
		case "enum":
			found := false
			for _, e := range kw.([]any) {
				found = found || reflect.DeepEqual(e, v)
			}
			if !found {
				fail("%v not in %v", v, kw)
			}
		case "oneOf":
			n := 0
			for _, sub := range kw.([]any) {
				if len(c.errors(sub.(map[string]any), v, path)) == 0 {
					n++
				}
			}
			if n != 1 {
				fail("matches %d of oneOf, want 1", n)
			}
		case "required":
			if m, ok := v.(map[string]any); ok {
				for _, r := range kw.([]any) {
					if _, ok := m[r.(string)]; !ok {
						fail("missing %v", r)
					}
				}
			}
		case "properties":
			if m, ok := v.(map[string]any); ok {
				props := kw.(map[string]any)
				for k, pv := range m {
					if ps, ok := props[k]; ok {
						errs = append(errs, c.errors(ps.(map[string]any), pv, path+"/"+k)...)
					}
				}
			}
		case "additionalProperties":
			if kw != false {
				fail("checker supports additionalProperties: false only")
				continue
			}
			props, _ := schema["properties"].(map[string]any)
			if m, ok := v.(map[string]any); ok {
				for k := range m {
					if _, ok := props[k]; !ok {
						fail("unknown field %q", k)
					}
				}
			}
		case "items":
			if a, ok := v.([]any); ok {
				for i, item := range a {
					errs = append(errs, c.errors(kw.(map[string]any), item, fmt.Sprintf("%s/%d", path, i))...)
				}
			}
		case "minItems":
			if a, ok := v.([]any); ok && int64(len(a)) < num(kw) {
				fail("fewer than %v items", kw)
			}
		case "pattern":
			if s, ok := v.(string); ok && !regexp.MustCompile(kw.(string)).MatchString(s) {
				fail("%q does not match %s", s, kw)
			}
		case "minLength", "maxLength":
			if s, ok := v.(string); ok {
				n := int64(len([]rune(s)))
				if (key == "minLength" && n < num(kw)) || (key == "maxLength" && n > num(kw)) {
					fail("length %d violates %s %v", n, key, kw)
				}
			}
		case "minimum", "maximum":
			if n, ok := v.(json.Number); ok {
				f, _ := n.Float64()
				lim, _ := kw.(json.Number).Float64()
				if (key == "minimum" && f < lim) || (key == "maximum" && f > lim) {
					fail("%v violates %s %v", n, key, kw)
				}
			}
		case "format":
			if kw != "date-time" {
				fail("checker supports format date-time only")
			} else if s, ok := v.(string); ok {
				if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
					fail("%q is not an RFC 3339 date-time", s)
				}
			}
		default:
			if !annotationKeys[key] {
				fail("schema keyword %q not supported by the test checker", key)
			}
		}
	}
	return errs
}

func num(v any) int64 {
	n, _ := v.(json.Number).Int64()
	return n
}

func hasType(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		return ok && isInteger(n)
	}
	return false
}

// ruleErrors checks the rules of providers.md §4.2 beyond the schema. It
// expects a transcript that passed the schema.
func ruleErrors(tr map[string]any) []string {
	var errs []string
	list := func(k string) []map[string]any {
		var out []map[string]any
		a, _ := tr[k].([]any)
		for _, x := range a {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	ms := func(m map[string]any, k string) int64 { return num(m[k]) }
	speakers := map[any]bool{}
	for _, s := range list("speakers") {
		speakers[s["ref"]] = true
	}
	tracks := map[any]map[string]any{}
	for _, tk := range list("tracks") {
		tracks[tk["ref"]] = tk
		if !speakers[tk["speaker_ref"]] {
			errs = append(errs, fmt.Sprintf("track %v: speaker_ref %v does not resolve", tk["ref"], tk["speaker_ref"]))
		}
	}
	spans := append(append(append(list("recording_periods"), list("tracks")...), list("segments")...), list("gaps")...)
	for _, s := range list("segments") {
		w, _ := s["words"].([]any)
		for _, x := range w {
			spans = append(spans, x.(map[string]any))
		}
	}
	for _, x := range spans {
		if ms(x, "end_ms") < ms(x, "start_ms") {
			errs = append(errs, fmt.Sprintf("end_ms < start_ms in %v", x))
		}
	}
	for _, kind := range []string{"segments", "gaps"} {
		for _, x := range list(kind) {
			tk, ok := tracks[x["track_ref"]]
			switch {
			case !ok:
				errs = append(errs, fmt.Sprintf("%s: track_ref %v does not resolve", kind, x["track_ref"]))
			case !speakers[x["speaker_ref"]]:
				errs = append(errs, fmt.Sprintf("%s: speaker_ref %v does not resolve", kind, x["speaker_ref"]))
			case x["speaker_ref"] != tk["speaker_ref"]:
				errs = append(errs, fmt.Sprintf("%s: speaker_ref %v differs from track %v's", kind, x["speaker_ref"], x["track_ref"]))
			case kind == "gaps" && tk["status"] != "failed":
				errs = append(errs, fmt.Sprintf("gap on track %v with status %v", x["track_ref"], tk["status"]))
			}
		}
	}
	periods := list("recording_periods")
	for i := 1; i < len(periods); i++ {
		if ms(periods[i], "start_ms") < ms(periods[i-1], "end_ms") {
			errs = append(errs, fmt.Sprintf("recording period %d is unsorted or overlaps the one before", i))
		}
	}
	// docs/reference/README.md "Units and time": timestamps are RFC 3339 UTC.
	tb, _ := tr["time_base"].(map[string]any)
	for _, v := range []any{tr["created_at"], tb["origin_utc"]} {
		if s, _ := v.(string); !isUTC(s) {
			errs = append(errs, fmt.Sprintf("time %q is not UTC", s))
		}
	}
	// Order and content rules of the §4.2 table.
	byRef := func(m map[string]any) []int64 { return []int64{int64(refNum(m["ref"]))} }
	byStart := func(m map[string]any) []int64 { return []int64{ms(m, "start_ms"), int64(refNum(m["track_ref"]))} }
	for _, o := range []struct {
		kind string
		key  func(map[string]any) []int64
	}{{"speakers", byRef}, {"tracks", byRef}, {"segments", byStart}, {"gaps", byStart}} {
		items := list(o.kind)
		for i := 1; i < len(items); i++ {
			if slices.Compare(o.key(items[i-1]), o.key(items[i])) > 0 {
				errs = append(errs, fmt.Sprintf("%s: item %d is out of order", o.kind, i))
			}
		}
	}
	withTrack := map[any]bool{}
	for _, tk := range list("tracks") {
		withTrack[tk["speaker_ref"]] = true
		if tk["status"] == "failed" && (tk["language"] != nil || tk["provider"] != nil) {
			errs = append(errs, fmt.Sprintf("failed track %v has a language or provider", tk["ref"]))
		}
	}
	for _, sp := range list("speakers") {
		if !withTrack[sp["ref"]] {
			errs = append(errs, fmt.Sprintf("speaker %v has no microphone track", sp["ref"]))
		}
	}
	return errs
}

// validTranscript returns the schema errors and the rule errors of one transcript.
func validTranscript(t *testing.T, c *schemaChecker, schema map[string]any, b []byte, what string) (schemaErrs, ruleErrs []string) {
	t.Helper()
	v := decode(t, b, what)
	schemaErrs = c.errors(schema, v, "")
	if len(schemaErrs) == 0 {
		ruleErrs = ruleErrors(v.(map[string]any))
	}
	return schemaErrs, ruleErrs
}

// --- comparing with the golden ---

// comparable rewrites a transcript into the form compared with the golden:
// the two date-time strings as instants (their textual form is not pinned) and
// segment times split off for the tolerance check. Everything else, including
// the order of every array, is compared exactly.
func comparable(t *testing.T, tr map[string]any) (map[string]any, [][2]int64) {
	t.Helper()
	out := maps.Clone(tr)
	out["created_at"] = instant(t, tr["created_at"])
	tb, _ := tr["time_base"].(map[string]any)
	tb = maps.Clone(tb)
	tb["origin_utc"] = instant(t, tb["origin_utc"])
	out["time_base"] = tb
	var segs []any
	var times [][2]int64
	a, _ := tr["segments"].([]any)
	for _, x := range a {
		m := maps.Clone(x.(map[string]any))
		times = append(times, [2]int64{num(m["start_ms"]), num(m["end_ms"])})
		delete(m, "start_ms")
		delete(m, "end_ms")
		segs = append(segs, m)
	}
	out["segments"] = segs
	return out, times
}

func instant(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	if !isUTC(s) {
		t.Fatalf("time %q: not UTC (docs/reference/README.md \"Units and time\": RFC 3339 UTC)", s)
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

// isUTC reports whether an RFC 3339 time string has the offset Z or +00:00.
func isUTC(s string) bool {
	return strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "+00:00")
}

func refNum(v any) int {
	s, _ := v.(string)
	n, _ := strconv.Atoi(s[strings.IndexByte(s, '-')+1:])
	return n
}

// compareGolden fails the test where got differs from the golden transcript.
func compareGolden(t *testing.T, got, want map[string]any, what string) {
	t.Helper()
	g, gTimes := comparable(t, got)
	w, wTimes := comparable(t, want)
	keys := slices.Sorted(maps.Keys(w))
	for k := range g {
		if _, ok := w[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if !reflect.DeepEqual(g[k], w[k]) {
			gb, _ := json.MarshalIndent(g[k], "", " ")
			wb, _ := json.MarshalIndent(w[k], "", " ")
			t.Errorf("%s: %s differs from the golden\ngot:  %s\nwant: %s", what, k, gb, wb)
		}
	}
	if len(gTimes) != len(wTimes) {
		return // reported above as a segments difference
	}
	for i := range wTimes {
		for j, name := range []string{"start_ms", "end_ms"} {
			if d := gTimes[i][j] - wTimes[i][j]; d > segmentToleranceMS || d < -segmentToleranceMS {
				t.Errorf("%s: segment %d %s = %d, golden %d (tolerance %d ms)", what, i, name, gTimes[i][j], wTimes[i][j], segmentToleranceMS)
			}
		}
	}
}
