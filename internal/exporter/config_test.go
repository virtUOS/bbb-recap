package exporter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnv writes an env file and returns the environment that names it.
func writeEnv(t *testing.T, content string, extra ...string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "exporter.env")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return append([]string{keyEnvFile + "=" + p}, extra...)
}

func configErrors(t *testing.T, err error) string {
	t.Helper()
	var ce ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want ConfigError, got %v", err)
	}
	return ce.Error()
}

const minimalEnv = "RECAP_EXPORTER_SERVICE_URL=https://ingest.recap.test/\nRECAP_EXPORTER_BBB_SERVER_ID=bbb-fixture-a\n"

func TestConfigDefaultsAndGrammar(t *testing.T) {
	env := writeEnv(t, "# comment\n\n   "+minimalEnv+"  RECAP_EXPORTER_LOG_LEVEL=debug  \nRECAP_EXPORTER_META_PREFIX=\n")
	c, err := LoadConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServiceURL != "https://ingest.recap.test" {
		t.Errorf("ServiceURL %q, want trailing slash removed", c.ServiceURL)
	}
	if c.MetaPrefix != "recap" {
		t.Errorf("empty value must mean the default, got %q", c.MetaPrefix)
	}
	if c.RawDir != "/var/bigbluebutton/recording/raw" || c.StateDir != "/var/lib/recap-exporter/state" ||
		c.SpoolDir != "/var/lib/recap-exporter/spool" || c.DeletePolicy != PolicyNever || c.FailureDays != 0 ||
		c.ListenAddr != "127.0.0.1:9466" || c.TokenFile != "/etc/recap-exporter/secrets/ingest-token" {
		t.Errorf("defaults: %+v", c)
	}
	if c.LogLevel.String() != "DEBUG" {
		t.Errorf("log level %v", c.LogLevel)
	}
}

func TestConfigEnvironmentOverridesFile(t *testing.T) {
	env := writeEnv(t, minimalEnv+"RECAP_EXPORTER_META_PREFIX=fromfile\n", "RECAP_EXPORTER_META_PREFIX=fromenv")
	c, err := LoadConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.MetaPrefix != "fromenv" {
		t.Errorf("MetaPrefix %q, want fromenv", c.MetaPrefix)
	}
}

func TestConfigBadLines(t *testing.T) {
	for _, line := range []string{
		"RECAP_EXPORTER_LOG_LEVEL = info",
		"RECAP_EXPORTER_LOG_LEVEL= info",
		"RECAP_EXPORTER_LOG_LEVEL =info",
		"export RECAP_EXPORTER_LOG_LEVEL=info",
		"just words",
	} {
		_, err := LoadConfig(writeEnv(t, minimalEnv+line+"\nRECAP_EXPORTER_NOPE=1\n"))
		msg := configErrors(t, err)
		if !strings.Contains(msg, "line 3") || !strings.Contains(msg, "RECAP_EXPORTER_NOPE") {
			t.Errorf("%q: want both the bad line and the unknown key listed, got\n%s", line, msg)
		}
	}
}

func TestConfigQuotesAreLiteral(t *testing.T) {
	_, err := LoadConfig(writeEnv(t, minimalEnv+`RECAP_EXPORTER_DELETE_POLICY="never"`+"\n"))
	if msg := configErrors(t, err); !strings.Contains(msg, keyDeletePolicy) {
		t.Errorf("quoted value must be taken literally and rejected:\n%s", msg)
	}
}

func TestConfigMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.env")
	// Required keys from the environment: fine.
	if _, err := LoadConfig([]string{keyEnvFile + "=" + missing,
		keyServiceURL + "=https://ingest.recap.test", keyServerID + "=bbb-fixture-a"}); err != nil {
		t.Errorf("missing file with required keys in the environment: %v", err)
	}
	_, err := LoadConfig([]string{keyEnvFile + "=" + missing})
	msg := configErrors(t, err)
	for _, want := range []string{keyServiceURL, keyServerID, "not found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	cases := []struct {
		name, env string
		want      []string
	}{
		{"http URL", "RECAP_EXPORTER_SERVICE_URL=http://x\n", []string{keyServiceURL, keyServerID}},
		{"URL with query", "RECAP_EXPORTER_SERVICE_URL=https://x/?a=b\nRECAP_EXPORTER_BBB_SERVER_ID=a\n", []string{keyServiceURL}},
		{"bad server id", "RECAP_EXPORTER_SERVICE_URL=https://x\nRECAP_EXPORTER_BBB_SERVER_ID=BBB_1\n", []string{keyServerID}},
		{"bad prefix", minimalEnv + "RECAP_EXPORTER_META_PREFIX=Recap-1\n", []string{keyMetaPrefix}},
		{"bad policy", minimalEnv + "RECAP_EXPORTER_DELETE_POLICY=always\n", []string{keyDeletePolicy}},
		{"after_ack without markers", minimalEnv + "RECAP_EXPORTER_DELETE_POLICY=after_ack\n", []string{keyStatusDir, keyFormats}},
		{"failure expiry without markers", minimalEnv + "RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS=3\n", []string{keyStatusDir, keyFormats}},
		{"negative days", minimalEnv + "RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS=-1\n", []string{keyFailureDays}},
		{"relative dir", minimalEnv + "RECAP_EXPORTER_STATE_DIR=state\n", []string{keyStateDir}},
		{"bad format", minimalEnv + "RECAP_EXPORTER_RECORDING_FORMATS=presentation,../x\n", []string{keyFormats}},
		{"bad listen addr", minimalEnv + "RECAP_EXPORTER_LISTEN_ADDR=9466\n", []string{keyListenAddr}},
		{"bad log level", minimalEnv + "RECAP_EXPORTER_LOG_LEVEL=loud\n", []string{keyLogLevel}},
		{"env file key in file", minimalEnv + "RECAP_EXPORTER_ENV_FILE=/x\n", []string{keyEnvFile}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeEnv(t, c.env))
			msg := configErrors(t, err)
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("missing %s in\n%s", w, msg)
				}
			}
		})
	}
}

func TestConfigAfterAck(t *testing.T) {
	c, err := LoadConfig(writeEnv(t, minimalEnv+"RECAP_EXPORTER_DELETE_POLICY=after_ack\n"+
		"RECAP_EXPORTER_RECORDING_STATUS_DIR=/var/bigbluebutton/recording/status\n"+
		"RECAP_EXPORTER_RECORDING_FORMATS=presentation, video\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Formats) != 2 || c.Formats[0] != "presentation" || c.Formats[1] != "video" {
		t.Errorf("formats %v", c.Formats)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	c := &Config{TokenFile: filepath.Join(dir, "token")}
	if err := os.WriteFile(c.TokenFile, []byte("FIXTURE-NOT-A-SECRET-x\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	tok, err := c.ReadToken()
	if err != nil || tok != "FIXTURE-NOT-A-SECRET-x" {
		t.Errorf("token %q, %v", tok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two"), []byte("a\nb\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	c.TokenFile = filepath.Join(dir, "two")
	if _, err := c.ReadToken(); err == nil || strings.Contains(err.Error(), "a\nb") {
		t.Errorf("two lines: %v", err)
	}
	c.TokenFile = filepath.Join(dir, "missing")
	if _, err := c.ReadToken(); err == nil {
		t.Error("missing token file accepted")
	}
}
