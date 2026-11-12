package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// sinksFixture returns testdata/sinks.yaml with key set to value (a key
// that occurs once; value "-" removes the line), written to a temp dir
// next to a copy of the key file.
func sinksFixture(t *testing.T, key, value string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/sinks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if key != "" {
		re := regexp.MustCompile(`(?m)^(\s+(?:- )?` + regexp.QuoteMeta(key) + `:) .*\n`)
		if n := len(re.FindAllString(text, -1)); n != 1 {
			t.Fatalf("key %s found %d times", key, n)
		}
		repl := "${1} " + value + "\n"
		if value == "-" {
			repl = ""
		}
		text = re.ReplaceAllString(text, repl)
	}
	dir := t.TempDir()
	key2, err := os.ReadFile("testdata/docs-s2s.key")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"sinks.yaml": []byte(text), "docs-s2s.key": key2} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "sinks.yaml")
}

func TestLoadSinksValid(t *testing.T) {
	sinks, err := LoadSinks(sinksFixture(t, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	s := sinks[0]
	if s.APIKey != "FIXTURE-NOT-A-SECRET-config-docs-s2s" || s.Timeout != 60*time.Second || s.MaxInflight != 2 ||
		s.MaxDocumentBytes != 2097152 {
		t.Errorf("profile = %+v", s)
	}
}

func TestLoadSinksDefaults(t *testing.T) {
	path := sinksFixture(t, "request_timeout", "-")
	b, _ := os.ReadFile(path)
	text := strings.NewReplacer("    max_inflight: 2\n", "", "    max_document_bytes: 2097152\n", "").Replace(string(b))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sinks, err := LoadSinks(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sinks[0]
	if s.Timeout != DefaultSinkRequestTimeout || s.MaxInflight != DefaultSinkMaxInflight ||
		s.MaxDocumentBytes != DefaultSinkMaxDocumentBytes {
		t.Errorf("defaults not applied: %+v", s)
	}
}

// check-config names each bad sink key (sinks.md §1.5, §3.1).
func TestLoadSinksBadKeys(t *testing.T) {
	for _, c := range []struct{ key, value, want string }{
		{"docs_base_url", "docs.recap.test", "sinks[0].docs_base_url"},
		{"docs_base_url", "ftp://docs.recap.test", "sinks[0].docs_base_url"},
		{"docs_base_url", "https://user:pw@docs.recap.test", "sinks[0].docs_base_url"},
		{"api_key_file", "nope.key", "sinks[0].api_key_file: cannot read file nope.key"},
		{"api_key_file", "-", "sinks[0].api_key_file: required"},
		{"document_url_template", "https://docs.recap.test/docs/", "sinks[0].document_url_template"},
		{"request_timeout", "soon", "sinks[0].request_timeout"},
		{"request_timeout", "0s", "sinks[0].request_timeout"},
		{"max_inflight", "-1", "sinks[0].max_inflight"},
		{"max_inflight", "two", "max_inflight"},
		{"max_document_bytes", "-5", "sinks[0].max_document_bytes"},
		{"type", "wiki", "sinks[0].type"},
		{"operator", "-", "sinks[0].declaration.operator: required"},
		{"location", "mars", "sinks[0].declaration.location"},
		{"deletion", "-", "sinks[0].declaration.deletion: required"},
		{"used_for_training", "maybe", "sinks[0].declaration.used_for_training"},
		{"reviewed_at", "yesterday", "sinks[0].declaration.reviewed_at"},
		{"data_categories", "[]", "sinks[0].declaration.data_categories"},
	} {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			_, err := LoadSinks(sinksFixture(t, c.key, c.value))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), "FIXTURE-NOT-A-SECRET") {
				t.Error("error contains the key")
			}
		})
	}
}

func TestLoadSinksEmptyKey(t *testing.T) {
	path := sinksFixture(t, "", "")
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "docs-s2s.key"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSinks(path); err == nil || !strings.Contains(err.Error(), "api_key_file: file docs-s2s.key is empty") {
		t.Fatalf("got %v", err)
	}
}

// The key file is read for the worker role only; the api does not deliver.
func TestSinkKeyReadByWorkerOnly(t *testing.T) {
	path := sinksFixture(t, "api_key_file", "nope.key")
	env := with(validEnv(), "RECAP_SINKS_FILE="+path)
	if _, err := Load(env, RoleAPI); err != nil {
		t.Fatalf("api role: %v", err)
	}
	if _, err := Load(env, RoleWorker); err == nil || !strings.Contains(err.Error(), "RECAP_SINKS_FILE: sinks[0].api_key_file") {
		t.Fatalf("worker role: got %v", err)
	}
	c, err := Load(validEnv(), RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sinks[0].APIKey == "" {
		t.Error("worker role: key not read")
	}
}

func TestLogSinkWarnings(t *testing.T) {
	sinks, err := LoadSinks(sinksFixture(t, "location", "third_country:us"))
	if err != nil {
		t.Fatal(err)
	}
	sinks[0].Declaration.UsedForTraining = "yes"
	var buf bytes.Buffer
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), sinks)
	out := buf.String()
	if strings.Count(out, `"level":"WARN"`) != 2 || !strings.Contains(out, "third-country") ||
		!strings.Contains(out, "training") {
		t.Errorf("warnings: %s", out)
	}
	buf.Reset()
	ok, _ := LoadSinks(sinksFixture(t, "", ""))
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), ok)
	if buf.Len() != 0 {
		t.Errorf("unexpected warnings: %s", buf.String())
	}
	// http stays allowed, with a WARN (sinks.md §3.4).
	buf.Reset()
	plain, err := LoadSinks(sinksFixture(t, "docs_base_url", "http://mock:8080"))
	if err != nil {
		t.Fatal(err)
	}
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), plain)
	if strings.Count(buf.String(), `"level":"WARN"`) != 1 || !strings.Contains(buf.String(), "https") {
		t.Errorf("http warning: %s", buf.String())
	}
}

// An invalid request_timeout leaves Timeout at its default.
func TestBadTimeoutNotAssigned(t *testing.T) {
	l := &loader{env: map[string]string{"RECAP_SINKS_FILE": sinksFixture(t, "request_timeout", "soon")}}
	sinks := loadSinks(l, &Config{files: map[string][]byte{}}, "RECAP_SINKS_FILE", true)
	if len(l.errs) == 0 || sinks[0].Timeout != DefaultSinkRequestTimeout {
		t.Errorf("errors %v, timeout %v", l.errs, sinks[0].Timeout)
	}
}
