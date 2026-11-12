// Package format loads an output format from RECAP_FORMATS_DIR
// (docs/reference/templates.md §1, §2; ADR-012). A format is a directory of data
// files named after its ID:
//
//	<RECAP_FORMATS_DIR>/<format_id>/
//	  format.json              {"id": "<format_id>", "version": "<semver>"}
//	  <name>.schema.json       JSON Schemas of LLM replies, e.g. summary.v1 (templates.md §4)
//	  prompts/<call>.md        system prompt of one LLM call, English (templates.md §4)
//	  strings/<lang>.json      string table of one output language (templates.md §7):
//	                           a JSON object of strings, "language_name" is the
//	                           English name of the language
//
// Other files (render pieces) are loaded too and count in the SHA-256.
//
// The format's SHA-256 (templates.md §1, "the SHA-256 of the format files")
// is computed over every regular file below the directory, sorted by
// slash-separated relative path: for each file its path, a NUL byte, its
// size in decimal, a NUL byte and its bytes.
package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

// ErrNotFound means the format directory does not exist.
var ErrNotFound = errors.New("format: not found")

var (
	idRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`) // README "Grammars"
	versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	langRE    = regexp.MustCompile(`^[a-z]{2}$`)
)

const schemaSuffix = ".schema.json"

// Format is a loaded format.
type Format struct {
	ID      string
	Version string
	SHA256  string

	prompts map[string]string
	schemas map[string]json.RawMessage
	strings map[string]map[string]string
	files   map[string][]byte

	compiled map[string]*jsonschema.Schema
}

// Load reads the format id from dir. A missing directory is ErrNotFound;
// any other problem (bad metadata, a schema that does not compile, a string
// table that is not an object of strings) is an error naming the file.
func Load(dir, id string) (*Format, error) {
	if !idRE.MatchString(id) {
		return nil, ErrNotFound
	}
	root := filepath.Join(dir, id)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, ErrNotFound
	}
	return load(os.DirFS(root), id)
}

func load(fsys fs.FS, id string) (*Format, error) {
	f := &Format{
		prompts:  map[string]string{},
		schemas:  map[string]json.RawMessage{},
		strings:  map[string]map[string]string{},
		files:    map[string][]byte{},
		compiled: map[string]*jsonschema.Schema{},
	}
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			f.files[p] = b
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", id, err)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n + "\x00" + strconv.Itoa(len(f.files[n])) + "\x00"))
		h.Write(f.files[n])
	}
	f.SHA256 = hex.EncodeToString(h.Sum(nil))

	var meta struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(f.files["format.json"], &meta); err != nil {
		return nil, fmt.Errorf("format %s: format.json: %v", id, err)
	}
	if meta.ID != id || !versionRE.MatchString(meta.Version) {
		return nil, fmt.Errorf("format %s: format.json: want id %q and a semver version", id, id)
	}
	f.ID, f.Version = meta.ID, meta.Version

	for _, n := range names {
		dir, base := path.Split(n)
		switch {
		case dir == "" && strings.HasSuffix(base, schemaSuffix):
			sch, err := providers.CompileSchema(f.files[n])
			if err != nil {
				return nil, fmt.Errorf("format %s: %s: %v", id, n, err)
			}
			name := strings.TrimSuffix(base, schemaSuffix)
			f.schemas[name] = f.files[n]
			f.compiled[name] = sch
		case dir == "prompts/" && strings.HasSuffix(base, ".md"):
			f.prompts[strings.TrimSuffix(base, ".md")] = string(f.files[n])
		case dir == "strings/" && strings.HasSuffix(base, ".json"):
			lang := strings.TrimSuffix(base, ".json")
			var table map[string]string
			if !langRE.MatchString(lang) || json.Unmarshal(f.files[n], &table) != nil || table == nil {
				return nil, fmt.Errorf("format %s: %s: want strings/<ISO 639-1>.json with an object of strings", id, n)
			}
			f.strings[lang] = table
		}
	}
	return f, nil
}

// Prompt returns the system prompt of an LLM call (prompts/<call>.md).
func (f *Format) Prompt(call string) (string, bool) {
	p, ok := f.prompts[call]
	return p, ok
}

// Schema returns the JSON Schema <name>.schema.json, e.g. "summary.v1".
func (f *Format) Schema(name string) (json.RawMessage, bool) {
	s, ok := f.schemas[name]
	return s, ok
}

// Validate checks a JSON document against the schema name. The error text
// may quote the document; log it at DEBUG at most.
func (f *Format) Validate(name string, data []byte) error {
	sch, ok := f.compiled[name]
	if !ok {
		return fmt.Errorf("format %s: no schema %q", f.ID, name)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

// Strings returns the string table of lang, or nil without one.
func (f *Format) Strings(lang string) map[string]string { return f.strings[lang] }

// File returns a file of the format by its slash-separated relative path.
func (f *Format) File(name string) ([]byte, bool) {
	b, ok := f.files[name]
	return b, ok
}
