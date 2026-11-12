package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// frontendEnv is validEnv (s3 backend) with the frontend fixture.
func frontendEnv(kv ...string) []string {
	return with(validEnv(), append([]string{"RECAP_SINKS_FILE=testdata/sinks-frontend.yaml"}, kv...)...)
}

// A frontend profile has no URL, key or timeout (sinks.md §4a).
func TestLoadSinksFrontend(t *testing.T) {
	sinks, err := LoadSinks("testdata/sinks-frontend.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := sinks[1]
	if f.Name != "frontend-main" || f.Type != SinkTypeFrontend || f.APIKey != "" || f.MaxDocumentBytes != 2097152 ||
		f.MaxInflight != DefaultSinkMaxInflight {
		t.Errorf("profile = %+v", f)
	}
	b, err := os.ReadFile("testdata/sinks-frontend.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"    docs_base_url: https://docs.recap.test\n",
		"    api_key_file: docs-s2s.key\n",
		"    document_url_template: \"https://docs.recap.test/docs/{id}/\"\n",
		"    request_timeout: 60s\n",
	} {
		key, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		t.Run(key, func(t *testing.T) {
			text := strings.Replace(string(b), "    type: frontend\n", "    type: frontend\n"+line, 1)
			path := filepath.Join(t.TempDir(), "sinks.yaml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			want := "sinks[1]." + key + ": not allowed for type frontend"
			if _, err := LoadSinks(path); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

func TestResultTTL(t *testing.T) {
	c, err := Load(validEnv(), bothRoles...)
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultTTL != 7*24*time.Hour {
		t.Errorf("default RECAP_RESULT_TTL = %s, want 7d", c.ResultTTL)
	}
	// 0 is allowed without a frontend profile: the row goes at cleanup.
	if c, err := Load(with(validEnv(), "RECAP_RESULT_TTL=0"), bothRoles...); err != nil || c.ResultTTL != 0 {
		t.Fatalf("RECAP_RESULT_TTL=0 without a frontend profile: %v", err)
	}
	if c, err := Load(frontendEnv("RECAP_RESULT_TTL=90m"), bothRoles...); err != nil || c.ResultTTL != 90*time.Minute {
		t.Fatalf("frontend profile with 90m: %v", err)
	}
	if _, err := Load(with(validEnv(), "RECAP_RESULT_TTL=-1h"), bothRoles...); err == nil ||
		!strings.Contains(err.Error(), "RECAP_RESULT_TTL") {
		t.Fatalf("negative TTL: %v", err)
	}
}

// check-config fails a frontend profile with RECAP_RESULT_TTL 0 (sinks.md
// §4a). Both blob backends keep results.
func TestFrontendProfileNeeds(t *testing.T) {
	const want = "sinks[1]: type frontend needs RECAP_RESULT_TTL above 0"
	for _, role := range bothRoles {
		if _, err := Load(frontendEnv("RECAP_RESULT_TTL=0"), role); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("role %s: got %v, want %q", role, err, want)
		}
	}
	for _, backend := range []string{"RECAP_BLOB_BACKEND=s3", "RECAP_BLOB_BACKEND=fs"} {
		if _, err := Load(frontendEnv(backend, "RECAP_FS_ROOT=testdata"), bothRoles...); err != nil {
			t.Errorf("%s: %v", backend, err)
		}
	}
}

// The frontend profile has no URL, so no https warning.
func TestLogSinkWarningsFrontend(t *testing.T) {
	sinks, err := LoadSinks("testdata/sinks-frontend.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), sinks)
	if buf.Len() != 0 {
		t.Errorf("warnings: %s", buf.String())
	}
}

// privacy.md §7: a docs and a frontend profile without a declaration load,
// and each logs one WARN naming the profile and its type.
func TestLogSinkWarningsNoDeclaration(t *testing.T) {
	sinks, err := LoadSinks("testdata/sinks-frontend-no-declaration.yaml")
	if err != nil {
		t.Fatalf("LoadSinks: %v", err)
	}
	var buf bytes.Buffer
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), sinks)
	out := buf.String()
	if n := strings.Count(out, `"level":"WARN"`); n != 2 {
		t.Errorf("%d WARN lines, want 2:\n%s", n, out)
	}
	for _, want := range []string{`"profile":"docs-main","kind":"docs"`, `"profile":"frontend-main","kind":"frontend"`} {
		if strings.Count(out, want) != 1 {
			t.Errorf("want one warning with %s:\n%s", want, out)
		}
	}
	if strings.Count(out, "has no declaration") != 2 || strings.Contains(out, "FIXTURE-NOT-A-SECRET") {
		t.Errorf("unexpected warning content:\n%s", out)
	}
}
