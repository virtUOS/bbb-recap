package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// check-config prints the same sink warnings as worker start-up:
// non-https base URL, third-country location, training.
func TestCheckConfigSinkWarnings(t *testing.T) {
	b, err := os.ReadFile(cfgData + "sinks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer(
		"docs_base_url: https://docs.recap.test", "docs_base_url: http://docs.recap.test",
		"location: on_prem", "location: third_country:us",
		`used_for_training: "no"`, `used_for_training: "yes"`,
		"api_key_file: docs-s2s.key", "api_key_file: "+mustAbs(t, cfgData+"docs-s2s.key"),
	).Replace(string(b))
	path := filepath.Join(t.TempDir(), "sinks.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(without(validEnv(), "RECAP_SINKS_FILE"), "RECAP_SINKS_FILE="+path)
	var stdout, stderr bytes.Buffer
	if code := checkConfig([]string{"--role", "worker"}, env, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, want := range []string{"does not use https", "third-country", "used for training"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
