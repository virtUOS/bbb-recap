package config

import (
	"bytes"
	"encoding/base64"
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

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

var semverMajor1RE = regexp.MustCompile(`^1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// MeetingType maps a meeting type (value of meta_recap-type) to a format
// (templates.md §1). Inputs switches optional inputs on; a missing name
// means false.
type MeetingType struct {
	Name   string          `yaml:"name"`
	Format string          `yaml:"format"`
	Inputs map[string]bool `yaml:"inputs,omitempty"`
}

// knownInputs are the optional inputs a meeting type can name (templates.md §1).
var knownInputs = []string{format.InputSlides}

// InputErrors checks the inputs of the meeting types (templates.md §1): an
// unknown input name, or one the type's format does not declare in its
// format.json, is an error, for true and false alike. A type whose format is
// missing is skipped; its jobs are held unknown_type. name is the variable
// or file the messages start with.
func InputErrors(name string, types []MeetingType, formatsDir string) []string {
	var errs []string
	for i, mt := range types {
		if len(mt.Inputs) == 0 {
			continue
		}
		declared, err := format.DeclaredInputs(formatsDir, mt.Format)
		if errors.Is(err, format.ErrNotFound) {
			continue // the job is held unknown_type
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: meeting_types[%d].inputs: %v", name, i, err))
			continue
		}
		keys := make([]string, 0, len(mt.Inputs))
		for k := range mt.Inputs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			at := fmt.Sprintf("%s: meeting_types[%d].inputs.%s", name, i, k)
			switch {
			case !slices.Contains(knownInputs, k):
				errs = append(errs, fmt.Sprintf("%s: unknown input (known: %s)", at, strings.Join(knownInputs, ", ")))
			case !slices.Contains(declared, k):
				errs = append(errs, fmt.Sprintf("%s: format %q does not declare this input", at, mt.Format))
			}
		}
	}
	return errs
}

type meetingTypesFile struct {
	SchemaVersion string        `yaml:"schema_version"`
	MeetingTypes  []MeetingType `yaml:"meeting_types"`
}

// Declaration is the privacy declaration of a provider or sink profile
// (privacy.md §7).
type Declaration struct {
	DataCategories []string `yaml:"data_categories"`
	Operator       string   `yaml:"operator"`
	Location       string   `yaml:"location"`
	Retention      string   `yaml:"retention"`
	Deletion       string   `yaml:"deletion"`
}

// SinkProfile is one entry of RECAP_SINKS_FILE (sinks.md §3.1, §4a, §4b),
// checked by loadSinks. Optional settings get their defaults there.
type SinkProfile struct {
	Name                string       `yaml:"name"`
	Type                string       `yaml:"type"`
	DocsBaseURL         string       `yaml:"docs_base_url"`
	APIKeyFile          string       `yaml:"api_key_file"`
	DocumentURLTemplate string       `yaml:"document_url_template"`
	URL                 string       `yaml:"url"`
	SigningSecretFile   string       `yaml:"signing_secret_file"`
	Content             string       `yaml:"content"`
	HomeserverURL       string       `yaml:"homeserver_url"`
	AccessTokenFile     string       `yaml:"access_token_file"`
	RequestTimeout      string       `yaml:"request_timeout"`
	MaxInflight         int          `yaml:"max_inflight"`
	MaxDocumentBytes    int64        `yaml:"max_document_bytes"`
	Declaration         *Declaration `yaml:"declaration"`

	// Timeout is RequestTimeout parsed.
	Timeout time.Duration `yaml:"-"`
	// APIKey is the content of APIKeyFile, trimmed of one trailing
	// newline; read for the worker role only. Never logged.
	APIKey string `yaml:"-"`
	// SigningKeys are the decoded keys of SigningSecretFile, one per line
	// in file order (sinks.md §4b.1); read for the worker role only. Never
	// logged.
	SigningKeys [][]byte `yaml:"-"`
}

// Sink profile types: the Docs sink (sinks.md §3), the front-end sink
// (§4a, ADR-014), the webhook sink (§4b) and the Matrix sink (§4c,
// ADR-016), which is not implemented yet.
const (
	SinkTypeDocs     = "docs"
	SinkTypeFrontend = "frontend"
	SinkTypeWebhook  = "webhook"
	SinkTypeMatrix   = "matrix"
)

// Values of a webhook profile's content (sinks.md §4b.1).
const (
	WebhookContentNotice  = "notice"
	WebhookContentSummary = "summary"
)

// Defaults of the sink profile settings (sinks.md §2.4, §3.1, §4b.1).
const (
	DefaultSinkRequestTimeout    = 60 * time.Second
	DefaultWebhookRequestTimeout = 30 * time.Second
	DefaultSinkMaxInflight       = 2
	DefaultSinkMaxDocumentBytes  = 2 << 20
)

// Bounds of a webhook signing key after base64 decoding (sinks.md §4b.1,
// Standard Webhooks "Signature scheme").
const (
	minSigningKeyBytes = 24
	maxSigningKeyBytes = 64
	maxSigningKeys     = 2
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
	for _, e := range InputErrors(name, f.MeetingTypes, c.FormatsDir) {
		l.errorf("%s", e)
	}
	return f.MeetingTypes
}

// loadSinks reads and checks the sinks file (sinks.md §1.5, §3.1, §4a,
// §4b): names, type (docs, frontend or webhook; matrix is refused as not
// supported yet), the keys of each type, request_timeout, max_inflight,
// max_document_bytes and the declaration, which is optional but complete
// when present (privacy.md §7) and required for a webhook profile (§11). Key
// and secret files are read when readKeys is set (the worker role); a
// relative path is relative to the directory of the sinks file. Each problem
// names its key.
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
			notAllowed(l, at, s, "url", "signing_secret_file", "content")
			checkDocsSink(l, at, name, s, readKeys)
		case SinkTypeFrontend:
			// sinks.md §4a: no URL, key or timeout; nothing leaves the service.
			notAllowed(l, at, s, "docs_base_url", "api_key_file", "document_url_template", "request_timeout",
				"url", "signing_secret_file", "content")
		case SinkTypeWebhook:
			notAllowed(l, at, s, "docs_base_url", "api_key_file", "document_url_template")
			checkWebhookSink(l, at, name, s, readKeys)
		case SinkTypeMatrix:
			l.errorf("%s.type: matrix is not supported yet", at)
		default:
			l.errorf("%s.type: want docs, frontend or webhook, got %q", at, s.Type)
		}
		if s.Type != SinkTypeMatrix {
			notAllowed(l, at, s, "homeserver_url", "access_token_file")
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
		switch {
		case s.Declaration != nil:
			checkDeclaration(l, at+".declaration", s.Declaration)
		case s.Type == SinkTypeWebhook:
			// privacy.md §7, §11: a webhook reaches beyond the owner.
			l.errorf("%s.declaration: required for type %s (privacy.md §7, §11)", at, s.Type)
		}
	}
	return f.Sinks
}

// notAllowed reports each of keys that is set in s although its type has
// no use for it.
func notAllowed(l *loader, at string, s *SinkProfile, keys ...string) {
	values := map[string]string{
		"docs_base_url": s.DocsBaseURL, "api_key_file": s.APIKeyFile, "document_url_template": s.DocumentURLTemplate,
		"request_timeout": s.RequestTimeout, "url": s.URL, "signing_secret_file": s.SigningSecretFile,
		"content": s.Content, "homeserver_url": s.HomeserverURL, "access_token_file": s.AccessTokenFile,
	}
	for _, k := range keys {
		if values[k] != "" {
			l.errorf("%s.%s: not allowed for type %s", at, k, s.Type)
		}
	}
}

// checkWebhookSink checks the keys of a webhook profile (sinks.md §4b.1,
// §4b.4) and reads its signing secrets when readKeys is set. It sets
// Timeout and, when empty, Content to notice.
func checkWebhookSink(l *loader, at, name string, s *SinkProfile, readKeys bool) {
	// The URL is configuration, not a secret, but it is never logged
	// (sinks.md §4b.1), so the problem does not quote it.
	if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.User != nil || u.Fragment != "" {
		l.errorf("%s.url: want an absolute http(s) URL without user info or fragment", at)
	}
	switch s.Content {
	case "":
		s.Content = WebhookContentNotice
	case WebhookContentNotice, WebhookContentSummary:
	default:
		l.errorf("%s.content: want notice or summary, got %q", at, s.Content)
	}
	switch {
	case s.SigningSecretFile == "":
		l.errorf("%s.signing_secret_file: required", at)
	case readKeys:
		b, err := os.ReadFile(resolve(l, name, s.SigningSecretFile))
		if err != nil {
			l.errorf("%s.signing_secret_file: cannot read file %s", at, s.SigningSecretFile)
			break
		}
		keys, problem := parseSigningSecrets(string(b))
		if problem != "" {
			l.errorf("%s.signing_secret_file: file %s: %s", at, s.SigningSecretFile, problem)
		}
		s.SigningKeys = keys
	}
	s.Timeout = requestTimeout(l, at, s.RequestTimeout, DefaultWebhookRequestTimeout)
}

// parseSigningSecrets decodes one or two lines whsec_<base64>, each 24 to
// 64 bytes after decoding (sinks.md §4b.1). Empty lines are ignored. The
// problem never quotes the file.
func parseSigningSecrets(text string) ([][]byte, string) {
	var keys [][]byte
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b64, ok := strings.CutPrefix(line, "whsec_")
		if !ok {
			return nil, fmt.Sprintf("line %d: want whsec_<base64>", len(keys)+1)
		}
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Sprintf("line %d: not valid standard base64 with padding", len(keys)+1)
		}
		if len(key) < minSigningKeyBytes || len(key) > maxSigningKeyBytes {
			return nil, fmt.Sprintf("line %d: want %d to %d bytes after decoding", len(keys)+1,
				minSigningKeyBytes, maxSigningKeyBytes)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 || len(keys) > maxSigningKeys {
		return nil, fmt.Sprintf("want one or two secret lines, got %d", len(keys))
	}
	return keys, ""
}

// resolve returns path, relative to the directory of the file of variable
// name unless it is absolute.
func resolve(l *loader, name, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(l.get(name)), path)
}

// requestTimeout parses a request_timeout; empty gives def.
func requestTimeout(l *loader, at, v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.errorf("%s.request_timeout: want a positive duration such as 60s, got %q", at, v)
		return def
	}
	return d
}

// checkDocsSink checks the keys of a docs profile (sinks.md §3.1) and reads
// its key when readKeys is set. It sets Timeout and, when empty,
// DocumentURLTemplate to <docs_base_url>/docs/{id}/.
func checkDocsSink(l *loader, at, name string, s *SinkProfile, readKeys bool) {
	if u, err := url.Parse(s.DocsBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		l.errorf("%s.docs_base_url: want an http(s) URL without query, fragment or credentials", at)
	}
	switch {
	case s.DocumentURLTemplate == "":
		// sinks.md §3.1: the default is Docs' own document page.
		s.DocumentURLTemplate = strings.TrimRight(s.DocsBaseURL, "/") + "/docs/{id}/"
	case !strings.Contains(s.DocumentURLTemplate, "{id}"):
		l.errorf("%s.document_url_template: want a URL with {id}", at)
	}
	switch {
	case s.APIKeyFile == "":
		l.errorf("%s.api_key_file: required", at)
	case readKeys:
		b, err := os.ReadFile(resolve(l, name, s.APIKeyFile))
		s.APIKey = strings.TrimSuffix(string(b), "\n")
		if err != nil {
			l.errorf("%s.api_key_file: cannot read file %s", at, s.APIKeyFile)
		} else if strings.TrimSpace(s.APIKey) == "" {
			l.errorf("%s.api_key_file: file %s is empty", at, s.APIKeyFile)
		}
	}
	s.Timeout = requestTimeout(l, at, s.RequestTimeout, DefaultSinkRequestTimeout)
}

// checkDeclaration checks that every field of a declaration is set and
// has an allowed value (privacy.md §7).
func checkDeclaration(l *loader, at string, d *Declaration) {
	for _, f := range []struct{ key, value string }{
		{"operator", d.Operator}, {"location", d.Location}, {"retention", d.Retention},
		{"deletion", d.Deletion},
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

// LogSinkWarnings logs a WARN for each sink profile whose docs_base_url or
// url is not https (sinks.md §3.4: TLS in production; http stays allowed
// for dev and tests), that has no declaration, or that declares a
// third-country location (privacy.md §7). It names the profile, never the
// URL.
func LogSinkWarnings(log *slog.Logger, sinks []SinkProfile) {
	for _, s := range sinks {
		if (s.Type == SinkTypeDocs && !isHTTPS(s.DocsBaseURL)) || (s.Type == SinkTypeWebhook && !isHTTPS(s.URL)) {
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
	}
}

// isHTTPS reports whether u parses with the scheme https, in any case.
func isHTTPS(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.EqualFold(p.Scheme, "https")
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
