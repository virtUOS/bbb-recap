package format

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// formatsDir is the repository's formats directory (templates.md §1).
const formatsDir = "../../formats"

func TestLoadMeetingSummary(t *testing.T) {
	f, err := Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "meeting-summary" || f.Version != "1.0.0" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(f.SHA256) {
		t.Errorf("format %q %q %q", f.ID, f.Version, f.SHA256)
	}
	for _, call := range []string{"summarize", "extract", "merge", "consolidate"} {
		if p, ok := f.Prompt(call); !ok || !strings.Contains(p, "{language}") {
			t.Errorf("prompt %s missing or without {language}", call)
		}
	}
	// Speaker labels exactly as STRINGS in testdata/render/make_goldens.py.
	if f.Strings("en")["speaker"] != "Speaker {n}" || f.Strings("de")["speaker"] != "Sprecher {n}" {
		t.Errorf("speaker labels %q %q", f.Strings("en")["speaker"], f.Strings("de")["speaker"])
	}
	if f.Strings("fr") != nil {
		t.Errorf("unexpected fr table")
	}
	again, err := Load(formatsDir, "meeting-summary")
	if err != nil || again.SHA256 != f.SHA256 {
		t.Errorf("SHA-256 not deterministic: %v", err)
	}
}

// The schemas accept and reject the LLM reply fixtures under testdata/render/
// as their names say (templates.md §4; the contract test CT-TPL-01 runs
// through recap tool render).
func TestReplySchemas(t *testing.T) {
	f, err := Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary.v1", "chunk-notes.v1"} {
		files, err := filepath.Glob(filepath.Join("../../testdata/render/llm", name, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures for %s: %v", name, err)
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			valid := strings.HasPrefix(filepath.Base(file), "valid.")
			if err := f.Validate(name, b); (err == nil) != valid {
				t.Errorf("%s: valid = %v, want %v (%v)", file, err == nil, valid, err)
			}
		}
	}
	for _, golden := range []string{"F-01", "gap"} {
		b, err := os.ReadFile(filepath.Join("../../testdata/render", golden, "summary.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Validate("summary.v1", b); err != nil {
			t.Errorf("%s/summary.json: %v", golden, err)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	for _, id := range []string{"no-such-format", "../formats", "Meeting"} {
		if _, err := Load(formatsDir, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Load(%q) = %v, want ErrNotFound", id, err)
		}
	}
	base := fstest.MapFS{
		"format.json":          {Data: []byte(`{"id": "x", "version": "1.0.0"}`)},
		"a.schema.json":        {Data: []byte(`{"type": "object"}`)},
		"prompts/summarize.md": {Data: []byte("p")},
		"strings/en.json":      {Data: []byte(`{"speaker": "S {n}"}`)},
	}
	if _, err := load(base, "x"); err != nil {
		t.Fatalf("base: %v", err)
	}
	for name, bad := range map[string]fstest.MapFS{
		"wrong id":      {"format.json": {Data: []byte(`{"id": "y", "version": "1.0.0"}`)}},
		"no version":    {"format.json": {Data: []byte(`{"id": "x"}`)}},
		"no metadata":   {},
		"bad schema":    {"format.json": base["format.json"], "a.schema.json": {Data: []byte(`{"type": 1}`)}},
		"bad strings":   {"format.json": base["format.json"], "strings/en.json": {Data: []byte(`{"a": 1}`)}},
		"bad lang name": {"format.json": base["format.json"], "strings/eng.json": {Data: []byte(`{}`)}},
	} {
		if _, err := load(bad, "x"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want a load error", name, err)
		}
	}
	// Any file changes the SHA-256.
	a, _ := load(base, "x")
	base["prompts/summarize.md"] = &fstest.MapFile{Data: []byte("q")}
	b, _ := load(base, "x")
	if a.SHA256 == b.SHA256 {
		t.Errorf("SHA-256 did not change with a prompt")
	}
}
