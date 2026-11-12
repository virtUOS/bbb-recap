package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/virtUOS/bbb-recap/internal/providers"
)

var semverMajor1RE = regexp.MustCompile(`^1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// MeetingType maps a meeting type (value of meta_recap-type) to a format
// (templates.md §1).
type MeetingType struct {
	Name   string `yaml:"name"`
	Format string `yaml:"format"`
}

type meetingTypesFile struct {
	SchemaVersion string        `yaml:"schema_version"`
	MeetingTypes  []MeetingType `yaml:"meeting_types"`
}

// Declaration is the privacy declaration of a provider or sink profile
// (privacy.md §7).
type Declaration struct {
	DataCategories  []string `yaml:"data_categories"`
	Operator        string   `yaml:"operator"`
	Location        string   `yaml:"location"`
	Retention       string   `yaml:"retention"`
	Deletion        string   `yaml:"deletion"`
	UsedForTraining string   `yaml:"used_for_training"`
	ContractRef     string   `yaml:"contract_ref"`
	ReviewedBy      string   `yaml:"reviewed_by"`
	ReviewedAt      string   `yaml:"reviewed_at"`
}

// SinkProfile is one entry of RECAP_SINKS_FILE (sinks.md §3.1), checked
// by loadSinks. Optional settings get their defaults there.
type SinkProfile struct {
	Name                string       `yaml:"name"`
	Type                string       `yaml:"type"`
	DocsBaseURL         string       `yaml:"docs_base_url"`
	APIKeyFile          string       `yaml:"api_key_file"`
	DocumentURLTemplate string       `yaml:"document_url_template"`
	RequestTimeout      string       `yaml:"request_timeout"`
	MaxInflight         int          `yaml:"max_inflight"`
	MaxDocumentBytes    int64        `yaml:"max_document_bytes"`
	Declaration         *Declaration `yaml:"declaration"`

	// Timeout is RequestTimeout parsed.
	Timeout time.Duration `yaml:"-"`
	// APIKey is the content of APIKeyFile, trimmed of one trailing
	// newline; read for the worker role only. Never logged.
	APIKey string `yaml:"-"`
}

// Sink profile types: the Docs sink (sinks.md §3) and the front-end sink
// (§4a, ADR-014).
const (
	SinkTypeDocs     = "docs"
	SinkTypeFrontend = "frontend"
)

// Defaults of the sink profile settings (sinks.md §2.4, §3.1).
const (
	DefaultSinkRequestTimeout   = 60 * time.Second
	DefaultSinkMaxInflight      = 2
	DefaultSinkMaxDocumentBytes = 2 << 20
)

type sinksFile struct {
	SchemaVersion string        `yaml:"schema_version"`
	Sinks         []SinkProfile `yaml:"sinks"`
}

// readRaw reads the file named by variable name and remembers its bytes for Hash.
func readRaw(l *loader, c *Config, name string) ([]byte, bool) {
	path := l.get(name)
	b, err := os.ReadFile(path)
	if err != nil {
		l.errorf("%s: cannot read file %s", name, path)
		return nil, false
	}
	c.files[name] = b
	return b, true
}

// readYAML reads the file named by variable name (readRaw) and decodes it
// into v. With strict set, unknown keys are errors.
func readYAML(l *loader, c *Config, name string, v any, strict bool) bool {
	b, ok := readRaw(l, c, name)
	if !ok {
		return false
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(strict)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("empty file")
		}
		l.errorf("%s: %s: %v", name, l.get(name), err)
		return false
	}
	return true
}

func checkSchemaVersion(l *loader, name, v string) {
	if !semverMajor1RE.MatchString(v) {
		l.errorf("%s: schema_version: want 1.x.y, got %q", name, v)
	}
}

func loadMeetingTypes(l *loader, c *Config, name string) []MeetingType {
	var f meetingTypesFile
	if !readYAML(l, c, name, &f, true) {
		return nil
	}
	checkSchemaVersion(l, name, f.SchemaVersion)
	seen := map[string]bool{}
	for i, mt := range f.MeetingTypes {
		if !nameRE.MatchString(mt.Name) {
			l.errorf("%s: meeting_types[%d].name: %q must match %s", name, i, mt.Name, nameRE)
		} else if seen[mt.Name] {
			l.errorf("%s: meeting_types[%d].name: duplicate %q", name, i, mt.Name)
		}
		seen[mt.Name] = true
		if !nameRE.MatchString(mt.Format) {
			l.errorf("%s: meeting_types[%d].format: %q must match %s", name, i, mt.Format, nameRE)
		}
	}
	return f.MeetingTypes
}

// loadSinks reads and checks the sinks file (sinks.md §1.5, §3.1, §4a):
// names, type (docs or frontend), for docs docs_base_url, api_key_file (read when readKeys is set, the worker
// role), request_timeout, max_inflight, max_document_bytes and the
// declaration, which is optional but complete when present (privacy.md §7). A relative api_key_file is relative to the
// directory of the sinks file. Each problem names its key.
func loadSinks(l *loader, c *Config, name string, readKeys bool) []SinkProfile {
	var f sinksFile
	if !readYAML(l, c, name, &f, true) {
		return nil
	}
	checkSchemaVersion(l, name, f.SchemaVersion) // sinks.md §3.1: schema_version 1.x.y
	seen := map[string]bool{}
	for i := range f.Sinks {
		s := &f.Sinks[i]
		at := fmt.Sprintf("%s: sinks[%d]", name, i)
		if !nameRE.MatchString(s.Name) {
			l.errorf("%s.name: %q must match %s", at, s.Name, nameRE)
		} else if seen[s.Name] {
			l.errorf("%s.name: duplicate %q", at, s.Name)
		}
		seen[s.Name] = true
		switch s.Type {
		case SinkTypeDocs:
			checkDocsSink(l, at, name, s, readKeys)
		case SinkTypeFrontend:
			// sinks.md §4a: no URL, key or timeout; nothing leaves the service.
			for _, k := range []struct{ key, value string }{
				{"docs_base_url", s.DocsBaseURL}, {"api_key_file", s.APIKeyFile},
				{"document_url_template", s.DocumentURLTemplate}, {"request_timeout", s.RequestTimeout},
			} {
				if k.value != "" {
					l.errorf("%s.%s: not allowed for type frontend", at, k.key)
				}
			}
		default:
			l.errorf("%s.type: want docs or frontend, got %q", at, s.Type)
		}
		switch {
		case s.MaxInflight < 0:
			l.errorf("%s.max_inflight: want a positive integer, got %d", at, s.MaxInflight)
		case s.MaxInflight == 0:
			s.MaxInflight = DefaultSinkMaxInflight
		}
		switch {
		case s.MaxDocumentBytes < 0:
			l.errorf("%s.max_document_bytes: want a positive integer, got %d", at, s.MaxDocumentBytes)
		case s.MaxDocumentBytes == 0:
			s.MaxDocumentBytes = DefaultSinkMaxDocumentBytes
		}
		if s.Declaration != nil {
			checkDeclaration(l, at+".declaration", s.Declaration)
		}
	}
	return f.Sinks
}

// checkDocsSink checks the keys of a docs profile (sinks.md §3.1) and reads
// its key when readKeys is set. It sets Timeout.
func checkDocsSink(l *loader, at, name string, s *SinkProfile, readKeys bool) {
	if u, err := url.Parse(s.DocsBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		l.errorf("%s.docs_base_url: want an http(s) URL without query, fragment or credentials", at)
	}
	if s.DocumentURLTemplate != "" && !strings.Contains(s.DocumentURLTemplate, "{id}") {
		l.errorf("%s.document_url_template: want a URL with {id}", at)
	}
	switch {
	case s.APIKeyFile == "":
		l.errorf("%s.api_key_file: required", at)
	case readKeys:
		path := s.APIKeyFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(l.get(name)), path)
		}
		b, err := os.ReadFile(path)
		s.APIKey = strings.TrimSuffix(string(b), "\n")
		if err != nil {
			l.errorf("%s.api_key_file: cannot read file %s", at, s.APIKeyFile)
		} else if strings.TrimSpace(s.APIKey) == "" {
			l.errorf("%s.api_key_file: file %s is empty", at, s.APIKeyFile)
		}
	}
	s.Timeout = DefaultSinkRequestTimeout
	if s.RequestTimeout != "" {
		d, err := time.ParseDuration(s.RequestTimeout)
		if err != nil || d <= 0 {
			l.errorf("%s.request_timeout: want a positive duration such as 60s, got %q", at, s.RequestTimeout)
		} else {
			s.Timeout = d
		}
	}
}

// checkDeclaration checks that every field of a declaration is set and
// has an allowed value (privacy.md §7).
func checkDeclaration(l *loader, at string, d *Declaration) {
	for _, f := range []struct{ key, value string }{
		{"operator", d.Operator}, {"location", d.Location}, {"retention", d.Retention},
		{"deletion", d.Deletion}, {"used_for_training", d.UsedForTraining}, {"contract_ref", d.ContractRef},
		{"reviewed_by", d.ReviewedBy}, {"reviewed_at", d.ReviewedAt},
	} {
		if strings.TrimSpace(f.value) == "" {
			l.errorf("%s.%s: required", at, f.key)
		}
	}
	if len(d.DataCategories) == 0 || slices.Contains(d.DataCategories, "") {
		l.errorf("%s.data_categories: want a non-empty list of strings", at)
	}
	switch loc := d.Location; {
	case loc == "", loc == "on_prem", loc == "eu", loc == "eea":
	case strings.HasPrefix(loc, "third_country:") && len(loc) > len("third_country:"):
	default:
		l.errorf("%s.location: want on_prem, eu, eea or third_country:<code>, got %q", at, loc)
	}
	switch del := d.Deletion; {
	case del == "", del == "adapter_cleanup", del == "none", del == "manual":
	case strings.HasPrefix(del, "ttl:") && len(del) > len("ttl:"):
	default:
		l.errorf("%s.deletion: want adapter_cleanup, ttl:<duration>, none or manual, got %q", at, del)
	}
	if v := d.UsedForTraining; v != "" && v != "no" && v != "yes" && v != "unknown" {
		l.errorf("%s.used_for_training: want no, yes or unknown, got %q", at, v)
	}
	if v := d.ReviewedAt; v != "" {
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			l.errorf("%s.reviewed_at: want a date YYYY-MM-DD, got %q", at, v)
		}
	}
}

// LoadSinks reads and checks the sinks file at path with the rules of the
// worker role (keys are read). recap tool sink uses it instead of Load.
func LoadSinks(path string) ([]SinkProfile, error) {
	l := &loader{env: map[string]string{"RECAP_SINKS_FILE": path}}
	c := &Config{files: map[string][]byte{}}
	sinks := loadSinks(l, c, "RECAP_SINKS_FILE", true)
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return sinks, nil
}

// LogSinkWarnings logs a WARN for each sink profile whose docs_base_url is
// not https (sinks.md §3.4: TLS in production; http stays allowed for dev
// and tests), that has no declaration, or that declares a third-country
// location or training on the data (privacy.md §7).
func LogSinkWarnings(log *slog.Logger, sinks []SinkProfile) {
	for _, s := range sinks {
		if s.Type == SinkTypeDocs && !strings.HasPrefix(s.DocsBaseURL, "https://") {
			log.Warn("sink profile does not use https", "profile", s.Name)
		}
		if s.Declaration == nil {
			log.Warn("sink profile has no declaration (privacy.md §7)", "profile", s.Name, "kind", s.Type)
			continue
		}
		if strings.HasPrefix(s.Declaration.Location, "third_country:") {
			log.Warn("sink profile declares a third-country location", "profile", s.Name,
				"location", s.Declaration.Location)
		}
		if s.Declaration.UsedForTraining == "yes" {
			log.Warn("sink profile declares that data is used for training", "profile", s.Name)
		}
	}
}

// loadProviders validates the providers file with the rules of
// providers.md §4.1 (internal/providers) and reports each problem as
// "<variable>: <key path>: <problem>".
func loadProviders(l *loader, c *Config, name string) *providers.Set {
	b, ok := readRaw(l, c, name)
	if !ok {
		return nil
	}
	set, err := providers.Parse(b)
	if err == nil {
		return set
	}
	errs := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	}
	for _, e := range errs {
		l.errorf("%s: %v", name, e)
	}
	return nil
}
