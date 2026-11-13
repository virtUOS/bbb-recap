// Package manifest implements the meeting package manifest of
// docs/reference/meeting-package.md: RFC 8785 (JCS) for the pinned subset, the
// ingest rules V-I01…V-I05, V-I07 and V-I08 (§7), the limits of §8 and the
// allow-list selection of §5. Ingest, `recap tool validate-package` and the
// exporter share it.
//
// Rule order in Check, stopping at the first violation
// (contracts/package/README.md): V-I01, V-I02, V-I03 (only with a server ID),
// V-I04, V-I07, V-I08. The limits of §8 (413 package-too-large) are checked
// right after the schema version, before the full schema. V-I05 is a separate
// per-file check (CheckFile, CheckFiles). V-I06 needs ingest state and is not
// here.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/virtUOS/bbb-recap/internal/jcs"
	"github.com/virtUOS/bbb-recap/internal/schemas"
)

// Supported schema_version: major 1, minors up to SupportedMinor (§8).
const (
	SupportedMajor = 1
	SupportedMinor = 0
)

// DefaultMetaPrefix is the default of RECAP_META_PREFIX (deployment.md §5.1).
const DefaultMetaPrefix = "recap"

// Manifest is manifest.json, schema_version 1.0.x (§4).
type Manifest struct {
	SchemaVersion string            `json:"schema_version"`
	CreatedAt     string            `json:"created_at"`
	Exporter      Exporter          `json:"exporter"`
	BBBServerID   string            `json:"bbb_server_id"`
	RecordID      string            `json:"record_id"`
	MeetingID     string            `json:"meeting_id"`
	BBBVersion    string            `json:"bbb_version,omitempty"`
	Breakout      bool              `json:"breakout"`
	MetaPrefix    string            `json:"meta_prefix"`
	Metadata      map[string]string `json:"metadata"`
	Files         []File            `json:"files"`
	Totals        Totals            `json:"totals"`
}

// Exporter names the program that built the manifest.
type Exporter struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// File is one files entry.
type File struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Totals are the file count and the sum of sizes.
type Totals struct {
	FileCount int   `json:"file_count"`
	Size      int64 `json:"size"`
}

// Violation is the first rule a manifest or file breaks, with the problem
// slug (urn:recap:problem:<slug>) and HTTP status of §7 and bbb-exporter.md
// §6.2. Detail is for DEBUG logs only: it may quote manifest values.
type Violation struct {
	Rule    string
	Problem string
	Status  int
	Detail  string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("%s %s (%d): %s", v.Rule, v.Problem, v.Status, v.Detail)
}

// Problem slugs.
const (
	ProblemPackageIDMismatch  = "package-id-mismatch"
	ProblemManifestInvalid    = "manifest-invalid"
	ProblemUnsupportedVersion = "unsupported-schema-version"
	ProblemServerMismatch     = "server-mismatch"
	ProblemFileHashMismatch   = "file-hash-mismatch"
	ProblemNotOptedIn         = "not-opted-in"
	ProblemPackageTooLarge    = "package-too-large"
)

func violation(rule, problem string, status int, format string, args ...any) *Violation {
	return &Violation{Rule: rule, Problem: problem, Status: status, Detail: fmt.Sprintf(format, args...)}
}

// Options carry what the ingest request adds to the manifest bytes.
type Options struct {
	// PackageID from the ingest URL. Empty: V-I01 checks only that the bytes
	// are canonical.
	PackageID string
	// BBBServerID bound to the upload credential. Empty: V-I03 is skipped.
	BBBServerID string
	// MetaPrefix is the service's RECAP_META_PREFIX; empty means "recap".
	MetaPrefix string
}

// PackageID returns the lowercase hex SHA-256 of the manifest bytes.
func PackageID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Check applies the ingest rules to the manifest bytes. It returns the parsed
// manifest once the bytes have passed the schema (also when a later rule
// fails), and the first violation or nil.
func Check(data []byte, opts Options) (*Manifest, *Violation) {
	// V-I01: SHA-256 = package_id, bytes canonical.
	if opts.PackageID != "" && PackageID(data) != opts.PackageID {
		return nil, violation("V-I01", ProblemPackageIDMismatch, 400, "SHA-256 of the manifest differs from the package ID")
	}
	canon, err := jcs.Canonicalize(data)
	if err != nil {
		// Not JSON of the supported subset (e.g. a float): no valid manifest.
		return nil, violation("V-I02", ProblemManifestInvalid, 422, "%v", err)
	}
	if !bytes.Equal(canon, data) {
		return nil, violation("V-I01", ProblemPackageIDMismatch, 400, "manifest bytes are not JCS-canonical")
	}

	// V-I02, part 1: a schema_version this reader has a schema for.
	var head struct {
		SchemaVersion any `json:"schema_version"`
	}
	_ = json.Unmarshal(data, &head) // a non-object fails the schema below
	if s, ok := head.SchemaVersion.(string); ok {
		if m := semverRE.FindStringSubmatch(s); m != nil {
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			if major != SupportedMajor || minor > SupportedMinor {
				return nil, violation("V-I02", ProblemUnsupportedVersion, 422,
					"schema_version %s; supported %d.0 to %d.%d", s, SupportedMajor, SupportedMajor, SupportedMinor)
			}
		}
	}

	// V-E03 at ingest: limits of §8.
	if v := checkLimitsLenient(data); v != nil {
		return nil, v
	}

	// V-I02, part 2: the JSON Schema of the version.
	if err := schemas.Validate(schemas.Manifest, data); err != nil {
		return nil, violation("V-I02", ProblemManifestInvalid, 422, "schema: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, violation("V-I02", ProblemManifestInvalid, 422, "decode: %v", err)
	}

	// V-I03: the server of the credential.
	if opts.BBBServerID != "" && m.BBBServerID != opts.BBBServerID {
		return &m, violation("V-I03", ProblemServerMismatch, 403, "bbb_server_id is not the credential's server")
	}

	// V-I04: paths unique and sorted, totals right. The role path patterns
	// are part of the schema and already hold.
	if v := checkFileList(&m); v != nil {
		return &m, v
	}

	// V-I07: opted in with the configured prefix.
	prefix := opts.MetaPrefix
	if prefix == "" {
		prefix = DefaultMetaPrefix
	}
	if m.Metadata[prefix+"-enabled"] != "true" {
		return &m, violation("V-I07", ProblemNotOptedIn, 422, "metadata %s-enabled is not \"true\"", prefix)
	}

	// V-I08: no breakout recordings.
	if m.Breakout {
		return &m, violation("V-I08", ProblemManifestInvalid, 422, "breakout is true")
	}
	return &m, nil
}

// checkFileList checks V-I04.
func checkFileList(m *Manifest) *Violation {
	var size int64
	for i, f := range m.Files {
		if i > 0 {
			prev := m.Files[i-1].Path
			if f.Path == prev {
				return violation("V-I04", ProblemManifestInvalid, 422, "path %q listed twice", f.Path)
			}
			if f.Path < prev { // Go compares strings bytewise
				return violation("V-I04", ProblemManifestInvalid, 422, "path %q not sorted after %q", f.Path, prev)
			}
		}
		size += f.Size
	}
	if m.Totals.FileCount != len(m.Files) || m.Totals.Size != size {
		return violation("V-I04", ProblemManifestInvalid, 422,
			"totals {file_count %d, size %d}, entries {%d, %d}", m.Totals.FileCount, m.Totals.Size, len(m.Files), size)
	}
	return nil
}

// Marshal returns the canonical bytes of m; their SHA-256 is the package_id.
func Marshal(m *Manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jcs.Canonicalize(b)
}
