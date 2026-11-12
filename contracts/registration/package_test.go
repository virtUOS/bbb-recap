package registration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// optedIn returns the metadata of an opted-in fixture (fixtures.md §3).
func optedIn() map[string]string {
	return map[string]string{
		"recap-v":       "1",
		"recap-enabled": "true",
		"recap-type":    "meeting-summary",
		"recap-sink":    "docs-main",
	}
}

// with returns a copy of m with k set to v.
func with(m map[string]string, k, v string) map[string]string {
	c := make(map[string]string, len(m)+1)
	for key, val := range m {
		c[key] = val
	}
	c[k] = v
	return c
}

// without returns a copy of m without k.
func without(m map[string]string, k string) map[string]string {
	c := with(m, k, "")
	delete(c, k)
	return c
}

// pkgOpts describes a package built from a fixture's raw dir and golden manifest.
type pkgOpts struct {
	base      string // fixture with a raw dir and a golden manifest: F-01 (default), F-04, F-06
	server    string // bbb_server_id; default bbb-fixture-a
	recordID  string // required: replaces the fixture's record_id everywhere
	meetingID string // required: replaces the fixture's meeting_id
	// meta replaces every recap-* attribute of /recording/metadata in
	// events.xml; nil keeps the fixture's.
	meta map[string]string
	// manifestMeta, if set, replaces the recap-* and bbb-origin keys of the
	// manifest's metadata only, so it differs from events.xml.
	manifestMeta map[string]string
}

// pkg is a meeting package ready for ingest.
type pkg struct {
	id       string            // package_id
	manifest []byte            // canonical manifest bytes
	files    map[string][]byte // sha256 → content
	server   string
	recordID string
}

var (
	metadataElem = regexp.MustCompile(`<metadata ([^>]*)/>`)
	xmlAttr      = regexp.MustCompile(`([A-Za-z0-9_.:-]+)="([^"]*)"`)
)

// buildPackage builds a package the way meeting-package.md §4 describes, from
// a fixture with fresh IDs and the given metadata. It first rebuilds the
// unchanged fixture and checks the golden package_id, so a broken builder
// fails loudly instead of testing the wrong thing.
func buildPackage(t *testing.T, o pkgOpts) *pkg {
	t.Helper()
	if o.base == "" {
		o.base = "F-01"
	}
	golden, err := os.ReadFile(filepath.Join(testdataDir, "meetings", o.base, "expected", "package_id.txt"))
	if err != nil {
		t.Fatal(err)
	}
	self := build(t, pkgOpts{base: o.base})
	if self.id != strings.TrimSpace(string(golden)) {
		t.Fatalf("package builder: unchanged %s gives package_id %s, golden %s", o.base, self.id, golden)
	}
	if o.recordID == "" || o.meetingID == "" {
		t.Fatal("package builder: recordID and meetingID are required")
	}
	return build(t, o)
}

// build does the work of buildPackage; empty IDs keep the fixture's.
func build(t *testing.T, o pkgOpts) *pkg {
	t.Helper()
	dir := filepath.Join(testdataDir, "meetings", o.base)
	mb, err := os.ReadFile(filepath.Join(dir, "expected", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(mb))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	baseRecord, _ := m["record_id"].(string)
	baseMeeting, _ := m["meeting_id"].(string)
	raw := filepath.Join(dir, "raw", baseRecord)
	if o.recordID == "" {
		o.recordID = baseRecord
	}
	if o.meetingID == "" {
		o.meetingID = baseMeeting
	}
	if o.server == "" {
		o.server, _ = m["bbb_server_id"].(string)
	}

	ev, err := os.ReadFile(filepath.Join(raw, "events.xml"))
	if err != nil {
		t.Fatal(err)
	}
	events, attrs := rewriteEvents(t, string(ev), baseRecord, o.recordID, baseMeeting, o.meetingID, o.meta)

	meta := map[string]any{}
	for k, v := range attrs {
		if isManifestMetaKey(k) {
			meta[k] = v
		}
	}
	if o.manifestMeta != nil {
		for k := range meta {
			delete(meta, k)
		}
		for k, v := range o.manifestMeta {
			meta[k] = v
		}
	}

	p := &pkg{files: map[string][]byte{}, server: o.server, recordID: o.recordID}
	var files []any
	var total int64
	for _, f := range m["files"].([]any) {
		path := f.(map[string]any)["path"].(string)
		role := f.(map[string]any)["role"].(string)
		data := []byte(events)
		if path != "events.xml" {
			if data, err = os.ReadFile(filepath.Join(raw, filepath.FromSlash(path))); err != nil {
				t.Fatal(err)
			}
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		p.files[sha] = data
		total += int64(len(data))
		files = append(files, map[string]any{"path": path, "role": role, "sha256": sha, "size": int64(len(data))})
	}
	m["files"] = files
	m["totals"] = map[string]any{"file_count": int64(len(files)), "size": total}
	m["metadata"] = meta
	m["record_id"] = o.recordID
	m["meeting_id"] = o.meetingID
	m["bbb_server_id"] = o.server

	if p.manifest, err = canonicalJSON(m); err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	sum := sha256.Sum256(p.manifest)
	p.id = hex.EncodeToString(sum[:])
	return p
}

// isManifestMetaKey: keys copied into the manifest (meeting-package.md §4).
func isManifestMetaKey(k string) bool {
	return strings.HasPrefix(k, "recap-") || k == "bbb-origin" || k == "bbb-origin-server-name"
}

// rewriteEvents replaces record and meeting IDs in events.xml and, if meta is
// set, the recap-* attributes of <metadata>. It returns the new XML and the
// final metadata attributes (unescaped).
func rewriteEvents(t *testing.T, s, oldRecord, newRecord, oldMeeting, newMeeting string, meta map[string]string) (string, map[string]string) {
	t.Helper()
	if !strings.Contains(s, `meeting_id="`+oldRecord+`"`) {
		t.Fatalf("events.xml: no /recording/@meeting_id %s", oldRecord)
	}
	s = strings.ReplaceAll(s, oldRecord, newRecord)
	for _, attr := range []string{"externalId", "meetingId"} {
		old := attr + `="` + oldMeeting + `"`
		if strings.Count(s, old) != 1 {
			t.Fatalf("events.xml: want exactly one %s", old)
		}
		s = strings.Replace(s, old, attr+`="`+xmlEscape(newMeeting)+`"`, 1)
	}
	loc := metadataElem.FindAllStringSubmatchIndex(s, -1)
	if len(loc) != 1 {
		t.Fatalf("events.xml: want exactly one <metadata/> element, found %d", len(loc))
	}
	attrs := map[string]string{}
	for _, a := range xmlAttr.FindAllStringSubmatch(s[loc[0][2]:loc[0][3]], -1) {
		attrs[a[1]] = html.UnescapeString(a[2])
	}
	if meta != nil {
		for k := range attrs {
			if strings.HasPrefix(k, "recap-") {
				delete(attrs, k)
			}
		}
		for k, v := range meta {
			attrs[k] = v
		}
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("<metadata")
	for _, k := range keys {
		fmt.Fprintf(&b, ` %s="%s"`, k, xmlEscape(attrs[k]))
	}
	b.WriteString("/>")
	return s[:loc[0][0]] + b.String() + s[loc[0][1]:], attrs
}

// xmlEscape escapes an attribute value. Control characters become character
// references, so they survive attribute value normalization.
func xmlEscape(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '"':
			b.WriteString("&quot;")
		case r < 0x20:
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// canonicalJSON serializes v per RFC 8785 (JCS) for the value types a manifest
// has: objects, arrays, strings, integers, booleans and null.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeJCS(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeJCS(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeJCSString(b, x)
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case json.Number:
		n, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return fmt.Errorf("only integers are supported, got %s", x)
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// RFC 8785 §3.2.3: sort by UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool {
			return slices.Compare(utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))) < 0
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJCSString(b, k)
			b.WriteByte(':')
			if err := writeJCS(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

// writeJCSString escapes per RFC 8785 §3.2.2.2.
func writeJCSString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
