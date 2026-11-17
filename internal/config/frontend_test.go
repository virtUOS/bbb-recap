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

// LogFrontendAccess prints the effective mapping per front end and warns on
// front-end IDs without a credential file (metadata-interface.md §2.1).
func TestLogFrontendAccess(t *testing.T) {
	tests := []struct {
		name      string
		env       []string
		frontends []string
		want      []string // each must occur once
		warns     int
	}{
		{"unset: all", with(validEnv(), "RECAP_FRONTEND_SERVERS=-"), []string{"pilos-fixture"},
			[]string{`"msg":"front end access","frontend":"pilos-fixture","bbb_servers":"all","meeting_types":"all","sinks":"all"`},
			0},
		{"unset: mistyped IDs", with(validEnv(), "RECAP_FRONTEND_SERVERS=-",
			"RECAP_FRONTEND_TYPES=pilos-fixture:meeting-summary,pilos-fixtrue:meeting-summary",
			"RECAP_FRONTEND_SINKS=pilos-fixtrue:docs-main"), []string{"pilos-fixture"},
			[]string{
				`"bbb_servers":"all","meeting_types":"meeting-summary","sinks":"all"`,
				`"variable":"RECAP_FRONTEND_TYPES","frontend":"pilos-fixtrue","want_file":"frontend.pilos-fixtrue"`,
				`"variable":"RECAP_FRONTEND_SINKS","frontend":"pilos-fixtrue","want_file":"frontend.pilos-fixtrue"`,
			}, 2},
		{"set", validEnv(), []string{"other-fixture", "pilos-fixture"},
			[]string{
				`"frontend":"pilos-fixture","bbb_servers":"bbb-fixture-a,bbb-fixture-b"`,
				`"msg":"front end is not in RECAP_FRONTEND_SERVERS and may claim no BBB server","frontend":"other-fixture"`,
				`"frontend":"other-fixture","bbb_servers":"none"`,
			}, 1},
		{"set: no credential", validEnv(), nil,
			[]string{`"variable":"RECAP_FRONTEND_SERVERS","frontend":"pilos-fixture"`}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(tt.env, RoleAPI)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			c.LogFrontendAccess(slog.New(slog.NewJSONHandler(&buf, nil)), tt.frontends)
			out := buf.String()
			for _, w := range tt.want {
				if strings.Count(out, w) != 1 {
					t.Errorf("want one %s:\n%s", w, out)
				}
			}
			if n := strings.Count(out, `"level":"WARN"`); n != tt.warns {
				t.Errorf("%d WARN lines, want %d:\n%s", n, tt.warns, out)
			}
		})
	}
}
