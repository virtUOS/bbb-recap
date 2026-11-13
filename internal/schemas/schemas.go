// Package schemas embeds the JSON Schemas of docs/reference/schemas/ and validates
// documents against them (JSON Schema draft 2020-12, formats asserted).
//
// Embedding cannot reach docs/, so the *.schema.json files here are byte copies.
// TestNoDrift fails when a copy differs from docs/reference/schemas/.
package schemas

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.schema.json
var files embed.FS

// FS returns the embedded schema files.
func FS() fs.FS { return files }

// Schema names: the file name without ".schema.json".
const (
	Manifest     = "manifest"
	Registration = "registration"
	Transcript   = "transcript"
)

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// compileAll compiles every embedded schema once.
func compileAll() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		names, err := fs.Glob(files, "*.schema.json")
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseRegexpEngine(ecmaToGo)
		// Each schema is added and compiled under its "$id" (a URN), so
		// validation errors name the schema and nothing about the host. A bare
		// file name would be resolved against the working directory. A reference
		// from one schema to another must use the other schema's "$id".
		ids := map[string]string{} // schema name -> $id
		for _, n := range names {
			b, err := files.ReadFile(n)
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				compileErr = fmt.Errorf("schema %s: %w", n, err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == "" {
				compileErr = fmt.Errorf("schema %s: no $id", n)
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				compileErr = fmt.Errorf("schema %s: %w", n, err)
				return
			}
			ids[strings.TrimSuffix(n, ".schema.json")] = id
		}
		m := map[string]*jsonschema.Schema{}
		for name, id := range ids {
			sch, err := c.Compile(id)
			if err != nil {
				compileErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			m[name] = sch
		}
		compiled = m
	})
	return compiled, compileErr
}

// Validate checks the JSON document data against the named schema. It
// returns nil if data is valid. The error text may quote values from data, so
// callers log it at DEBUG only.
func Validate(name string, data []byte) error {
	all, err := compileAll()
	if err != nil {
		return err
	}
	sch, ok := all[name]
	if !ok {
		return fmt.Errorf("schemas: unknown schema %q", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return sch.Validate(doc)
}

// ecmaToGo compiles a schema pattern with Go's regexp package. Schema
// patterns are ECMA-262 regular expressions; the only ECMA syntax our schemas
// use that Go lacks is \uXXXX, which is rewritten to \x{XXXX}.
func ecmaToGo(pattern string) (jsonschema.Regexp, error) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '\\' || i+1 >= len(pattern) {
			b.WriteByte(c)
			continue
		}
		if pattern[i+1] == 'u' && i+6 <= len(pattern) && isHex(pattern[i+2:i+6]) {
			b.WriteString(`\x{` + pattern[i+2:i+6] + `}`)
			i += 5
			continue
		}
		b.WriteByte(c) // keep any other escape pair as is
		b.WriteByte(pattern[i+1])
		i++
	}
	return regexp.Compile(b.String())
}

func isHex(s string) bool {
	return strings.Trim(s, "0123456789abcdefABCDEF") == ""
}
