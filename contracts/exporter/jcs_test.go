package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// canonicalJSON encodes a decoded JSON value per RFC 8785 (JCS) for the value
// kinds a manifest holds: objects, arrays, strings, integers, booleans, null
// (meeting-package.md §4, README "Identifiers"). Non-integer numbers are an
// error: the manifest has none.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// decodeJSON decodes with json.Number so integers keep their exact digits.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") || (strings.HasPrefix(s, "-") && s == "-0") {
			return fmt.Errorf("non-integer number %s", s)
		}
		b.WriteString(s)
	case string:
		writeCanonicalString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
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
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

// writeCanonicalString follows RFC 8785 §3.2.2.2 (ECMAScript JSON.stringify).
func writeCanonicalString(b *bytes.Buffer, s string) {
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

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// checkCanonicalHelper checks canonicalJSON against the golden F-01 manifest,
// which tools/ref/jcs.py wrote in canonical form, and against key ordering
// where UTF-16 and UTF-8 order differ.
func checkCanonicalHelper(t *testing.T) {
	t.Helper()
	golden := readFile(t, testdataDir(t)+"/meetings/F-01/expected/manifest.json")
	v, err := decodeJSON(golden)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, golden) {
		t.Fatalf("canonical form differs from the golden manifest:\n got %s\nwant %s", got, golden)
	}
	got, err = canonicalJSON(map[string]any{"\uff61": "a\u0001\"", "\U0001F600": true, "b": nil, "a": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	// U+1F600 (surrogate 0xD83D) sorts before U+FF61 in UTF-16 order, after it in UTF-8 order.
	want := `{"a":[],"b":null,"` + "\U0001F600" + `":true,"` + "\uff61" + `":"a\u0001\""}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
