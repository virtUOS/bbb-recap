package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	f01Golden   = "../../testdata/meetings/F-01/expected/manifest.json"
	f01ID       = "../../testdata/meetings/F-01/expected/package_id.txt"
	f01RawDir   = "../../testdata/meetings/F-01/raw/536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
	invalidDir  = "../../testdata/manifests/invalid"
	f01ServerID = "bbb-fixture-a"
)

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mutate parses the golden F-01 manifest, applies fn and returns the
// canonical bytes.
func mutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(readFile(t, f01Golden)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantViolation(t *testing.T, v *Violation, rule, problem string, status int) {
	t.Helper()
	if v == nil {
		t.Fatalf("no violation, want %s %s %d", rule, problem, status)
	}
	if v.Rule != rule || v.Problem != problem || v.Status != status {
		t.Fatalf("violation %s %s %d (%s), want %s %s %d", v.Rule, v.Problem, v.Status, v.Detail, rule, problem, status)
	}
}

func TestGoldenF01(t *testing.T) {
	data := readFile(t, f01Golden)
	id := strings.TrimSpace(string(readFile(t, f01ID)))
	if got := PackageID(data); got != id {
		t.Fatalf("PackageID = %s, package_id.txt = %s", got, id)
	}
	m, v := Check(data, Options{PackageID: id, BBBServerID: f01ServerID})
	if v != nil {
		t.Fatalf("golden F-01: %v", v)
	}
	if m.RecordID != "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000" || len(m.Files) != 9 || m.Totals.Size != 393967 {
		t.Fatalf("parsed manifest: %+v", m)
	}
	if v, err := CheckFiles(f01RawDir, m.Files); v != nil || err != nil {
		t.Fatalf("CheckFiles on F-01: %v, %v", v, err)
	}
	again, err := Marshal(m)
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("Marshal does not reproduce the golden bytes (err %v)", err)
	}
}

// Every testdata/manifests/invalid/<M-ID>.<rule>.json fails for its rule.
func TestInvalidManifests(t *testing.T) {
	outcomes := map[string]struct {
		rule, problem string
		status        int
	}{
		"V-I01-non-canonical":              {"V-I01", "package-id-mismatch", 400},
		"V-I02-schema":                     {"V-I02", "manifest-invalid", 422},
		"V-I02-unsupported-schema-version": {"V-I02", "unsupported-schema-version", 422},
		"V-I04-paths-duplicate":            {"V-I04", "manifest-invalid", 422},
		"V-I04-paths-unsorted":             {"V-I04", "manifest-invalid", 422},
		"V-I04-totals":                     {"V-I04", "manifest-invalid", 422},
		"V-I08-breakout":                   {"V-I08", "manifest-invalid", 422},
	}
	paths, err := filepath.Glob(filepath.Join(invalidDir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no invalid manifests: %v", err)
	}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		t.Run(name, func(t *testing.T) {
			_, rule, _ := strings.Cut(name, ".")
			want, ok := outcomes[rule]
			if !ok {
				t.Fatalf("unknown rule %q in file name", rule)
			}
			_, v := Check(readFile(t, p), Options{})
			wantViolation(t, v, want.rule, want.problem, want.status)
		})
	}
}

func TestRules(t *testing.T) {
	golden := readFile(t, f01Golden)
	t.Run("V-I01-wrong-package-id", func(t *testing.T) {
		_, v := Check(golden, Options{PackageID: strings.Repeat("0", 64)})
		wantViolation(t, v, "V-I01", ProblemPackageIDMismatch, 400)
	})
	t.Run("V-I02-float", func(t *testing.T) {
		data := bytes.Replace(golden, []byte(`"size":350}`), []byte(`"size":350.0}`), 1)
		_, v := Check(data, Options{})
		wantViolation(t, v, "V-I02", ProblemManifestInvalid, 422)
	})
	t.Run("V-I02-not-json", func(t *testing.T) {
		_, v := Check([]byte("not json"), Options{})
		wantViolation(t, v, "V-I02", ProblemManifestInvalid, 422)
	})
	t.Run("V-I02-newer-patch-ok", func(t *testing.T) {
		data := mutate(t, func(m map[string]any) { m["schema_version"] = "1.0.7" })
		if _, v := Check(data, Options{}); v != nil {
			t.Fatalf("1.0.7: %v", v)
		}
	})
	t.Run("V-I03", func(t *testing.T) {
		_, v := Check(golden, Options{BBBServerID: "bbb-fixture-b"})
		wantViolation(t, v, "V-I03", ProblemServerMismatch, 403)
	})
	t.Run("V-I07", func(t *testing.T) {
		for _, val := range []any{"yes", nil} {
			data := mutate(t, func(m map[string]any) {
				md := m["metadata"].(map[string]any)
				if val == nil {
					delete(md, "recap-enabled")
				} else {
					md["recap-enabled"] = val
				}
			})
			_, v := Check(data, Options{})
			wantViolation(t, v, "V-I07", ProblemNotOptedIn, 422)
		}
	})
	t.Run("V-I07-other-prefix", func(t *testing.T) {
		_, v := Check(golden, Options{MetaPrefix: "other"})
		wantViolation(t, v, "V-I07", ProblemNotOptedIn, 422)
	})
	t.Run("V-I08", func(t *testing.T) {
		data := mutate(t, func(m map[string]any) { m["breakout"] = true })
		m, v := Check(data, Options{})
		wantViolation(t, v, "V-I08", ProblemManifestInvalid, 422)
		if m == nil {
			t.Fatal("no parsed manifest with a V-I08 violation")
		}
	})
	t.Run("limits", func(t *testing.T) {
		big := mutate(t, func(m map[string]any) {
			files := m["files"].([]any)
			f := files[0].(map[string]any)
			f["size"] = json.Number("3221225472") // 3 GiB
			m["totals"].(map[string]any)["size"] = json.Number("3221525472")
		})
		_, v := Check(big, Options{})
		wantViolation(t, v, "V-E03", ProblemPackageTooLarge, 413)
	})
}

func TestCheckLimits(t *testing.T) {
	if v := CheckLimits([]File{{Size: MaxFileSize}, {Size: MaxFileSize}}); v != nil {
		t.Fatalf("at the limit: %v", v)
	}
	if v := CheckLimits([]File{{Size: MaxFileSize + 1}}); v == nil {
		t.Fatal("file over 2 GiB accepted")
	}
	five := make([]File, 5)
	for i := range five {
		five[i].Size = MaxFileSize
	}
	if v := CheckLimits(five); v == nil {
		t.Fatal("package over 8 GiB accepted")
	}
	if v := CheckLimits(make([]File, MaxFiles+1)); v == nil {
		t.Fatal("more than 10 000 files accepted")
	}
}

// V-I05: M-01 flips a byte in a microphone file.
func TestCheckFilesMismatch(t *testing.T) {
	m, v := Check(readFile(t, f01Golden), Options{})
	if v != nil {
		t.Fatal(v)
	}
	dir := t.TempDir()
	for _, f := range m.Files {
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, readFile(t, filepath.Join(f01RawDir, filepath.FromSlash(f.Path))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := CheckFiles(dir, m.Files); v != nil || err != nil {
		t.Fatalf("copied package: %v, %v", v, err)
	}

	mic := filepath.Join(dir, "audio", "microphone-w_fixada0001-TR_fixA1-1761732005200.webm")
	b := readFile(t, mic)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(mic, b, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := CheckFiles(dir, m.Files)
	if err != nil {
		t.Fatal(err)
	}
	wantViolation(t, v, "V-I05", ProblemFileHashMismatch, 422)

	// Missing file, and a symlink in place of a file.
	if err := os.Remove(mic); err != nil {
		t.Fatal(err)
	}
	v, _ = CheckFiles(dir, m.Files)
	wantViolation(t, v, "V-I05", ProblemFileHashMismatch, 422)
	if err := os.Symlink(filepath.Join(f01RawDir, "audio", filepath.Base(mic)), mic); err != nil {
		t.Fatal(err)
	}
	v, _ = CheckFiles(dir, m.Files)
	wantViolation(t, v, "V-I05", ProblemFileHashMismatch, 422)
}
