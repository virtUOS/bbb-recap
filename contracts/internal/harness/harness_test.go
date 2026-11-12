package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTB records Skipf and Fatalf instead of stopping the test.
type fakeTB struct {
	skipped, failed bool
	msg             string
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Skipf(format string, args ...any) {
	f.skipped = true
	f.msg = fmt.Sprintf(format, args...)
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
}

func TestBinSkipsWhenUnset(t *testing.T) {
	t.Setenv(EnvBin, "")
	var f fakeTB
	Bin(&f)
	if !f.skipped || f.failed {
		t.Fatalf("skipped=%v failed=%v, want a skip", f.skipped, f.failed)
	}
	if !strings.Contains(f.msg, EnvBin) {
		t.Fatalf("skip message %q does not name %s", f.msg, EnvBin)
	}
}

func TestExporterBinAndBaseURLSkipWhenUnset(t *testing.T) {
	t.Setenv(EnvExporterBin, "")
	t.Setenv(EnvBaseURL, "")
	for name, call := range map[string]func(TB){
		EnvExporterBin: func(tb TB) { ExporterBin(tb) },
		EnvBaseURL:     func(tb TB) { BaseURL(tb) },
	} {
		var f fakeTB
		call(&f)
		if !f.skipped || !strings.Contains(f.msg, name) {
			t.Errorf("%s: skipped=%v msg=%q, want a skip naming the variable", name, f.skipped, f.msg)
		}
	}
}

func TestBinFailsOnNonExecutable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recap")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBin, p)
	var f fakeTB
	Bin(&f)
	if !f.failed {
		t.Fatalf("want a failure for a non-executable file, got skipped=%v msg=%q", f.skipped, f.msg)
	}
}

func TestBinReturnsAbsolutePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recap")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBin, p)
	var f fakeTB
	if got := Bin(&f); got != p || f.skipped || f.failed {
		t.Fatalf("Bin = %q (skipped=%v failed=%v msg=%q), want %q", got, f.skipped, f.failed, f.msg, p)
	}
}

// TestBinResolvesRelativeToModuleRoot builds a fake module with a binary at
// bin/recap, runs from a package directory two levels down (as `go test`
// does) and sets RECAP_BIN=./bin/recap, as a user would from the repo root.
func TestBinResolvesRelativeToModuleRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "contracts", "x")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "bin", "recap")
	if err := os.WriteFile(want, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(pkg)
	t.Setenv(EnvBin, "./bin/recap")
	var f fakeTB
	if got := Bin(&f); got != want || f.skipped || f.failed {
		t.Fatalf("Bin = %q (skipped=%v failed=%v msg=%q), want %q", got, f.skipped, f.failed, f.msg, want)
	}
}

// TestLocators calls the helpers with the real *testing.T and the real
// environment. Under `make contracts` without the variables set, each subtest
// shows up as SKIP with the missing variable name.
func TestLocators(t *testing.T) {
	t.Run(EnvBin, func(t *testing.T) { t.Logf("recap binary: %s", Bin(t)) })
	t.Run(EnvExporterBin, func(t *testing.T) { t.Logf("exporter binary: %s", ExporterBin(t)) })
	t.Run(EnvBaseURL, func(t *testing.T) { t.Logf("base URL: %s", BaseURL(t)) })
}
