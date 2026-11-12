package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// testdata is the fixture root, relative to this package directory (go test runs here).
const testdata = "../../testdata"

// exchangeCase is one golden file in testdata/ingest/exchanges (format: README.md there).
type exchangeCase struct {
	Name     string         `json:"case"`
	Pins     []string       `json:"pins"`
	Fixture  string         `json:"fixture"`
	RecordID string         `json:"record_id"`
	Exporter *outcome       `json:"exporter"` // expected exporter end state; nil = service side only
	Steps    []exchangeStep `json:"steps"`
	// RawCopies are extra files the exporter side adds to its copy of the
	// fixture raw dir (paths relative to the record dir).
	RawCopies []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"raw_copies"`

	file string // file name without .json, used as the subtest name
}

type outcome struct {
	State  string  `json:"state"`
	Reason *string `json:"reason"`
}

type exchangeStep struct {
	Sides     []string  `json:"sides"` // "service", "exporter"; empty = both
	Request   request   `json:"request"`
	Response  *response `json:"response"` // nil when the request is aborted
	MinDelayS float64   `json:"min_delay_s"`
}

type request struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Credential string            `json:"credential"` // file in testdata/secrets/clients, or "invalid"
	Headers    map[string]string `json:"headers"`
	Body       *bodyRef          `json:"body"`
	Fault      string            `json:"fault"` // "" or "abort_body"
}

type bodyRef struct {
	Source   string `json:"source"` // relative to testdata/
	FlipByte *int   `json:"flip_byte"`
	SHA256   string `json:"sha256"`
	Size     int    `json:"size"`
}

type response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`  // null = not checked / empty
	Fault   string            `json:"fault"` // "" or "drop" (exporter side: no response is sent)
}

func (s exchangeStep) on(side string) bool {
	if len(s.Sides) == 0 {
		return true
	}
	for _, v := range s.Sides {
		if v == side {
			return true
		}
	}
	return false
}

func (c exchangeCase) stepsOn(side string) []exchangeStep {
	var out []exchangeStep
	for _, s := range c.Steps {
		if s.on(side) {
			out = append(out, s)
		}
	}
	return out
}

func (r *response) hasBody() bool {
	return r != nil && len(r.Body) > 0 && string(r.Body) != "null"
}

// loadCases reads every golden exchange, sorted by file name. The service side
// replays them in this order.
func loadCases(t *testing.T) []exchangeCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testdata, "ingest", "exchanges", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden exchanges found: %v", err)
	}
	sort.Strings(files)
	var cases []exchangeCase
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var c exchangeCase
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		c.file = strings.TrimSuffix(filepath.Base(f), ".json")
		cases = append(cases, c)
	}
	return cases
}

// readBody returns the bytes a golden request sends and checks them against
// the recorded hash and size.
func readBody(t *testing.T, b *bodyRef) []byte {
	t.Helper()
	if b == nil {
		return nil
	}
	if filepath.IsAbs(b.Source) || strings.Contains(b.Source, "..") {
		t.Fatalf("body source %q must be relative to testdata/", b.Source)
	}
	data, err := os.ReadFile(filepath.Join(testdata, filepath.FromSlash(b.Source)))
	if err != nil {
		t.Fatal(err)
	}
	if b.FlipByte != nil {
		data[*b.FlipByte] ^= 0xFF
	}
	if got := sha256hex(data); got != b.SHA256 || len(data) != b.Size {
		t.Fatalf("golden body %s: sha256 %s size %d, recorded %s size %d", b.Source, got, len(data), b.SHA256, b.Size)
	}
	return data
}

// token returns the bearer credential named by a golden request.
func token(t *testing.T, credential string) string {
	t.Helper()
	if credential == "invalid" {
		return "FIXTURE-NOT-A-SECRET-invalid-00000000000000000000000000000000"
	}
	if strings.ContainsAny(credential, `/\`) {
		t.Fatalf("bad credential name %q", credential)
	}
	v, err := os.ReadFile(filepath.Join(testdata, "secrets", "clients", credential))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(v), "\n")
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Placeholders in golden response bodies stand for values the service chooses:
// "{{uuid:NAME}}" (a UUIDv7) and "{{time:NAME}}" (an RFC 3339 UTC timestamp).
// The same NAME must have the same value wherever it appears.
var (
	placeholderRE = regexp.MustCompile(`^\{\{(uuid|time):([A-Za-z0-9_-]+)\}\}$`)
	uuidV7RE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// bindings records the values seen for each placeholder name.
type bindings map[string]string

// match checks got against want. Objects match when every key of want is
// present with a matching value (extra keys are allowed); arrays match
// regardless of order; strings may be placeholders.
func (bd bindings) match(where string, want, got any) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: got %v, want an object", where, got)
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s.%s: missing", where, k)
			}
			if err := bd.match(where+"."+k, wv, gv); err != nil {
				return err
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: got %v, want an array", where, got)
		}
		if !reflect.DeepEqual(sortedJSON(w), sortedJSON(g)) {
			return fmt.Errorf("%s: got %v, want %v (any order)", where, g, w)
		}
		return nil
	case string:
		m := placeholderRE.FindStringSubmatch(w)
		if m == nil {
			if got != w {
				return fmt.Errorf("%s: got %v, want %q", where, got, w)
			}
			return nil
		}
		g, ok := got.(string)
		if !ok {
			return fmt.Errorf("%s: got %v, want a %s", where, got, m[1])
		}
		if err := checkKind(m[1], g); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
		key := m[1] + ":" + m[2]
		if prev, seen := bd[key]; seen && prev != g {
			return fmt.Errorf("%s: got %q, want the same value as before (%q)", where, g, prev)
		}
		bd[key] = g
		return nil
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Errorf("%s: got %v, want %v", where, got, want)
		}
		return nil
	}
}

func checkKind(kind, v string) error {
	switch kind {
	case "uuid":
		if !uuidV7RE.MatchString(v) {
			return fmt.Errorf("%q is not a lowercase UUIDv7", v)
		}
	case "time":
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return err
		}
		if _, off := ts.Zone(); off != 0 {
			return fmt.Errorf("%q is not UTC", v)
		}
	}
	return nil
}

func sortedJSON(a []any) []string {
	out := make([]string, len(a))
	for i, v := range a {
		b, _ := json.Marshal(v)
		out[i] = string(b)
	}
	sort.Strings(out)
	return out
}

// checkHeaders compares the headers a golden step lists. Content-Type is
// compared by media type, so parameters such as charset are ignored.
func checkHeaders(want map[string]string, get func(string) string) error {
	for k, w := range want {
		g := get(k)
		if strings.EqualFold(k, "Content-Type") {
			mt, _, err := mime.ParseMediaType(g)
			if err != nil || mt != w {
				return fmt.Errorf("header %s: got %q, want media type %q", k, g, w)
			}
			continue
		}
		if g != w {
			return fmt.Errorf("header %s: got %q, want %q", k, g, w)
		}
	}
	return nil
}

// mapStrings applies fn to every string in a decoded JSON value, in place.
func mapStrings(v any, fn func(string) string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = mapStrings(e, fn)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = mapStrings(e, fn)
		}
		return x
	case string:
		return fn(x)
	}
	return v
}
