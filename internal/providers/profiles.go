// Package providers holds the provider-agnostic side of docs/reference/providers.md:
// the provider profiles of §4.1 (RECAP_PROVIDERS_FILE), the ASR adapter
// interface of §1 and the LLM adapter interface of §3. Adapters for concrete
// dialects live in internal/adapters and implement the interfaces here; this
// package imports none of them.
package providers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Profile kinds.
const (
	KindASR = "asr"
	KindLLM = "llm"
)

// Dialects lists the known dialects per kind (providers.md §2, §3). A
// profile with any other dialect stops the start.
var Dialects = map[string][]string{
	KindASR: {"whisperx-family", "transkribierbar"},
	KindLLM: {"openai-chat"},
}

// Structured-output modes of LLM profiles (providers.md §3.3).
var structuredOutputModes = []string{"json_schema", "json_object", "prompt"}

var (
	nameRE          = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`) // README "Grammars"
	schemaVersionRE = regexp.MustCompile(`^1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// Keys of the providers file. Every key is listed here; any other key is an
// error (providers.md §4.1, contracts/providers/README.md "Start").
var (
	topKeys         = []string{"schema_version", "defaults", "profiles"}
	defaultsKeys    = []string{KindASR, KindLLM}
	commonKeys      = []string{"name", "kind", "dialect", "base_url", "model", "credentials_file", "max_inflight", "declaration"}
	asrKeys         = []string{"max_file_bytes"}
	llmKeys         = []string{"context_tokens", "max_output_tokens", "structured_output", "extra_body"}
	declarationKeys = []string{"data_categories", "operator", "location", "retention", "deletion",
		"used_for_training", "contract_ref", "reviewed_by", "reviewed_at"}
)

// Declaration is the privacy declaration of a profile (privacy.md §7).
type Declaration struct {
	DataCategories  []string
	Operator        string
	Location        string // on_prem, eu, eea, third_country:<code>
	Retention       string
	Deletion        string // adapter_cleanup, ttl:<duration>, none, manual
	UsedForTraining string // no, yes, unknown
	ContractRef     string
	ReviewedBy      string
	ReviewedAt      string
}

// Profile is one validated provider profile.
type Profile struct {
	Name            string
	Kind            string // KindASR or KindLLM
	Dialect         string
	BaseURL         string
	Model           string
	CredentialsFile string       // empty: no credentials
	MaxInflight     int          // default 1
	Declaration     *Declaration // nil: none given (privacy.md §7)

	// ASR only. Nil: the dialect's capability applies.
	MaxFileBytes *int64

	// LLM only.
	ContextTokens    int
	MaxOutputTokens  int
	StructuredOutput string // default json_object
	ExtraBody        map[string]any

	token string // contents of CredentialsFile; never logged
}

// Token returns the credential read from CredentialsFile, or "" without one.
func (p *Profile) Token() string { return p.token }

// Set is a loaded providers file.
type Set struct {
	SchemaVersion string
	DefaultASR    string
	DefaultLLM    string
	Profiles      []*Profile
}

// Get returns the profile of the given kind named name, or the kind's
// default when name is empty.
func (s *Set) Get(kind, name string) (*Profile, error) {
	if name == "" {
		name = s.defaultName(kind)
	}
	for _, p := range s.Profiles {
		if p.Name == name {
			if p.Kind != kind {
				return nil, fmt.Errorf("profile %q is not an %s profile", name, kind)
			}
			return p, nil
		}
	}
	return nil, fmt.Errorf("no %s profile named %q", kind, name)
}

func (s *Set) defaultName(kind string) string {
	if kind == KindLLM {
		return s.DefaultLLM
	}
	return s.DefaultASR
}

// LogWarnings logs a WARN for each profile without a declaration and for
// each profile that declares a third-country location or training on the
// data (privacy.md §7).
func (s *Set) LogWarnings(log *slog.Logger) {
	for _, p := range s.Profiles {
		if p.Declaration == nil {
			log.Warn("provider profile has no declaration (privacy.md §7)", "profile", p.Name, "kind", p.Kind)
			continue
		}
		if strings.HasPrefix(p.Declaration.Location, "third_country:") {
			log.Warn("provider profile declares a third-country location", "profile", p.Name,
				"location", p.Declaration.Location)
		}
		if p.Declaration.UsedForTraining == "yes" {
			log.Warn("provider profile declares that data is used for training", "profile", p.Name)
		}
	}
}

// Load reads and validates the providers file at path. See Parse.
func Load(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file %s", path)
	}
	return Parse(b)
}

// Parse validates a providers file in one pass (providers.md §4.1). On any
// problem it returns all of them together (errors.Join), each naming the
// key, as "<path of the key>: <problem>". credentials_file is read here.
func Parse(data []byte) (*Set, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty file")
		}
		return nil, err
	}
	c := &checker{}
	s := c.set(doc.Content[0])
	if len(c.errs) > 0 {
		return nil, errors.Join(c.errs...)
	}
	return s, nil
}

// checker collects the errors of one Parse call.
type checker struct{ errs []error }

func (c *checker) errorf(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf(format, args...))
}

// join builds the path of a key: "profiles[0].declaration.location".
func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

// fields returns the values of a mapping by key. Keys not in allowed are
// reported as unknown.
func (c *checker) fields(at string, n *yaml.Node, allowed []string) map[string]*yaml.Node {
	if n.Kind != yaml.MappingNode {
		name := at
		if name == "" {
			name = "file"
		}
		c.errorf("%s: want a mapping", name)
		return nil
	}
	m := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if !slices.Contains(allowed, key) {
			c.errorf("%s: unknown key", join(at, key))
			continue
		}
		m[key] = n.Content[i+1]
	}
	return m
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

// str returns a required, non-empty scalar.
func (c *checker) str(at string, m map[string]*yaml.Node, key string) string {
	n, ok := m[key]
	switch {
	case !ok:
		c.errorf("%s: required", join(at, key))
	case n.Kind != yaml.ScalarNode || isNull(n):
		c.errorf("%s: want a string", join(at, key))
	case n.Value == "":
		c.errorf("%s: must not be empty", join(at, key))
	default:
		return n.Value
	}
	return ""
}

// optionalStr returns a scalar or "" when the key is absent.
func (c *checker) optionalStr(at string, m map[string]*yaml.Node, key string) string {
	if _, ok := m[key]; !ok {
		return ""
	}
	return c.str(at, m, key)
}

// positive returns an integer ≥ 1. present is false when the key is absent
// or null.
func (c *checker) positive(at string, m map[string]*yaml.Node, key string) (v int64, present bool) {
	n, ok := m[key]
	if !ok || isNull(n) {
		return 0, false
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!int" || n.Decode(&v) != nil || v < 1 {
		c.errorf("%s: want a positive integer", join(at, key))
		return 0, false
	}
	return v, true
}

func (c *checker) set(root *yaml.Node) *Set {
	m := c.fields("", root, topKeys)
	if m == nil {
		return nil
	}
	s := &Set{SchemaVersion: c.str("", m, "schema_version")}
	if s.SchemaVersion != "" && !schemaVersionRE.MatchString(s.SchemaVersion) {
		c.errorf("schema_version: want 1.x.y, got %q", s.SchemaVersion)
	}

	if n, ok := m["profiles"]; !ok {
		c.errorf("profiles: required")
	} else if n.Kind != yaml.SequenceNode {
		c.errorf("profiles: want a list")
	} else {
		seen := map[string]bool{}
		for i, pn := range n.Content {
			at := fmt.Sprintf("profiles[%d]", i)
			p := c.profile(at, pn)
			if p == nil || p.Name == "" {
				continue
			}
			if seen[p.Name] {
				c.errorf("%s.name: duplicate %q", at, p.Name)
				continue
			}
			seen[p.Name] = true
			s.Profiles = append(s.Profiles, p)
		}
	}

	if n, ok := m["defaults"]; !ok {
		c.errorf("defaults: required")
	} else if d := c.fields("defaults", n, defaultsKeys); d != nil {
		s.DefaultASR = c.str("defaults", d, KindASR)
		s.DefaultLLM = c.str("defaults", d, KindLLM)
		for _, kind := range defaultsKeys {
			if name := s.defaultName(kind); name != "" {
				if _, err := s.Get(kind, name); err != nil {
					c.errorf("defaults.%s: %v", kind, err)
				}
			}
		}
	}
	return s
}

func (c *checker) profile(at string, n *yaml.Node) *Profile {
	// The allowed keys depend on the kind; read it first.
	kind := ""
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "kind" {
				kind = n.Content[i+1].Value
			}
		}
	}
	allowed := slices.Concat(commonKeys, asrKeys, llmKeys) // unknown kind: report the kind only
	switch kind {
	case KindASR:
		allowed = slices.Concat(commonKeys, asrKeys)
	case KindLLM:
		allowed = slices.Concat(commonKeys, llmKeys)
	}
	m := c.fields(at, n, allowed)
	if m == nil {
		return nil
	}

	p := &Profile{
		Name:    c.str(at, m, "name"),
		Kind:    c.str(at, m, "kind"),
		Dialect: c.str(at, m, "dialect"),
		BaseURL: c.str(at, m, "base_url"),
		Model:   c.str(at, m, "model"),
	}
	if p.Name != "" && !nameRE.MatchString(p.Name) {
		c.errorf("%s.name: %q must match %s", at, p.Name, nameRE)
	}
	if p.Kind != "" && p.Kind != KindASR && p.Kind != KindLLM {
		c.errorf("%s.kind: want asr or llm, got %q", at, p.Kind)
	}
	if p.Dialect != "" && (p.Kind == KindASR || p.Kind == KindLLM) && !slices.Contains(Dialects[p.Kind], p.Dialect) {
		c.errorf("%s.dialect: unknown %s dialect %q (known: %s)", at, p.Kind, p.Dialect,
			strings.Join(Dialects[p.Kind], ", "))
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			c.errorf("%s.base_url: want an http:// or https:// URL without credentials", at)
		}
	}

	p.CredentialsFile = c.optionalStr(at, m, "credentials_file")
	if p.CredentialsFile != "" {
		b, err := os.ReadFile(p.CredentialsFile)
		if err != nil {
			c.errorf("%s.credentials_file: cannot read file %s", at, p.CredentialsFile)
		} else if p.token = strings.TrimSuffix(string(b), "\n"); p.token == "" {
			c.errorf("%s.credentials_file: file %s is empty", at, p.CredentialsFile)
		}
	}

	p.MaxInflight = 1
	if v, ok := c.positive(at, m, "max_inflight"); ok {
		p.MaxInflight = int(v)
	}

	switch p.Kind {
	case KindASR:
		if v, ok := c.positive(at, m, "max_file_bytes"); ok {
			p.MaxFileBytes = &v
		}
	case KindLLM:
		ctxTokens, ok1 := c.positive(at, m, "context_tokens")
		outTokens, ok2 := c.positive(at, m, "max_output_tokens")
		if _, present := m["context_tokens"]; !present {
			c.errorf("%s.context_tokens: required", at)
		}
		if _, present := m["max_output_tokens"]; !present {
			c.errorf("%s.max_output_tokens: required", at)
		}
		if ok1 && ok2 && outTokens >= ctxTokens {
			c.errorf("%s.max_output_tokens: must be below context_tokens", at)
		}
		p.ContextTokens, p.MaxOutputTokens = int(ctxTokens), int(outTokens)
		p.StructuredOutput = "json_object"
		if v := c.optionalStr(at, m, "structured_output"); v != "" {
			if !slices.Contains(structuredOutputModes, v) {
				c.errorf("%s.structured_output: want one of %s, got %q", at,
					strings.Join(structuredOutputModes, ", "), v)
			}
			p.StructuredOutput = v
		}
		if n, ok := m["extra_body"]; ok {
			if n.Kind != yaml.MappingNode || n.Decode(&p.ExtraBody) != nil {
				c.errorf("%s.extra_body: want a mapping", at)
			}
		}
	}

	if n, ok := m["declaration"]; ok {
		p.Declaration = c.declaration(at+".declaration", n)
	}
	return p
}

// declaration checks a declaration that is present: it must be complete
// (privacy.md §7).
func (c *checker) declaration(at string, n *yaml.Node) *Declaration {
	m := c.fields(at, n, declarationKeys)
	if m == nil {
		return nil
	}
	d := &Declaration{
		Operator:        c.str(at, m, "operator"),
		Location:        c.str(at, m, "location"),
		Retention:       c.str(at, m, "retention"),
		Deletion:        c.str(at, m, "deletion"),
		UsedForTraining: c.str(at, m, "used_for_training"),
		ContractRef:     c.str(at, m, "contract_ref"),
		ReviewedBy:      c.str(at, m, "reviewed_by"),
		ReviewedAt:      c.str(at, m, "reviewed_at"),
	}
	if n, ok := m["data_categories"]; !ok {
		c.errorf("%s.data_categories: required", at)
	} else if n.Kind != yaml.SequenceNode || len(n.Content) == 0 || n.Decode(&d.DataCategories) != nil ||
		slices.Contains(d.DataCategories, "") {
		c.errorf("%s.data_categories: want a non-empty list of strings", at)
	}
	switch loc := d.Location; {
	case loc == "", loc == "on_prem", loc == "eu", loc == "eea":
	case strings.HasPrefix(loc, "third_country:") && len(loc) > len("third_country:"):
	default:
		c.errorf("%s.location: want on_prem, eu, eea or third_country:<code>, got %q", at, loc)
	}
	switch del := d.Deletion; {
	case del == "", del == "adapter_cleanup", del == "none", del == "manual":
	case strings.HasPrefix(del, "ttl:") && len(del) > len("ttl:"):
	default:
		c.errorf("%s.deletion: want adapter_cleanup, ttl:<duration>, none or manual, got %q", at, del)
	}
	if v := d.UsedForTraining; v != "" && v != "no" && v != "yes" && v != "unknown" {
		c.errorf("%s.used_for_training: want no, yes or unknown, got %q", at, v)
	}
	if v := d.ReviewedAt; v != "" {
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			c.errorf("%s.reviewed_at: want a date YYYY-MM-DD, got %q", at, v)
		}
	}
	return d
}
