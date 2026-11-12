package manifest

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// jcsVector: in input and want, "%u" stands for a JSON \u escape. The rfc-*
// vectors are from RFC 8785 §3.2.2 (without its numbers array) and §3.2.3,
// the same ones contracts/package uses (CT-PKG-07).
type jcsVector struct{ name, input, want string }

func unescapeVector(s string) []byte { return []byte(strings.ReplaceAll(s, "%u", `\u`)) }

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
	{"minus-zero", `[-0]`, `[0]`},
	{"empty", ` { } `, `{}`},
}

func TestCanonicalizeVectors(t *testing.T) {
	for _, vec := range jcsVectors {
		t.Run(vec.name, func(t *testing.T) {
			got, err := Canonicalize(unescapeVector(vec.input))
			if err != nil {
				t.Fatal(err)
			}
			if want := unescapeVector(vec.want); !bytes.Equal(got, want) {
				t.Fatalf("got  %q\nwant %q", got, want)
			}
		})
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	golden, err := os.ReadFile("../../testdata/meetings/F-01/expected/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, float := range []string{"350.5", "35E1", "350.0"} {
		in := bytes.Replace(golden, []byte(`"size":350}`), []byte(`"size":`+float+`}`), 1)
		if bytes.Equal(in, golden) {
			t.Fatal("fixture: no \"size\":350 in F-01")
		}
		if out, err := Canonicalize(in); err == nil {
			t.Errorf("size %s: accepted as %q", float, out)
		}
	}
	for name, in := range map[string]string{
		"duplicate key":   `{"a":1,"a":2}`,
		"too large":       `[9007199254740992]`,
		"too small":       `[-9007199254740992]`,
		"trailing data":   `{} {}`,
		"trailing junk":   `{}x`,
		"empty":           ``,
		"broken":          `{"a":`,
		"invalid UTF-8":   "[\"\xff\"]",
		"leading zero":    `[01]`,
		"unquoted string": `[abc]`,
	} {
		if out, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: accepted as %q", name, out)
		}
	}
}

func TestCanonicalizeGolden(t *testing.T) {
	golden, err := os.ReadFile("../../testdata/meetings/F-01/expected/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Canonicalize(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, golden) {
		t.Fatal("JCS of the golden F-01 manifest differs from its bytes")
	}
}
