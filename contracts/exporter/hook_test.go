package exporter

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestHook runs the post_archive hook contract tests, which are Ruby
// (hook/post_archive/test/*_test.rb, docs/reference/bbb-exporter.md §3), with the
// ruby on PATH. Without ruby it skips; `make hook-test` runs the same files in
// podman. The Ruby tests skip on their own until the hook exists.
func TestHook(t *testing.T) {
	t.Run("CT-EXP-01", func(t *testing.T) {
		ruby, err := exec.LookPath("ruby")
		if err != nil {
			t.Skip("skipped: ruby not on PATH (run `make hook-test`, which falls back to podman)")
		}
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		files, err := filepath.Glob(filepath.Join(root, "hook", "post_archive", "test", "*_test.rb"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no hook tests under hook/post_archive/test: %v", err)
		}
		args := append([]string{"-e", "ARGV.each { |f| load f }"}, files...)
		cmd := exec.Command(ruby, args...)
		cmd.Dir = root
		// A clean environment: no RECAP_EXPORTER_* of the caller reaches the tests.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("hook tests failed: %v", err)
		}
	})
}
