package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Test IDs from docs/reference/providers.md §6.
func TestTranscript(t *testing.T) {
	t.Run("CT-TR-01 schema and rules", testSchemaAndRules)
	t.Run("CT-TR-02 normalize and assemble", testAssemble)
	t.Run("CT-TR-03 no content in logs", testLogs)
}

// §4.2: every golden transcript passes the schema and the rules; each file in
// testdata/transcripts/invalid/ breaks exactly one of them. The fixtures are
// first checked with the test's own checker, then with `recap tool assemble --check`.
func testSchemaAndRules(t *testing.T) {
	checker, schema := loadSchema(t)
	files := map[string]bool{} // path -> want valid
	for _, name := range caseNames {
		p := filepath.Join(transcriptsDir, name, "transcript.json")
		files[p] = true
		if s, r := validTranscript(t, checker, schema, readFile(t, p), p); len(s)+len(r) > 0 {
			t.Fatalf("golden %s is not valid: %v %v", p, s, r)
		}
	}
	invalid, err := filepath.Glob(filepath.Join(transcriptsDir, "invalid", "*.json"))
	if err != nil || len(invalid) == 0 {
		t.Fatalf("no invalid transcripts found: %v", err)
	}
	for _, p := range invalid {
		files[p] = false
		s, r := validTranscript(t, checker, schema, readFile(t, p), p)
		base := filepath.Base(p)
		switch {
		case strings.HasPrefix(base, "schema-") && len(s) == 1:
		case strings.HasPrefix(base, "rule-") && len(s) == 0 && len(r) == 1:
		default:
			t.Fatalf("%s: want exactly one defect of its kind, got schema errors %v, rule errors %v", p, s, r)
		}
	}

	bin := harness.Bin(t)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if got := check(t, bin, readFile(t, p), p); got != files[p] {
			t.Errorf("%s: assemble --check says valid=%v, want %v", p, got, files[p])
		}
	}
}

// §4.3: F-01 and its variants through normalize and assemble give the golden
// transcript: offsets and recorded_offset_ms (F-01), _N grouping (F-01-rejoin),
// empty tracks and gaps (F-01-gaps), clipping and a period without a stop mark
// (F-01-clip). Both tools are byte-identical on a re-run.
func testAssemble(t *testing.T) {
	bin := harness.Bin(t)
	checker, schema := loadSchema(t)
	for _, name := range caseNames {
		t.Run(name, func(t *testing.T) {
			c := loadCase(t, name)
			dir := inputDir(t, name, c)
			normalized := runTool(t, bin, nil, "normalize", dir)
			if again := runTool(t, bin, nil, "normalize", dir); !bytes.Equal(normalized, again) {
				t.Errorf("normalize is not byte-identical on a re-run")
			}
			if _, ok := decode(t, normalized, "normalize stdout").(map[string]any); !ok {
				t.Fatalf("normalize stdout is not a JSON object: %.200s", normalized)
			}
			in := assembleInput(t, c, normalized)
			out := runTool(t, bin, in, "assemble")
			if again := runTool(t, bin, in, "assemble"); !bytes.Equal(out, again) {
				t.Errorf("assemble is not byte-identical on a re-run")
			}
			if s, r := validTranscript(t, checker, schema, out, "assemble stdout"); len(s)+len(r) > 0 {
				t.Fatalf("assemble output is not a valid transcript: %v %v", s, r)
			}
			want := decode(t, readFile(t, filepath.Join(transcriptsDir, name, "transcript.json")), "golden").(map[string]any)
			compareGolden(t, decode(t, out, "assemble stdout").(map[string]any), want, name)
		})
	}
}

// §6: the normalize and assemble runs of the previous test write no segment
// text or display name to their log (stderr) at INFO or above. The log level
// is set to info; every case runs once more for the scan.
func testLogs(t *testing.T) {
	bin := harness.Bin(t)
	var canaries []string
	for _, name := range caseNames {
		var tr struct {
			Speakers []struct {
				DisplayName string `json:"display_name"`
			} `json:"speakers"`
			Segments []struct {
				Text string `json:"text"`
			} `json:"segments"`
		}
		if err := json.Unmarshal(readFile(t, filepath.Join(transcriptsDir, name, "transcript.json")), &tr); err != nil {
			t.Fatal(err)
		}
		for _, s := range tr.Speakers {
			canaries = append(canaries, s.DisplayName)
		}
		for _, s := range tr.Segments {
			canaries = append(canaries, s.Text)
		}
	}
	env := []string{"RECAP_LOG_LEVEL=info"}
	var logs bytes.Buffer
	for _, name := range caseNames {
		c := loadCase(t, name)
		dir := inputDir(t, name, c)
		normalized, stderr, code := execTool(t, bin, env, nil, "normalize", dir)
		logs.Write(stderr)
		if code != 0 {
			t.Fatalf("%s: normalize exit %d", name, code)
		}
		_, stderr, code = execTool(t, bin, env, assembleInput(t, c, normalized), "assemble")
		logs.Write(stderr)
		if code != 0 {
			t.Fatalf("%s: assemble exit %d", name, code)
		}
	}
	low := strings.ToLower(logs.String())
	for _, c := range canaries {
		if c != "" && strings.Contains(low, strings.ToLower(c)) {
			t.Errorf("tool log contains fixture content %q", c)
		}
	}
}
