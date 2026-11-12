package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// validEnv is a complete configuration for both roles, using the files under testdata/.
func validEnv() []string {
	return []string{
		"PATH=/usr/bin",
		"RECAP_DB_DSN=postgres://recap@127.0.0.1:5432/recap?sslmode=disable",
		"RECAP_DB_PASSWORD_FILE=testdata/db-password",
		"RECAP_S3_ENDPOINT=https://s3.recap.test",
		"RECAP_S3_BUCKET=recap",
		"RECAP_S3_ACCESS_KEY_ID_FILE=testdata/s3-access-key",
		"RECAP_S3_SECRET_ACCESS_KEY_FILE=testdata/s3-secret-key",
		"RECAP_PROVIDERS_FILE=testdata/providers.yaml",
		"RECAP_SINKS_FILE=testdata/sinks.yaml",
		"RECAP_MEETING_TYPES_FILE=testdata/meeting-types.yaml",
		"RECAP_CLIENT_SECRETS_DIR=testdata/clients",
		"RECAP_FRONTEND_SERVERS=pilos-fixture:bbb-fixture-a, pilos-fixture:bbb-fixture-b",
	}
}

// with returns env with the given NAME=value entries replacing or adding entries.
// An entry "NAME=" with the marker value "-" removes NAME.
func with(env []string, kv ...string) []string {
	out := append([]string(nil), env...)
	for _, e := range kv {
		name, value, _ := strings.Cut(e, "=")
		filtered := out[:0]
		for _, o := range out {
			if !strings.HasPrefix(o, name+"=") {
				filtered = append(filtered, o)
			}
		}
		out = filtered
		if value != "-" {
			out = append(out, e)
		}
	}
	return out
}

var bothRoles = []Role{RoleAPI, RoleWorker}

func TestLoadValid(t *testing.T) {
	env := with(validEnv(), "RECAP_EXPORTER_X=1", "RECAP_CONTRACT_X=1", "RECAP_BIN=/bin/recap")
	c, err := Load(env, bothRoles...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DBPassword != "FIXTURE-NOT-A-SECRET-db" {
		t.Errorf("DBPassword = %q: want the file content without the trailing newline", c.DBPassword)
	}
	if c.RegistrationTTL != 14*24*time.Hour || c.IngestStagingTTL != 72*time.Hour ||
		c.JobMetadataRetention != 180*24*time.Hour || c.RetentionAfterDelivery != 0 {
		t.Errorf("duration defaults wrong: %+v", c)
	}
	if c.WorkerSlots != 4 || c.MetaPrefix != "recap" || c.DefaultLanguage != "en" || c.DisplayTZ.String() != "UTC" ||
		c.LogLevel != slog.LevelInfo || c.BlobBackend != "s3" || c.S3Region != "us-east-1" || c.OIDCRoleClaim != "roles" {
		t.Errorf("defaults wrong: %+v", c)
	}
	want := []FrontendServer{{"pilos-fixture", "bbb-fixture-a"}, {"pilos-fixture", "bbb-fixture-b"}}
	if len(c.FrontendServers) != 2 || c.FrontendServers[0] != want[0] || c.FrontendServers[1] != want[1] {
		t.Errorf("FrontendServers = %v, want %v", c.FrontendServers, want)
	}
	if len(c.MeetingTypes) != 1 || c.MeetingTypes[0] != (MeetingType{"meeting-summary", "meeting-summary"}) {
		t.Errorf("MeetingTypes = %v", c.MeetingTypes)
	}
	if len(c.Sinks) != 1 || c.Sinks[0].Name != "docs-main" || c.Sinks[0].Declaration == nil {
		t.Errorf("Sinks = %+v", c.Sinks)
	}
}

func TestLoadFrontendAllowLists(t *testing.T) {
	c, err := Load(validEnv(), bothRoles...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FrontendTypes != nil || c.FrontendSinks != nil {
		t.Errorf("unset lists: FrontendTypes = %v, FrontendSinks = %v, want nil", c.FrontendTypes, c.FrontendSinks)
	}
	env := with(validEnv(), "RECAP_FRONTEND_TYPES=pilos-fixture:meeting-summary",
		"RECAP_FRONTEND_SINKS= pilos-fixture:docs-main ")
	if c, err = Load(env, bothRoles...); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []FrontendName{{"pilos-fixture", "meeting-summary"}}
	if len(c.FrontendTypes) != 1 || c.FrontendTypes[0] != want[0] {
		t.Errorf("FrontendTypes = %v, want %v", c.FrontendTypes, want)
	}
	want = []FrontendName{{"pilos-fixture", "docs-main"}}
	if len(c.FrontendSinks) != 1 || c.FrontendSinks[0] != want[0] {
		t.Errorf("FrontendSinks = %v, want %v", c.FrontendSinks, want)
	}
}

// A broken meeting-types or sinks file is reported once; the allow-list
// names are not checked against it, the front ends still are.
func TestLoadFrontendAllowListsWithBrokenFile(t *testing.T) {
	env := with(validEnv(),
		"RECAP_MEETING_TYPES_FILE=testdata/meeting-types-unknown-key.yaml",
		"RECAP_SINKS_FILE=testdata/sinks-bad-name.yaml",
		"RECAP_FRONTEND_TYPES=pilos-fixture:meeting-summary,moodle-x:meeting-summary",
		"RECAP_FRONTEND_SINKS=pilos-fixture:docs-main")
	_, err := Load(env, bothRoles...)
	if err == nil {
		t.Fatal("Load: want error")
	}
	msg := err.Error()
	for _, w := range []string{"RECAP_MEETING_TYPES_FILE", "RECAP_SINKS_FILE: sinks[0].name",
		`front end "moodle-x" is not in RECAP_FRONTEND_SERVERS`} {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q does not contain %q", msg, w)
		}
	}
	if strings.Contains(msg, "is not configured") {
		t.Errorf("error %q checks allow-list names against a broken file", msg)
	}
}

// Without RECAP_FRONTEND_SERVERS every front end may claim packages of
// every BBB server, and the front ends of RECAP_FRONTEND_TYPES and
// RECAP_FRONTEND_SINKS are not checked against it.
func TestLoadWithoutFrontendServers(t *testing.T) {
	for _, v := range []string{"RECAP_FRONTEND_SERVERS=-", "RECAP_FRONTEND_SERVERS="} {
		env := with(validEnv(), v,
			"RECAP_FRONTEND_TYPES=moodle-x:meeting-summary",
			"RECAP_FRONTEND_SINKS=moodle-x:docs-main")
		c, err := Load(env, bothRoles...)
		if err != nil {
			t.Fatalf("%s: Load: %v", v, err)
		}
		if len(c.FrontendServers) != 0 {
			t.Errorf("%s: FrontendServers = %v, want none", v, c.FrontendServers)
		}
		if len(c.FrontendTypes) != 1 || len(c.FrontendSinks) != 1 {
			t.Errorf("%s: FrontendTypes = %v, FrontendSinks = %v, want one each", v, c.FrontendTypes, c.FrontendSinks)
		}
	}
	// The names are still checked against the files.
	env := with(validEnv(), "RECAP_FRONTEND_SERVERS=-", "RECAP_FRONTEND_TYPES=moodle-x:lecture")
	if _, err := Load(env, RoleWorker); err == nil || !strings.Contains(err.Error(), `meeting type "lecture" is not configured`) {
		t.Errorf("Load: err = %v, want unknown meeting type", err)
	}
}

func TestLoadMigrateNeedsOnlyTheDatabase(t *testing.T) {
	env := []string{
		"RECAP_DB_DSN=postgres://recap@127.0.0.1/recap",
		"RECAP_DB_PASSWORD_FILE=testdata/db-password",
	}
	if _, err := Load(env, RoleMigrate); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		env   []string
		roles []Role
		want  []string // substrings of the error
	}{
		{"unknown variable", with(validEnv(), "RECAP_FOO=1"), bothRoles, []string{"RECAP_FOO: unknown variable"}},
		{"bad frontend servers", with(validEnv(), "RECAP_FRONTEND_SERVERS=pilos"), bothRoles,
			[]string{"RECAP_FRONTEND_SERVERS"}},
		{"bad frontend types", with(validEnv(), "RECAP_FRONTEND_TYPES=pilos-fixture"), bothRoles,
			[]string{"RECAP_FRONTEND_TYPES: item \"pilos-fixture\": want <frontend_id>:<meeting type>"}},
		{"frontend types: unknown front end", with(validEnv(), "RECAP_FRONTEND_TYPES=moodle-x:meeting-summary"),
			[]Role{RoleWorker}, []string{"RECAP_FRONTEND_TYPES", "front end \"moodle-x\" is not in RECAP_FRONTEND_SERVERS"}},
		{"frontend types: unknown type", with(validEnv(), "RECAP_FRONTEND_TYPES=pilos-fixture:lecture"),
			[]Role{RoleAPI}, []string{"RECAP_FRONTEND_TYPES", "meeting type \"lecture\" is not configured"}},
		{"bad frontend sinks", with(validEnv(), "RECAP_FRONTEND_SINKS=pilos-fixture:Docs"), bothRoles,
			[]string{"RECAP_FRONTEND_SINKS: item \"pilos-fixture:Docs\": want <frontend_id>:<sink profile>"}},
		{"frontend sinks: unknown front end", with(validEnv(), "RECAP_FRONTEND_SINKS=moodle-x:docs-main"),
			bothRoles, []string{"RECAP_FRONTEND_SINKS", "front end \"moodle-x\" is not in RECAP_FRONTEND_SERVERS"}},
		{"frontend sinks: unknown sink", with(validEnv(), "RECAP_FRONTEND_SINKS=pilos-fixture:docs-other"),
			bothRoles, []string{"RECAP_FRONTEND_SINKS", "sink profile \"docs-other\" is not configured"}},
		{"missing DSN", with(validEnv(), "RECAP_DB_DSN=-"), bothRoles, []string{"RECAP_DB_DSN: required"}},
		{"DSN with password", with(validEnv(), "RECAP_DB_DSN=postgres://recap:pw@localhost/recap"), bothRoles,
			[]string{"RECAP_DB_DSN: must not contain a password"}},
		{"DSN with password parameter", with(validEnv(), "RECAP_DB_DSN=postgres://recap@localhost/recap?password=pw"),
			bothRoles, []string{"RECAP_DB_DSN: must not contain a password"}},
		{"missing password file", with(validEnv(), "RECAP_DB_PASSWORD_FILE=testdata/nope"), bothRoles,
			[]string{"RECAP_DB_PASSWORD_FILE: cannot read file"}},
		{"bad duration", with(validEnv(), "RECAP_REGISTRATION_TTL=2w"), bothRoles,
			[]string{"RECAP_REGISTRATION_TTL: invalid duration"}},
		{"bad log level", with(validEnv(), "RECAP_LOG_LEVEL=loud"), bothRoles, []string{"RECAP_LOG_LEVEL"}},
		{"bad backend", with(validEnv(), "RECAP_BLOB_BACKEND=ftp"), bothRoles, []string{"RECAP_BLOB_BACKEND"}},
		{"fs without root", with(validEnv(), "RECAP_BLOB_BACKEND=fs"), bothRoles, []string{"RECAP_FS_ROOT: required"}},
		{"s3 without bucket", with(validEnv(), "RECAP_S3_BUCKET=-"), bothRoles, []string{"RECAP_S3_BUCKET: required"}},
		{"bad time zone", with(validEnv(), "RECAP_DISPLAY_TZ=Mars/Base"), bothRoles, []string{"RECAP_DISPLAY_TZ"}},
		{"bad slots", with(validEnv(), "RECAP_WORKER_SLOTS=0"), bothRoles, []string{"RECAP_WORKER_SLOTS"}},
		{"bad meta prefix", with(validEnv(), "RECAP_META_PREFIX=Recap"), bothRoles, []string{"RECAP_META_PREFIX"}},
		{"no client secrets dir", with(validEnv(), "RECAP_CLIENT_SECRETS_DIR=testdata/nope"), []Role{RoleAPI},
			[]string{"RECAP_CLIENT_SECRETS_DIR"}},
		{"unknown meeting type key", with(validEnv(), "RECAP_MEETING_TYPES_FILE=testdata/meeting-types-unknown-key.yaml"),
			bothRoles, []string{"RECAP_MEETING_TYPES_FILE", "prompt"}},
		{"unknown sink key", with(validEnv(), "RECAP_SINKS_FILE=testdata/sinks-unknown-key.yaml"), bothRoles,
			[]string{"RECAP_SINKS_FILE", "retries"}},
		{"bad sink name", with(validEnv(), "RECAP_SINKS_FILE=testdata/sinks-bad-name.yaml"), bothRoles,
			[]string{"RECAP_SINKS_FILE: sinks[0].name", "Docs_Main"}},
		{"missing providers file", with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/nope.yaml"), []Role{RoleWorker},
			[]string{"RECAP_PROVIDERS_FILE: cannot read file"}},
		{"sinks with schema_version 2", with(validEnv(), "RECAP_SINKS_FILE=testdata/sinks-bad-version.yaml"), bothRoles,
			[]string{"RECAP_SINKS_FILE: schema_version: want 1.x.y"}},
		{"providers without schema_version", with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/providers-no-version.yaml"),
			[]Role{RoleWorker}, []string{"RECAP_PROVIDERS_FILE: schema_version: required"}},
		{"bad provider profile", with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/providers-bad-profile.yaml"),
			[]Role{RoleWorker}, []string{"RECAP_PROVIDERS_FILE: profiles[0].declaration.retention: required",
				"RECAP_PROVIDERS_FILE: profiles[1].temperature: unknown key"}},
		{"several errors at once", with(validEnv(), "RECAP_FOO=1", "RECAP_DB_DSN=-", "RECAP_INGEST_STAGING_TTL=soon"),
			bothRoles, []string{"RECAP_FOO", "RECAP_DB_DSN", "RECAP_INGEST_STAGING_TTL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.env, tt.roles...)
			if err == nil {
				t.Fatal("Load: want error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// privacy.md §7: declarations are optional; a profile without one loads.
func TestLoadWithoutDeclarations(t *testing.T) {
	env := with(validEnv(), "RECAP_SINKS_FILE=testdata/sinks-no-declaration.yaml",
		"RECAP_PROVIDERS_FILE=testdata/providers-no-declaration.yaml")
	c, err := Load(env, bothRoles...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Sinks) != 1 || c.Sinks[0].Declaration != nil {
		t.Errorf("Sinks = %+v, want one sink without declaration", c.Sinks)
	}
	if c.Providers == nil || len(c.Providers.Profiles) != 2 || c.Providers.Profiles[0].Declaration != nil ||
		c.Providers.Profiles[1].Declaration != nil {
		t.Errorf("Providers = %+v, want two profiles without declaration", c.Providers)
	}
}

func TestProvidersFileParsedOnlyByWorker(t *testing.T) {
	env := with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/providers-no-version.yaml")
	if _, err := Load(env, RoleAPI); err != nil {
		t.Fatalf("api role must not parse the providers file: %v", err)
	}
	// It is still read for config_hash.
	env = with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/nope.yaml")
	if _, err := Load(env, RoleAPI); err == nil || !strings.Contains(err.Error(), "RECAP_PROVIDERS_FILE") {
		t.Fatalf("api role with an unreadable providers file: got %v, want an error", err)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "14d": 14 * 24 * time.Hour, "72h": 72 * time.Hour, "1h30m": 90 * time.Minute,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "1.5d", "-1h", "-2d", "2w"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q): want error", in)
		}
	}
}

// wantHash computes architecture.md §5.2 by hand: SHA-256 over the
// meeting-types, providers and sinks files, each prefixed by its length
// (8-byte big-endian).
func wantHash(t *testing.T, files ...string) string {
	t.Helper()
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestHash(t *testing.T) {
	want := wantHash(t, "testdata/meeting-types.yaml", "testdata/providers.yaml", "testdata/sinks.yaml")
	for _, roles := range [][]Role{{RoleAPI}, {RoleWorker}, bothRoles} {
		c, err := Load(validEnv(), roles...)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Hash(); got != want {
			t.Errorf("roles %v: Hash = %s, want %s", roles, got, want)
		}
	}

	// Variables are not part of the hash; file contents are.
	c, _ := Load(with(validEnv(), "RECAP_WORKER_SLOTS=5"), RoleAPI)
	if c.Hash() != want {
		t.Error("hash changed with a variable")
	}
	e, err := Load(with(validEnv(), "RECAP_PROVIDERS_FILE=testdata/providers-no-version.yaml"), RoleAPI)
	if err != nil {
		t.Fatal(err)
	}
	if e.Hash() == want {
		t.Error("hash does not change with the providers file")
	}
}

func TestLogBootNeverLogsSecrets(t *testing.T) {
	c, err := Load(with(validEnv(), "RECAP_S3_ENDPOINT=http://minio:9000"), bothRoles...)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	c.LogBoot(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := buf.String()
	if strings.Contains(out, "FIXTURE-NOT-A-SECRET") {
		t.Errorf("boot log contains a secret: %s", out)
	}
	for _, w := range []string{`"RECAP_DB_PASSWORD_FILE":"testdata/db-password"`, `"config_hash":`,
		`"level":"WARN","msg":"S3 endpoint uses plain http"`} {
		if !strings.Contains(out, w) {
			t.Errorf("boot log lacks %s: %s", w, out)
		}
	}
}

// The fs backend is supported on compose: the boot log notes it at INFO
// among the effective values and logs no warning.
func TestLogBootNotesFSWithoutWarning(t *testing.T) {
	c, err := Load(with(validEnv(), "RECAP_BLOB_BACKEND=fs", "RECAP_FS_ROOT=/tmp/x"), bothRoles...)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	c.LogBoot(slog.New(slog.NewJSONHandler(&buf, nil)))
	out := buf.String()
	for _, w := range []string{`"level":"INFO","msg":"configuration loaded"`, `"RECAP_BLOB_BACKEND":"fs"`,
		`"RECAP_FS_ROOT":"/tmp/x"`} {
		if !strings.Contains(out, w) {
			t.Errorf("boot log lacks %s: %s", w, out)
		}
	}
	if strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("boot log warns on fs: %s", out)
	}
}

// The worker role keeps the loaded provider profiles; the api role does not
// parse the file.
func TestProvidersLoadedForWorker(t *testing.T) {
	c, err := Load(validEnv(), RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers == nil || c.Providers.DefaultASR != "whisperx-lab" || len(c.Providers.Profiles) != 2 {
		t.Fatalf("providers = %+v", c.Providers)
	}
	c, err = Load(validEnv(), RoleAPI)
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers != nil {
		t.Fatalf("api role: providers = %+v, want nil", c.Providers)
	}
}
