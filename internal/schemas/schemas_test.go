package schemas

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// specDir holds the authoritative schemas (go test runs in internal/schemas).
const specDir = "../../docs/reference/schemas"

// drift compares the embedded files with the *.json files in dir and returns
// an error naming every difference.
func drift(embedded fs.FS, dir string) error {
	want, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	got, err := fs.Glob(embedded, "*.json")
	if err != nil {
		return err
	}
	var names []string
	for _, p := range want {
		names = append(names, filepath.Base(p))
	}
	if !slices.Equal(names, got) {
		return fmt.Errorf("file sets differ: %s has %v, embedded %v", dir, names, got)
	}
	for _, n := range names {
		a, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		b, err := fs.ReadFile(embedded, n)
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("%s differs from %s; copy it again", n, filepath.Join(dir, n))
		}
	}
	return nil
}

func TestNoDrift(t *testing.T) {
	if err := drift(FS(), specDir); err != nil {
		t.Fatal(err)
	}
}

// The drift check notices one flipped byte.
func TestDriftDetectsFlippedByte(t *testing.T) {
	dir := t.TempDir()
	names, _ := fs.Glob(FS(), "*.json")
	for _, n := range names {
		b, _ := fs.ReadFile(FS(), n)
		if n == "manifest.schema.json" {
			b[len(b)/2] ^= 0x01
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := drift(FS(), dir)
	if err == nil {
		t.Fatal("drift check passed on a copy with a flipped byte")
	}
	t.Logf("drift check on a flipped byte: %v", err)
}

func TestAllSchemasCompile(t *testing.T) {
	all, err := compileAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{Manifest, Registration, Transcript} {
		if all[n] == nil {
			t.Errorf("schema %s not compiled", n)
		}
	}
}

func TestValidateManifest(t *testing.T) {
	golden, err := os.ReadFile("../../testdata/meetings/F-01/expected/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(Manifest, golden); err != nil {
		t.Fatalf("F-01 golden manifest: %v", err)
	}
	bad, err := os.ReadFile("../../testdata/manifests/invalid/M-03.V-I02-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(Manifest, bad); err == nil {
		t.Fatal("M-03 (path ../events.xml) is valid, want invalid")
	}
	if err := Validate("nope", golden); err == nil {
		t.Fatal("unknown schema name: no error")
	}
	if err := Validate(Manifest, []byte("{")); err == nil {
		t.Fatal("broken JSON: no error")
	}
}

// \u escapes from the schemas work under Go's regexp engine.
func TestECMAPatterns(t *testing.T) {
	cases := []struct {
		pattern, s string
		match      bool
	}{
		{`^[^\u0000-\u001f]*$`, "ok text", true},
		{`^[^\u0000-\u001f]*$`, "tab\there", false},
		{`^[^/\\\u0000-\u001f\u007f]+$`, `a\b`, false},
		{`^[^/\\\u0000-\u001f\u007f]+$`, "a\x7f", false},
		{`^[^/\\\u0000-\u001f\u007f]+$`, "plain", true},
		{`^\\u0041$`, "\\u0041", true}, // escaped backslash, then literal u0041
	}
	for _, c := range cases {
		re, err := ecmaToGo(c.pattern)
		if err != nil {
			t.Fatalf("%q: %v", c.pattern, err)
		}
		if got := re.MatchString(c.s); got != c.match {
			t.Errorf("%q on %q = %v, want %v", c.pattern, c.s, got, c.match)
		}
	}
}
