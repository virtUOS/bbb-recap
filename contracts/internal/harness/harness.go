// Package harness holds helpers shared by the black-box contract tests.
//
// Contract tests find what they test through environment variables
// (contracts/README.md). When a variable is unset, the
// test skips and names the variable, so `go test ./contracts/...` passes on a
// machine without binaries or a running service.
package harness

import (
	"errors"
	"os"
	"path/filepath"
)

// Environment variables read by the contract tests.
const (
	EnvBin         = "RECAP_BIN"               // path to the recap binary
	EnvExporterBin = "RECAP_EXPORTER_BIN"      // path to the recap-exporter binary
	EnvBaseURL     = "RECAP_CONTRACT_BASE_URL" // base URL of a running recap service
)

// TB is the subset of testing.TB the helpers use. *testing.T satisfies it.
type TB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Bin returns the absolute path of the recap binary from RECAP_BIN, or skips.
// A relative path is resolved against the module root (the directory holding
// go.mod), so `RECAP_BIN=./bin/recap make contracts` works from the repo root.
func Bin(t TB) string {
	t.Helper()
	return binFromEnv(t, EnvBin)
}

// ExporterBin returns the absolute path of the recap-exporter binary from
// RECAP_EXPORTER_BIN, or skips.
func ExporterBin(t TB) string {
	t.Helper()
	return binFromEnv(t, EnvExporterBin)
}

// BaseURL returns RECAP_CONTRACT_BASE_URL, or skips.
func BaseURL(t TB) string {
	t.Helper()
	v := os.Getenv(EnvBaseURL)
	if v == "" {
		t.Skipf("skipped: %s not set", EnvBaseURL)
	}
	return v
}

// binFromEnv skips when the variable is unset and fails when it is set but
// does not point at an executable file.
func binFromEnv(t TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("skipped: %s not set", name)
		return ""
	}
	p := v
	if !filepath.IsAbs(p) {
		root, err := moduleRoot()
		if err != nil {
			t.Fatalf("%s=%q: %v", name, v, err)
			return ""
		}
		p = filepath.Join(root, p)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, v, err)
		return ""
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s=%q: not an executable file", name, v)
		return ""
	}
	return p
}

// moduleRoot walks up from the working directory to the first directory that
// contains go.mod. `go test` runs each test in its package directory, which
// lies below the module root.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("relative path, but no go.mod found above the working directory")
		}
		dir = parent
	}
}
