// Package jcs writes the RFC 8785 (JSON Canonicalization Scheme) form of the
// JSON subset the meeting package manifest and the normalized meeting use
// (contracts/README.md).
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxSafeInteger is 2^53-1, the largest integer RFC 8785 (via IEEE 754
// doubles) represents exactly.
const maxSafeInteger = 1<<53 - 1

// Canonicalize returns the RFC 8785 (JCS) canonical form of one JSON value.
// Only the subset the manifest and the normalized meeting use is supported
// (contracts/README.md): null, booleans, integers in ±(2^53-1), strings,
// arrays and objects. A number with a fraction or exponent is rejected, not
// formatted. Duplicate object keys, invalid UTF-8 and data after the value
// are rejected too.
func Canonicalize(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("jcs: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var b bytes.Buffer
	if err := writeValue(&b, dec); err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("jcs: data after the JSON value")
	}
	return b.Bytes(), nil
}

// writeValue reads the next value from dec and writes its canonical form.
func writeValue(b *bytes.Buffer, dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case json.Number:
		return writeInteger(b, v)
	case string:
		writeString(b, v)
	case json.Delim:
		if v == '[' {
			return writeArray(b, dec)
		}
		return writeObject(b, dec)
	}
	return nil
}

func writeArray(b *bytes.Buffer, dec *json.Decoder) error {
	b.WriteByte('[')
	for i := 0; dec.More(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeValue(b, dec); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	_, err := dec.Token() // ']'
	return err
}

func writeObject(b *bytes.Buffer, dec *json.Decoder) error {
	members := map[string][]byte{}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string) // the decoder only yields string keys here
		if _, dup := members[key]; dup {
			return fmt.Errorf("duplicate object key %q", key)
		}
		var vb bytes.Buffer
		if err := writeValue(&vb, dec); err != nil {
			return err
		}
		members[key] = vb.Bytes()
		keys = append(keys, key)
	}
	if _, err := dec.Token(); err != nil { // '}'
		return err
	}
	// RFC 8785 §3.2.3: sort keys by their UTF-16 code units.
	sort.Slice(keys, func(i, j int) bool {
		return slices.Compare(utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))) < 0
	})
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeString(b, k)
		b.WriteByte(':')
		b.Write(members[k])
	}
	b.WriteByte('}')
	return nil
}

// writeInteger writes an integer literal. Floats are rejected.
func writeInteger(b *bytes.Buffer, n json.Number) error {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return fmt.Errorf("number %s: floats are not supported", s)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i > maxSafeInteger || i < -maxSafeInteger {
		return fmt.Errorf("number %s: outside ±(2^53-1)", s)
	}
	b.WriteString(strconv.FormatInt(i, 10)) // also turns -0 into 0
	return nil
}

// writeString follows RFC 8785 §3.2.2.2: escape only the quote, the backslash
// and U+0000..U+001F (with the short forms where JSON has them); write
// everything else as UTF-8.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
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
