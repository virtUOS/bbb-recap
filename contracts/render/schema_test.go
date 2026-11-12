package render

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// checkSchema runs `recap tool render --check-schema meeting-summary/<schema>`
// with the reply bytes on stdin and returns the verdict.
func checkSchema(t *testing.T, bin, schema string, reply []byte) bool {
	t.Helper()
	args := []string{"tool", "render", "--check-schema", "meeting-summary/" + schema}
	stdout, stderr, code := execBin(t, bin, nil, reply, args...)
	if code != 0 {
		t.Fatalf("recap %s: exit %d (want 0 for every verdict)\nstderr: %s", strings.Join(args, " "), code, stderr)
	}
	var v struct {
		Valid *bool `json:"valid"`
	}
	if err := strictUnmarshal(stdout, &v); err != nil || v.Valid == nil {
		t.Fatalf("recap %s: stdout is not {\"valid\": bool}: %v\nstdout: %q", strings.Join(args, " "), err, stdout)
	}
	return *v.Valid
}

// LLM reply fixtures validate against summary.v1 and chunk-notes.v1
// (templates.md §4) exactly as their file names say:
// testdata/render/llm/<schema>/{valid,invalid}.<case>.json.
func TestReplySchemas(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-01", func(t *testing.T) {
		for _, schema := range []string{"summary.v1", "chunk-notes.v1"} {
			files, err := filepath.Glob(filepath.Join(renderDir, "llm", schema, "*.json"))
			if err != nil || len(files) < 2 {
				t.Fatalf("fixture: no reply files for %s (%v)", schema, err)
			}
			for _, f := range files {
				name := filepath.Base(f)
				want := strings.HasPrefix(name, "valid.")
				if !want && !strings.HasPrefix(name, "invalid.") {
					t.Fatalf("fixture: %s is neither valid.* nor invalid.*", f)
				}
				t.Run(schema+"/"+name, func(t *testing.T) {
					if got := checkSchema(t, bin, schema, readFile(t, f)); got != want {
						t.Errorf("valid = %v, want %v", got, want)
					}
				})
			}
		}
		// The mock LLM replies of the render goldens are valid summary.v1.
		for _, c := range []string{"F-01", "gap"} {
			t.Run("summary.v1/"+c+"/summary.json", func(t *testing.T) {
				if !checkSchema(t, bin, "summary.v1", golden(t, c, "summary.json")) {
					t.Errorf("testdata/render/%s/summary.json: valid = false, want true", c)
				}
			})
		}
	})
}
