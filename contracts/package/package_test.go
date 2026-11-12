// Package meetingpackage holds the black-box contract tests for the meeting
// package (docs/reference/meeting-package.md §9). The test IDs are the subtest
// names; the tool's I/O is defined in README.md in this directory.
package meetingpackage

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// invalidOutcomes maps the <rule> part of testdata/manifests/invalid/<M-ID>.<rule>.json
// (testdata/README.md) to the outcome in meeting-package.md §7.
var invalidOutcomes = map[string]ingestOutcome{
	"V-I01-non-canonical":              {"V-I01", "package-id-mismatch", 400},
	"V-I02-schema":                     {"V-I02", "manifest-invalid", 422},
	"V-I02-unsupported-schema-version": {"V-I02", "unsupported-schema-version", 422},
	"V-I04-paths-duplicate":            {"V-I04", "manifest-invalid", 422},
	"V-I04-paths-unsorted":             {"V-I04", "manifest-invalid", 422},
	"V-I04-totals":                     {"V-I04", "manifest-invalid", 422},
	"V-I08-breakout":                   {"V-I08", "manifest-invalid", 422},
}

// checkInvalidManifest validates one invalid manifest against F-01's files
// and expects the outcome its file name documents.
func checkInvalidManifest(t *testing.T, bin, path string) {
	t.Helper()
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	_, rule, ok := strings.Cut(name, ".")
	want, known := invalidOutcomes[rule]
	if !ok || !known {
		t.Fatalf("%s: unknown rule %q in file name", path, rule)
	}
	v := validate(t, bin, "--manifest", path, rawDirFor(t, f01RecordID))
	wantIngestViolation(t, v, want)
}

// Schema and manifest rules: golden manifests are valid, each invalid
// manifest fails for the rule in its name (§7, fixtures.md §6).
func TestManifestSchema(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-PKG-01", func(t *testing.T) {
		for _, path := range goldenManifests(t) {
			t.Run("golden/"+fixtureName(path), func(t *testing.T) {
				m := loadManifest(t, path)
				v := validate(t, bin, "--manifest", path, rawDirFor(t, m["record_id"].(string)))
				wantValid(t, v, nil)
			})
		}
		for _, path := range glob(t, testdataPath(t, "manifests", "invalid", "*.json")) {
			t.Run("invalid/"+filepath.Base(path), func(t *testing.T) {
				checkInvalidManifest(t, bin, path)
			})
		}
	})
}

// package_id: SHA-256 of the canonical manifest bytes = golden package_id.txt,
// and the golden bytes are already canonical (§4).
func TestPackageID(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-PKG-02", func(t *testing.T) {
		for _, path := range goldenManifests(t) {
			t.Run(fixtureName(path), func(t *testing.T) {
				raw := readFile(t, path)
				want := strings.TrimSpace(string(readFile(t, filepath.Join(filepath.Dir(path), "package_id.txt"))))
				if got := sha256Hex(raw); got != want {
					t.Fatalf("fixture: SHA-256 of %s = %s, package_id.txt = %s", path, got, want)
				}
				m := loadManifest(t, path)
				v := validate(t, bin, "--manifest", path, "--package-id", want, rawDirFor(t, m["record_id"].(string)))
				wantValid(t, v, nil)
				if v.PackageID == nil || *v.PackageID != want {
					t.Fatalf("package_id = %v, want %s", v.PackageID, want)
				}
				if got := canonicalize(t, bin, raw); !bytes.Equal(got, raw) {
					t.Fatalf("JCS of the golden manifest differs from its bytes:\ngot  %q\nwant %q", got, raw)
				}
			})
		}
	})
}

// Allow-list (§5) on the F-01 raw directory selects exactly the golden files.
func TestAllowList(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-PKG-03", func(t *testing.T) {
		raw := rawDirFor(t, f01RecordID)
		link := filepath.Join(raw, "audio", "link-to-elsewhere.webm")
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("fixture: %s must be a symlink (fixtures.md §3.1): %v", link, err)
		}
		want := loadManifest(t, testdataPath(t, "meetings", "F-01", "expected", "manifest.json")).files(t)

		v := validate(t, bin, raw)
		wantValid(t, v, nil)
		if v.PackageID != nil {
			t.Errorf("raw form: package_id = %q, want null", *v.PackageID)
		}
		for _, f := range v.Files {
			p := f.Path
			if strings.HasPrefix(p, "video/") || strings.HasPrefix(p, "deskshare/") ||
				strings.HasPrefix(p, "learning-dashboard/") || strings.Contains(p, "screen_share_audio-") ||
				p == "notes/notes.pdf" || p == "audio/link-to-elsewhere.webm" {
				t.Errorf("excluded file selected: %s", p)
			}
		}
		if len(v.Files) != len(want) {
			t.Fatalf("selected %d files, want %d:\ngot  %+v\nwant %+v", len(v.Files), len(want), v.Files, want)
		}
		for i := range want {
			if v.Files[i] != want[i] {
				t.Errorf("files[%d] = %+v, want %+v", i, v.Files[i], want[i])
			}
		}

		// Usage error: an unknown flag exits 2 with nothing on stdout.
		stdout, stderr, code := execTool(t, bin, nil, "--no-such-flag", raw)
		if code != 2 || len(stdout) != 0 {
			t.Errorf("unknown flag: exit %d, stdout %q, stderr %q; want exit 2 and empty stdout", code, stdout, stderr)
		}
	})
}

// Ingest rules V-I01…V-I08, one fixture or mutation each (§7, fixtures.md §6).
// V-I06 needs ingest state and runs over HTTP; the others use the tool.
func TestIngestRules(t *testing.T) {
	if os.Getenv(harness.EnvBin) == "" && os.Getenv(harness.EnvBaseURL) == "" {
		t.Skipf("skipped: %s and %s not set", harness.EnvBin, harness.EnvBaseURL)
	}
	golden := testdataPath(t, "meetings", "F-01", "expected", "manifest.json")
	invalid := func(name string) string { return testdataPath(t, "manifests", "invalid", name) }

	t.Run("CT-PKG-05", func(t *testing.T) {
		t.Run("V-I01", func(t *testing.T) {
			bin := harness.Bin(t)
			raw := rawDirFor(t, f01RecordID)
			id := strings.TrimSpace(string(readFile(t, testdataPath(t, "meetings", "F-01", "expected", "package_id.txt"))))
			wantValid(t, validate(t, bin, "--manifest", golden, "--package-id", id, raw), nil)
			wrong := strings.Repeat("0", 64)
			wantIngestViolation(t, validate(t, bin, "--manifest", golden, "--package-id", wrong, raw),
				ingestOutcome{"V-I01", "package-id-mismatch", 400})
			checkInvalidManifest(t, bin, invalid("M-06.V-I01-non-canonical.json"))
		})
		t.Run("V-I02", func(t *testing.T) {
			bin := harness.Bin(t)
			checkInvalidManifest(t, bin, invalid("M-03.V-I02-schema.json"))
			checkInvalidManifest(t, bin, invalid("M-05a.V-I02-unsupported-schema-version.json"))
		})
		t.Run("V-I03", func(t *testing.T) {
			// M-07: upload with the bbb-fixture-b credential.
			bin := harness.Bin(t)
			raw := rawDirFor(t, f01RecordID)
			wantValid(t, validate(t, bin, "--manifest", golden, "--bbb-server-id", "bbb-fixture-a", raw), nil)
			wantIngestViolation(t, validate(t, bin, "--manifest", golden, "--bbb-server-id", "bbb-fixture-b", raw),
				ingestOutcome{"V-I03", "server-mismatch", 403})
		})
		t.Run("V-I04", func(t *testing.T) {
			bin := harness.Bin(t)
			checkInvalidManifest(t, bin, invalid("M-04c.V-I04-totals.json"))
		})
		t.Run("V-I05", func(t *testing.T) {
			// M-01: flip a byte in a microphone file after the manifest is built.
			bin := harness.Bin(t)
			dir := copyRegularFiles(t, rawDirFor(t, f01RecordID))
			mic := filepath.Join(dir, "audio", "microphone-w_fixada0001-TR_fixA1-1761732005200.webm")
			b := readFile(t, mic)
			b[len(b)/2] ^= 0xff
			if err := os.WriteFile(mic, b, 0o644); err != nil {
				t.Fatal(err)
			}
			wantIngestViolation(t, validate(t, bin, "--manifest", golden, dir),
				ingestOutcome{"V-I05", "file-hash-mismatch", 422})
		})
		t.Run("V-I06", func(t *testing.T) {
			// M-08: second package for an ingested record_id.
			testRecordAlreadyIngested(t, harness.BaseURL(t), golden)
		})
		t.Run("V-I07", func(t *testing.T) {
			// M-12: recap-enabled=yes sent to ingest.
			bin := harness.Bin(t)
			m := loadManifest(t, golden)
			m["metadata"].(map[string]any)["recap-enabled"] = "yes"
			path, _ := m.write(t)
			wantIngestViolation(t, validate(t, bin, "--manifest", path, rawDirFor(t, f01RecordID)),
				ingestOutcome{"V-I07", "not-opted-in", 422})
		})
		t.Run("V-I08", func(t *testing.T) {
			// M-13: breakout true.
			bin := harness.Bin(t)
			m := loadManifest(t, golden)
			m["breakout"] = true
			path, _ := m.write(t)
			wantIngestViolation(t, validate(t, bin, "--manifest", path, rawDirFor(t, f01RecordID)),
				ingestOutcome{"V-I08", "manifest-invalid", 422})
		})
	})
}

// testRecordAlreadyIngested PUTs two manifests with different package_ids for
// one fresh record_id (bbb-exporter.md §6.2 endpoint 1, architecture.md §4.1)
// and expects 201, then 409 record-already-ingested.
func testRecordAlreadyIngested(t *testing.T, baseURL, golden string) {
	t.Helper()
	token := strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", "clients", "bbb_server.bbb-fixture-a"))))
	m := loadManifest(t, golden)
	// contracts/README.md: rebuild the unchanged fixture and check its golden package_id first.
	wantID := strings.TrimSpace(string(readFile(t, filepath.Join(filepath.Dir(golden), "package_id.txt"))))
	if got := sha256Hex(canonicalJSON(t, map[string]any(m))); got != wantID {
		t.Fatalf("test helper: rebuilt F-01 manifest has package_id %s, golden %s", got, wantID)
	}
	m["record_id"] = f01RecordID + "-ct" + strconv.FormatInt(time.Now().UnixNano(), 36)
	first := canonicalJSON(t, map[string]any(m))
	m["created_at"] = "2025-10-29T10:11:00Z"
	second := canonicalJSON(t, map[string]any(m))

	if status, _, body := putManifest(t, baseURL, token, first); status != http.StatusCreated {
		t.Fatalf("first manifest PUT: status %d, want 201; body %q", status, body)
	}
	status, ctype, body := putManifest(t, baseURL, token, second)
	if status != http.StatusConflict {
		t.Fatalf("second manifest PUT: status %d, want 409; body %q", status, body)
	}
	if !strings.HasPrefix(ctype, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json (RFC 9457)", ctype)
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &problem); err != nil || problem.Type != "urn:recap:problem:record-already-ingested" {
		t.Fatalf("problem type = %q (err %v), want urn:recap:problem:record-already-ingested", problem.Type, err)
	}
}

func putManifest(t *testing.T, baseURL, token string, manifest []byte) (status int, contentType string, body []byte) {
	t.Helper()
	url := strings.TrimRight(baseURL, "/") + "/api/v1/ingest/packages/" + sha256Hex(manifest)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT manifest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// Validate rules V-S01…V-S05 on F-01, F-02 and the mutation set (§6, §7,
// fixtures.md §6). V-S06 and V-S07 need configuration and are not the tool's.
func TestValidateRules(t *testing.T) {
	bin := harness.Bin(t)
	golden := testdataPath(t, "meetings", "F-01", "expected", "manifest.json")
	t.Run("CT-PKG-06", func(t *testing.T) {
		t.Run("F-01", func(t *testing.T) {
			wantValid(t, validate(t, bin, "--manifest", golden, rawDirFor(t, f01RecordID)), nil)
		})
		t.Run("F-02", func(t *testing.T) {
			// No manifest exists for F-02 (fixtures.md §7): raw form.
			var want struct {
				Job struct{ State, Reason string } `json:"job"`
			}
			if err := json.Unmarshal(readFile(t, testdataPath(t, "meetings", "F-02", "expected", "service.json")), &want); err != nil {
				t.Fatal(err)
			}
			raw := glob(t, testdataPath(t, "meetings", "F-02", "raw", "*"))[0]
			wantStepViolation(t, validate(t, bin, raw), []string{"V-S02", "V-S03"}, want.Job.State, want.Job.Reason)
		})
		t.Run("M-09", func(t *testing.T) {
			// Manifest metadata differs from events.xml.
			m := loadManifest(t, golden)
			m["metadata"].(map[string]any)["recap-sink"] = "docs-m09"
			path, _ := m.write(t)
			wantStepViolation(t, validate(t, bin, "--manifest", path, rawDirFor(t, f01RecordID)),
				[]string{"V-S04"}, "failed", "manifest_metadata_mismatch")
		})
		t.Run("M-10", func(t *testing.T) {
			// externalId in events.xml differs from the manifest's meeting_id.
			m := loadManifest(t, golden)
			m["meeting_id"] = "fixture-meeting-m10"
			path, _ := m.write(t)
			wantStepViolation(t, validate(t, bin, "--manifest", path, rawDirFor(t, f01RecordID)),
				[]string{"V-S01"}, "failed", "events_mismatch")
		})
		t.Run("M-11", func(t *testing.T) {
			// Rename Ada's A1 file so no AudioTrackPublishedEvent matches it:
			// the job continues and the file becomes a gap.
			const oldPath = "audio/microphone-w_fixada0001-TR_fixA1-1761732005200.webm"
			const newPath = "audio/microphone-w_fixada0001-TR_fixM11-1761732005200.webm"
			dir := copyRegularFiles(t, rawDirFor(t, f01RecordID))
			if err := os.Rename(filepath.Join(dir, filepath.FromSlash(oldPath)), filepath.Join(dir, filepath.FromSlash(newPath))); err != nil {
				t.Fatal(err)
			}
			m := loadManifest(t, golden)
			entries := m.files(t)
			for i := range entries {
				if entries[i].Path == oldPath {
					entries[i].Path = newPath
				}
			}
			m.setFiles(entries)
			path, _ := m.write(t)
			wantValid(t, validate(t, bin, "--manifest", path, dir), []string{newPath})
		})
		t.Run("V-S05-none-matches", func(t *testing.T) {
			// Rename every microphone file of F-01 so that none matches an
			// AudioTrackPublishedEvent: failed all_tracks_failed (§7 V-S05).
			dir := copyRegularFiles(t, rawDirFor(t, f01RecordID))
			m := loadManifest(t, golden)
			entries := m.files(t)
			for i := range entries {
				if entries[i].Role != "audio-microphone" {
					continue
				}
				newPath := strings.Replace(entries[i].Path, "-TR_fix", "-TR_nomatch", 1)
				if err := os.Rename(filepath.Join(dir, filepath.FromSlash(entries[i].Path)), filepath.Join(dir, filepath.FromSlash(newPath))); err != nil {
					t.Fatal(err)
				}
				entries[i].Path = newPath
			}
			m.setFiles(entries)
			path, _ := m.write(t)
			wantStepViolation(t, validate(t, bin, "--manifest", path, dir), []string{"V-S05"}, "failed", "all_tracks_failed")
		})
	})
}

// jcsVector is one JCS test vector. In input and want, "%u" stands for a JSON
// \u escape, so the source shows the escapes the RFC uses.
type jcsVector struct{ name, input, want string }

func unescapeVector(s string) []byte { return []byte(strings.ReplaceAll(s, "%u", `\u`)) }

// JCS is pinned for integers, strings, arrays and objects only; a float is
// rejected (contracts/README.md). The rfc-* vectors are from RFC 8785
// (https://www.rfc-editor.org/rfc/rfc8785): §3.2.2 main example without its
// numbers array, and the §3.2.3 sorting example. The int-* and nested vectors
// are ours.
var jcsVectors = []jcsVector{
	{
		"rfc-3.2.2-string-and-literals",
		`{"string": "%u20ac$%u000F%u000aA'%u0042%u0022%u005c\\\"\/",
		  "literals": [null, true, false]}`,
		`{"literals":[null,true,false],"string":"` + "\xe2\x82\xac" + `$%u000f\nA'B\"\\\\\"/"}`,
	},
	{
		"rfc-3.2.3-sorting",
		`{"%u20ac": "Euro Sign", "\r": "Carriage Return", "%ufb33": "Hebrew Letter Dalet With Dagesh", "1": "One",
		  "%ud83d%ude00": "Emoji: Grinning Face", "%u0080": "Control", "%u00f6": "Latin Small Letter O With Diaeresis"}`,
		`{"\r":"Carriage Return","1":"One","` + "\xc2\x80" + `":"Control","` + "\xc3\xb6" +
			`":"Latin Small Letter O With Diaeresis","` + "\xe2\x82\xac" + `":"Euro Sign","` + "\xf0\x9f\x98\x80" +
			`":"Emoji: Grinning Face","` + "\xef\xac\xb3" + `":"Hebrew Letter Dalet With Dagesh"}`,
	},
	{"int-safe-range", `[0, 1, -1, 393658, 9007199254740991, -9007199254740991]`, `[0,1,-1,393658,9007199254740991,-9007199254740991]`},
	{"nested-whitespace", "{ \"b\" : [ 1 , 2 ],\n \"a\" : { \"d\": true, \"c\": null } }", `{"a":{"c":null,"d":true},"b":[1,2]}`},
}

// JCS output of the tool matches the RFC 8785 test vectors.
func TestJCS(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-PKG-07", func(t *testing.T) {
		for _, vec := range jcsVectors {
			t.Run(vec.name, func(t *testing.T) {
				want := unescapeVector(vec.want)
				if got := canonicalize(t, bin, unescapeVector(vec.input)); !bytes.Equal(got, want) {
					t.Fatalf("JCS\ngot  %q\nwant %q", got, want)
				}
			})
		}
		t.Run("float-rejected", func(t *testing.T) {
			// The F-01 manifest is accepted as is; with one size turned into a
			// float it is rejected: non-zero exit, nothing on stdout.
			raw := readFile(t, testdataPath(t, "meetings", "F-01", "expected", "manifest.json"))
			if got := canonicalize(t, bin, raw); !bytes.Equal(got, raw) {
				t.Fatalf("JCS of the golden manifest differs from its bytes:\ngot  %q\nwant %q", got, raw)
			}
			// A fraction and an exponent (35E1 is 350 in value, but not an integer literal).
			for _, float := range []string{"350.5", "35E1"} {
				withFloat := bytes.Replace(raw, []byte(`"size":350}`), []byte(`"size":`+float+`}`), 1)
				if bytes.Equal(withFloat, raw) {
					t.Fatalf("fixture: no \"size\":350 entry in the F-01 manifest")
				}
				stdout, _, code := execTool(t, bin, withFloat, "--jcs")
				if code == 0 || len(stdout) != 0 {
					t.Errorf("size %s: exit %d, stdout %q; want a non-zero exit and empty stdout", float, code, stdout)
				}
			}
		})
	})
}
