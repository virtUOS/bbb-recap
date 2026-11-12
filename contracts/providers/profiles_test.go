package providers

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// declarationKeys are the fields of a profile declaration (privacy.md §7).
var declarationKeys = []string{
	"data_categories", "operator", "location", "retention", "deletion",
	"used_for_training", "contract_ref", "reviewed_by", "reviewed_at",
}

// loadOnly runs `recap tool asr` with no operations, which only loads and
// validates the providers file (README.md "Start").
func loadOnly(t *testing.T, bin string, p profiles) (stdout, stderr []byte, code int) {
	t.Helper()
	return execTool(t, bin, toolEnv{providersFile: p.write(t)}, []byte(`{"operations": []}`), "asr")
}

func expectLoads(t *testing.T, bin string, p profiles) {
	t.Helper()
	stdout, stderr, code := loadOnly(t, bin, p)
	if code != 0 {
		t.Fatalf("exit %d, want 0 for a valid providers file\nstderr: %s", code, stderr)
	}
	if !bytes.Contains(stdout, []byte(`"operations"`)) {
		t.Fatalf("stdout = %q, want {\"operations\": []}", stdout)
	}
}

// expectStartupError wants exit 2, empty stdout and the key named on stderr.
func expectStartupError(t *testing.T, bin string, p profiles, key string) {
	t.Helper()
	stdout, stderr, code := loadOnly(t, bin, p)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (configuration error)\nstderr: %s", code, stderr)
	}
	if len(stdout) != 0 {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	// The key must appear as a whole word (letters, digits, "_" and "-" count
	// as word characters), so a value such as "fixture-no-such-kind" or a
	// longer key does not count.
	if !regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(key) + `($|[^A-Za-z0-9_-])`).Match(stderr) {
		t.Errorf("stderr does not name %q:\n%s", key, stderr)
	}
}

// expectLoadsWithWarn wants the file to load and exactly one WARN line on
// stderr that names the profile (privacy.md §7: a profile without a
// declaration loads with one WARN).
func expectLoadsWithWarn(t *testing.T, bin string, p profiles, profile string) {
	t.Helper()
	stdout, stderr, code := loadOnly(t, bin, p)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, stderr)
	}
	if !bytes.Contains(stdout, []byte(`"operations"`)) {
		t.Fatalf("stdout = %q, want {\"operations\": []}", stdout)
	}
	warns := 0
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		var rec struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(line, &rec) == nil && strings.EqualFold(rec.Level, "warn") &&
			bytes.Contains(line, []byte(`"`+profile+`"`)) {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("%d WARN lines name profile %q, want 1\nstderr: %s", warns, profile, stderr)
	}
}

func TestProfiles(t *testing.T) {
	t.Run("CT-PROV-12 provider profiles", func(t *testing.T) { testProfiles(t, harness.Bin(t)) })
}

// §4.1: the fixture loads; a profile without a declaration loads with one
// WARN; each missing required key, unknown key, incomplete declaration,
// unreadable credentials_file, bad defaults entry and bad name stops the
// start with an error naming the key.
func testProfiles(t *testing.T, bin string) {
	base := loadProfiles(t)

	t.Run("valid-fixture", func(t *testing.T) { expectLoads(t, bin, base) })
	t.Run("optional-credentials_file-absent", func(t *testing.T) {
		expectLoads(t, bin, base.without(t, "credentials_file", asrProfile))
	})
	t.Run("optional-extra_body-absent", func(t *testing.T) {
		expectLoads(t, bin, base.without(t, "extra_body", 1))
	})
	t.Run("optional-structured_output-absent", func(t *testing.T) {
		expectLoads(t, bin, base.without(t, "structured_output", 1))
	})
	t.Run("optional-max_inflight-absent", func(t *testing.T) {
		expectLoads(t, bin, base.without(t, "max_inflight", asrProfile).without(t, "max_inflight", 1))
	})
	t.Run("optional-max_file_bytes-absent", func(t *testing.T) {
		expectLoads(t, bin, base.without(t, "max_file_bytes", 1))
	})
	t.Run("optional-declaration-absent-asr", func(t *testing.T) {
		expectLoadsWithWarn(t, bin, base.without(t, "declaration", asrProfile), "whisperx-lab")
	})
	t.Run("optional-declaration-absent-llm", func(t *testing.T) {
		expectLoadsWithWarn(t, bin, base.without(t, "declaration", llmProfile), "vllm-lab")
	})

	missing := []struct {
		key string
		n   int
	}{
		{"schema_version", 1}, {"defaults", 1}, {"profiles", 1},
		{"name", asrProfile}, {"kind", asrProfile}, {"dialect", asrProfile}, {"base_url", asrProfile},
		{"model", asrProfile},
		{"name", llmProfile}, {"base_url", llmProfile}, {"model", llmProfile},
		{"context_tokens", 1}, {"max_output_tokens", 1},
	}
	for _, m := range missing {
		t.Run("missing-"+m.key+"-"+profileName(m.n), func(t *testing.T) {
			expectStartupError(t, bin, base.without(t, m.key, m.n), m.key)
		})
	}
	for _, key := range declarationKeys {
		for _, n := range []int{asrProfile, llmProfile} {
			t.Run("declaration-without-"+key+"-"+profileName(n), func(t *testing.T) {
				expectStartupError(t, bin, base.without(t, key, n), key)
			})
		}
	}

	t.Run("unknown-key-top-level", func(t *testing.T) {
		expectStartupError(t, bin, base.with(t, "schema_version", 1, "fixture_unknown_top: 1"), "fixture_unknown_top")
	})
	t.Run("unknown-key-profile", func(t *testing.T) {
		expectStartupError(t, bin, base.with(t, "model", asrProfile, "fixture_unknown_profile: x"), "fixture_unknown_profile")
	})
	t.Run("unknown-key-declaration", func(t *testing.T) {
		expectStartupError(t, bin, base.with(t, "operator", llmProfile, "fixture_unknown_decl: x"), "fixture_unknown_decl")
	})
	t.Run("unknown-dialect", func(t *testing.T) {
		expectStartupError(t, bin, base.set(t, "dialect", asrProfile, "fixture-no-such-dialect"), "dialect")
	})
	t.Run("unknown-kind", func(t *testing.T) {
		expectStartupError(t, bin, base.set(t, "kind", asrProfile, "fixture-no-such-kind"), "kind")
	})
	t.Run("unreadable-credentials_file", func(t *testing.T) {
		p := base.set(t, "credentials_file", llmProfile, testdataPath(t, "secrets", "no-such-fixture.key"))
		expectStartupError(t, bin, p, "credentials_file")
	})
	t.Run("defaults-unknown-profile", func(t *testing.T) {
		expectStartupError(t, bin, base.set(t, "asr", 1, "no-such-profile"), "defaults")
	})
	t.Run("defaults-wrong-kind", func(t *testing.T) {
		expectStartupError(t, bin, base.set(t, "llm", 1, "whisperx-lab"), "defaults")
	})
	t.Run("bad-name", func(t *testing.T) {
		// Names follow ^[a-z][a-z0-9-]{0,62}$ (README "Grammars"); defaults
		// follows the rename so the name is the only error.
		p := base.set(t, "name", asrProfile, "Whisperx_Lab").set(t, "asr", 1, "Whisperx_Lab")
		expectStartupError(t, bin, p, "name")
	})
}

func profileName(n int) string {
	if n == asrProfile {
		return "asr"
	}
	return "llm"
}
