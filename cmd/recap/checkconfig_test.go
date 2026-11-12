package main

import (
	"bytes"
	"strings"
	"testing"
)

const cfgData = "../../internal/config/testdata/"

func validEnv() []string {
	return []string{
		"RECAP_DB_DSN=postgres://recap@127.0.0.1:5432/recap",
		"RECAP_DB_PASSWORD_FILE=" + cfgData + "db-password",
		"RECAP_S3_ENDPOINT=https://s3.recap.test",
		"RECAP_S3_BUCKET=recap",
		"RECAP_S3_ACCESS_KEY_ID_FILE=" + cfgData + "s3-access-key",
		"RECAP_S3_SECRET_ACCESS_KEY_FILE=" + cfgData + "s3-secret-key",
		"RECAP_PROVIDERS_FILE=" + cfgData + "providers.yaml",
		"RECAP_SINKS_FILE=" + cfgData + "sinks.yaml",
		"RECAP_MEETING_TYPES_FILE=" + cfgData + "meeting-types.yaml",
		"RECAP_CLIENT_SECRETS_DIR=" + cfgData + "clients",
		"RECAP_FRONTEND_SERVERS=pilos-fixture:bbb-fixture-a",
		"RECAP_EXPORTER_X=1", "RECAP_CONTRACT_X=1", "RECAP_BIN=/bin/recap",
	}
}

// frontendSinkEnv is validEnv (s3 backend) with a frontend sink profile
// (sinks.md §4a).
func frontendSinkEnv() []string {
	return append(without(validEnv(), "RECAP_SINKS_FILE"), "RECAP_SINKS_FILE="+cfgData+"sinks-frontend.yaml")
}

// without drops the entries for name from env.
func without(env []string, name string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return out
}

func TestCheckConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
		code int
		want []string // on stderr
	}{
		{"valid, both roles", nil, validEnv(), exitOK, nil},
		{"valid, api", []string{"--role", "api"}, validEnv(), exitOK, nil},
		{"unknown variable", nil, append(validEnv(), "RECAP_FOO=1"), exitUsage, []string{"RECAP_FOO"}},
		{"without frontend servers", nil, without(validEnv(), "RECAP_FRONTEND_SERVERS"), exitOK, nil},
		{"bad frontend servers", nil, append(without(validEnv(), "RECAP_FRONTEND_SERVERS"), "RECAP_FRONTEND_SERVERS=pilos"),
			exitUsage, []string{"RECAP_FRONTEND_SERVERS: item \"pilos\""}},
		{"missing DSN", nil, without(validEnv(), "RECAP_DB_DSN"), exitUsage, []string{"RECAP_DB_DSN"}},
		{"DSN with password", nil, append(without(validEnv(), "RECAP_DB_DSN"), "RECAP_DB_DSN=postgres://recap:pw@db/recap"),
			exitUsage, []string{"RECAP_DB_DSN"}},
		{"bad duration", nil, append(validEnv(), "RECAP_REGISTRATION_TTL=fortnight"), exitUsage,
			[]string{"RECAP_REGISTRATION_TTL"}},
		{"unknown YAML key", nil, append(without(validEnv(), "RECAP_SINKS_FILE"), "RECAP_SINKS_FILE="+cfgData+"sinks-unknown-key.yaml"),
			exitUsage, []string{"RECAP_SINKS_FILE", "retries"}},
		{"bad profile name", nil, append(without(validEnv(), "RECAP_SINKS_FILE"), "RECAP_SINKS_FILE="+cfgData+"sinks-bad-name.yaml"),
			exitUsage, []string{"RECAP_SINKS_FILE", "Docs_Main"}},
		{"several errors", nil, append(without(validEnv(), "RECAP_DB_DSN"), "RECAP_FOO=1", "RECAP_WORKER_SLOTS=x"),
			exitUsage, []string{"RECAP_FOO", "RECAP_DB_DSN", "RECAP_WORKER_SLOTS"}},
		{"bad role", []string{"--role", "exporter"}, validEnv(), exitUsage, []string{"--role"}},
		{"frontend sink", nil, frontendSinkEnv(), exitOK, nil},
		{"frontend sink with RECAP_RESULT_TTL=0", nil, append(frontendSinkEnv(), "RECAP_RESULT_TTL=0"), exitUsage,
			[]string{"RECAP_SINKS_FILE: sinks[1]: type frontend needs RECAP_RESULT_TTL above 0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := checkConfig(tt.args, tt.env, &out, &errOut); code != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, tt.code, errOut.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, errOut.String())
				}
			}
			if strings.Contains(errOut.String(), "FIXTURE-NOT-A-SECRET") {
				t.Errorf("stderr contains a secret:\n%s", errOut.String())
			}
		})
	}
}

// --help after --role prints the same line as plain --help (dispatch).
func TestCheckConfigHelpConsistent(t *testing.T) {
	var plain, plainErr bytes.Buffer
	if code := run([]string{"check-config", "--help"}, &plain, &plainErr); code != exitOK {
		t.Fatalf("check-config --help: exit %d", code)
	}
	for _, args := range [][]string{{"--role", "api", "--help"}, {"-h"}, {"--role", "worker", "-help"}} {
		var out, errOut bytes.Buffer
		if code := checkConfig(args, validEnv(), &out, &errOut); code != exitOK {
			t.Errorf("%v: exit %d", args, code)
		}
		if out.String() != plain.String() || errOut.Len() != 0 {
			t.Errorf("%v: stdout %q stderr %q; want stdout %q, empty stderr", args, out.String(), errOut.String(), plain.String())
		}
	}
}

// privacy.md §7: profiles without a declaration pass check-config with one
// WARN each. Provider profiles are checked by the worker role only.
func TestCheckConfigNoDeclaration(t *testing.T) {
	env := append(without(without(validEnv(), "RECAP_SINKS_FILE"), "RECAP_PROVIDERS_FILE"),
		"RECAP_SINKS_FILE="+cfgData+"sinks-no-declaration.yaml",
		"RECAP_PROVIDERS_FILE="+cfgData+"providers-no-declaration.yaml")
	for _, tt := range []struct {
		args     []string
		profiles []string
	}{
		{nil, []string{"docs-main", "whisperx-lab", "vllm-lab"}},
		{[]string{"--role", "api"}, []string{"docs-main"}},
	} {
		var out, errOut bytes.Buffer
		if code := checkConfig(tt.args, env, &out, &errOut); code != exitOK {
			t.Fatalf("%v: exit code = %d, want %d; stderr:\n%s", tt.args, code, exitOK, errOut.String())
		}
		got := errOut.String()
		if n := strings.Count(got, "has no declaration"); n != len(tt.profiles) {
			t.Errorf("%v: %d declaration warnings, want %d:\n%s", tt.args, n, len(tt.profiles), got)
		}
		for _, p := range tt.profiles {
			if strings.Count(got, `"profile":"`+p+`"`) != 1 {
				t.Errorf("%v: want one warning for %s:\n%s", tt.args, p, got)
			}
		}
	}
}
