// Package config loads and validates the service configuration of
// docs/reference/deployment.md §5: RECAP_* environment variables, secrets from
// *_FILE files and the YAML files that are needed at start.
package config

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

// Role selects which variables and files are required (deployment.md §5.1–§5.3).
type Role string

// Roles. RoleMigrate needs only the database.
const (
	RoleAPI     Role = "api"
	RoleWorker  Role = "worker"
	RoleMigrate Role = "migrate"
)

// Grammars from docs/reference/README.md.
var (
	idRE         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`) // bbb_server_id, frontend_id
	nameRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)    // meeting types, profiles
	metaPrefixRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)
	languageRE   = regexp.MustCompile(`^[a-z]{2}$`) // ISO 639-1
)

// FrontendServer allows a front end to register recordings of a BBB server.
type FrontendServer struct {
	FrontendID  string
	BBBServerID string
}

// FrontendName allows a front end to request a meeting type or a sink
// profile (RECAP_FRONTEND_TYPES, RECAP_FRONTEND_SINKS).
type FrontendName struct {
	FrontendID string
	Name       string
}

// Config is the validated configuration. Secret values are read from their
// files; they are never logged and never part of Hash.
type Config struct {
	LogLevel slog.Level

	DBDSN          string
	DBPasswordFile string
	DBPassword     string

	BlobBackend            string // "s3" or "fs"
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3AccessKeyIDFile      string
	S3SecretAccessKeyFile  string
	S3AccessKeyID          string
	S3SecretAccessKey      string
	FSRoot                 string
	ProvidersFile          string
	SinksFile              string
	MeetingTypesFile       string
	FormatsDir             string
	MetaPrefix             string
	ClientSecretsDir       string
	FrontendServers        []FrontendServer // empty: every front end may claim packages of every server
	FrontendTypes          []FrontendName   // empty: every front end may request every type
	FrontendSinks          []FrontendName   // empty: every front end may request every sink
	RegistrationTTL        time.Duration
	IngestStagingTTL       time.Duration
	PublicBaseURL          string
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecretFile   string
	OIDCClientSecret       string
	OIDCRoleClaim          string
	SessionKeyFile         string
	SessionKey             string
	WorkerSlots            int
	RetentionAfterDelivery time.Duration
	JobMetadataRetention   time.Duration
	ResultTTL              time.Duration // RECAP_RESULT_TTL (metadata-interface.md §2.7)
	DisplayTZ              *time.Location
	DefaultLanguage        string
	ASRLanguage            string
	MeetingTypes           []MeetingType
	Sinks                  []SinkProfile
	Providers              *providers.Set // worker role only

	effective map[string]string // variable → effective value (non-secret)
	files     map[string][]byte // variable → bytes of a loaded YAML file
}

// defaults for variables that have one (deployment.md §5).
var defaults = map[string]string{
	"RECAP_LOG_LEVEL":                "info",
	"RECAP_BLOB_BACKEND":             "s3",
	"RECAP_S3_REGION":                "us-east-1",
	"RECAP_PROVIDERS_FILE":           "/etc/recap/providers.yaml",
	"RECAP_SINKS_FILE":               "/etc/recap/sinks.yaml",
	"RECAP_MEETING_TYPES_FILE":       "/etc/recap/meeting-types.yaml",
	"RECAP_FORMATS_DIR":              "/etc/recap/formats",
	"RECAP_META_PREFIX":              "recap",
	"RECAP_CLIENT_SECRETS_DIR":       "/run/secrets/recap/clients",
	"RECAP_REGISTRATION_TTL":         "14d",
	"RECAP_INGEST_STAGING_TTL":       "72h",
	"RECAP_WORKER_SLOTS":             "4",
	"RECAP_RETENTION_AFTER_DELIVERY": "0",
	"RECAP_JOB_METADATA_RETENTION":   "180d",
	"RECAP_RESULT_TTL":               "7d",
	"RECAP_DISPLAY_TZ":               "UTC",
	"RECAP_DEFAULT_LANGUAGE":         "en",
	"RECAP_OIDC_ROLE_CLAIM":          "roles",
}

// known lists every variable of deployment.md §5.1–§5.3.
var known = []string{
	"RECAP_LOG_LEVEL", "RECAP_DB_DSN", "RECAP_DB_PASSWORD_FILE", "RECAP_BLOB_BACKEND",
	"RECAP_S3_ENDPOINT", "RECAP_S3_REGION", "RECAP_S3_BUCKET",
	"RECAP_S3_ACCESS_KEY_ID_FILE", "RECAP_S3_SECRET_ACCESS_KEY_FILE", "RECAP_FS_ROOT",
	"RECAP_PROVIDERS_FILE", "RECAP_SINKS_FILE", "RECAP_MEETING_TYPES_FILE", "RECAP_FORMATS_DIR",
	"RECAP_META_PREFIX",
	"RECAP_CLIENT_SECRETS_DIR", "RECAP_FRONTEND_SERVERS",
	"RECAP_FRONTEND_TYPES", "RECAP_FRONTEND_SINKS", "RECAP_REGISTRATION_TTL",
	"RECAP_INGEST_STAGING_TTL", "RECAP_PUBLIC_BASE_URL", "RECAP_OIDC_ISSUER", "RECAP_OIDC_CLIENT_ID",
	"RECAP_OIDC_CLIENT_SECRET_FILE", "RECAP_OIDC_ROLE_CLAIM", "RECAP_SESSION_KEY_FILE",
	"RECAP_WORKER_SLOTS", "RECAP_RETENTION_AFTER_DELIVERY", "RECAP_JOB_METADATA_RETENTION",
	"RECAP_RESULT_TTL", "RECAP_DISPLAY_TZ", "RECAP_DEFAULT_LANGUAGE", "RECAP_ASR_LANGUAGE",
}

// ignored reports whether a RECAP_* variable belongs to another program on
// the same host (deployment.md §5).
func ignored(name string) bool {
	return name == "RECAP_BIN" ||
		strings.HasPrefix(name, "RECAP_EXPORTER_") ||
		strings.HasPrefix(name, "RECAP_CONTRACT_")
}

// loader collects all errors of one Load call.
type loader struct {
	env  map[string]string
	errs []error
}

func (l *loader) errorf(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

// get returns the value of a variable or its default. Empty counts as unset.
func (l *loader) get(name string) string {
	if v := l.env[name]; v != "" {
		return v
	}
	return defaults[name]
}

func (l *loader) required(name string) string {
	v := l.get(name)
	if v == "" {
		l.errorf("%s: required", name)
	}
	return v
}

func (l *loader) duration(name string) time.Duration {
	d, err := ParseDuration(l.get(name))
	if err != nil {
		l.errorf("%s: %v", name, err)
	}
	return d
}

// secret reads the file named by a *_FILE variable, trimmed of one trailing newline.
func (l *loader) secret(name string) string {
	path := l.required(name)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		l.errorf("%s: cannot read file %s", name, path)
		return ""
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		l.errorf("%s: file %s is empty", name, path)
	}
	return s
}

// ParseDuration parses Go duration syntax plus whole days ("14d"). Negative
// durations are rejected.
func ParseDuration(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

func has(roles []Role, r Role) bool { return slices.Contains(roles, r) }

// Load reads the configuration from environ (os.Environ format) for the given
// roles. It collects every error and returns them together (errors.Join), so
// the caller can print all of them and exit 2.
func Load(environ []string, roles ...Role) (*Config, error) {
	l := &loader{env: map[string]string{}}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "RECAP_") || ignored(name) {
			continue
		}
		if !slices.Contains(known, name) {
			l.errorf("%s: unknown variable", name)
			continue
		}
		l.env[name] = value
	}

	api, worker := has(roles, RoleAPI), has(roles, RoleWorker)
	service := api || worker
	c := &Config{effective: map[string]string{}, files: map[string][]byte{}}

	level, err := logging.ParseLevel(l.get("RECAP_LOG_LEVEL"))
	if err != nil {
		l.errorf("RECAP_LOG_LEVEL: %v", err)
	}
	c.LogLevel = level

	c.DBDSN = l.required("RECAP_DB_DSN")
	if c.DBDSN != "" {
		if err := checkDSN(c.DBDSN); err != nil {
			l.errorf("RECAP_DB_DSN: %v", err)
		}
	}
	c.DBPasswordFile = l.get("RECAP_DB_PASSWORD_FILE")
	c.DBPassword = l.secret("RECAP_DB_PASSWORD_FILE")

	c.BlobBackend = l.get("RECAP_BLOB_BACKEND")
	c.S3Endpoint = l.get("RECAP_S3_ENDPOINT")
	c.S3Region = l.get("RECAP_S3_REGION")
	c.S3Bucket = l.get("RECAP_S3_BUCKET")
	c.S3AccessKeyIDFile = l.get("RECAP_S3_ACCESS_KEY_ID_FILE")
	c.S3SecretAccessKeyFile = l.get("RECAP_S3_SECRET_ACCESS_KEY_FILE")
	c.FSRoot = l.get("RECAP_FS_ROOT")
	switch c.BlobBackend {
	case "s3":
		if c.S3Endpoint != "" {
			u, err := url.Parse(c.S3Endpoint)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				l.errorf("RECAP_S3_ENDPOINT: want an http(s) URL")
			}
		}
		if service {
			l.required("RECAP_S3_ENDPOINT")
			l.required("RECAP_S3_BUCKET")
			c.S3AccessKeyID = l.secret("RECAP_S3_ACCESS_KEY_ID_FILE")
			c.S3SecretAccessKey = l.secret("RECAP_S3_SECRET_ACCESS_KEY_FILE")
		}
	case "fs":
		if service {
			l.required("RECAP_FS_ROOT")
		}
	default:
		l.errorf("RECAP_BLOB_BACKEND: want s3 or fs, got %q", c.BlobBackend)
	}

	c.ProvidersFile = l.get("RECAP_PROVIDERS_FILE")
	c.SinksFile = l.get("RECAP_SINKS_FILE")
	c.MeetingTypesFile = l.get("RECAP_MEETING_TYPES_FILE")
	c.FormatsDir = l.get("RECAP_FORMATS_DIR")
	// A file with errors gives no names to check the allow-lists against
	// (RECAP_FRONTEND_TYPES, RECAP_FRONTEND_SINKS); its own errors are the root cause.
	typesOK, sinksOK := false, false
	if service {
		n := len(l.errs)
		c.MeetingTypes = loadMeetingTypes(l, c, "RECAP_MEETING_TYPES_FILE")
		typesOK, n = len(l.errs) == n, len(l.errs)
		c.Sinks = loadSinks(l, c, "RECAP_SINKS_FILE", worker)
		sinksOK = len(l.errs) == n
	}
	if worker {
		c.Providers = loadProviders(l, c, "RECAP_PROVIDERS_FILE")
	}
	if service {
		// Both roles hash all three files, including the ones they do not parse.
		for _, name := range hashFiles {
			if _, ok := c.files[name]; !ok {
				readRaw(l, c, name)
			}
		}
	}

	c.MetaPrefix = l.get("RECAP_META_PREFIX")
	if !metaPrefixRE.MatchString(c.MetaPrefix) {
		l.errorf("RECAP_META_PREFIX: must match %s", metaPrefixRE)
	}

	c.ClientSecretsDir = l.get("RECAP_CLIENT_SECRETS_DIR")
	if api {
		if fi, err := os.Stat(c.ClientSecretsDir); err != nil || !fi.IsDir() {
			l.errorf("RECAP_CLIENT_SECRETS_DIR: %s is not a readable directory", c.ClientSecretsDir)
		}
	}
	// Empty RECAP_FRONTEND_SERVERS: every front end may claim packages of
	// every BBB server (metadata-interface.md §2.1).
	if v := l.get("RECAP_FRONTEND_SERVERS"); v != "" {
		c.FrontendServers = parseFrontendServers(l, v)
	}
	if service {
		var types, sinks []string
		for _, mt := range c.MeetingTypes {
			types = append(types, mt.Name)
		}
		for _, s := range c.Sinks {
			sinks = append(sinks, s.Name)
		}
		c.FrontendTypes = parseFrontendNames(l, "RECAP_FRONTEND_TYPES", "meeting type", c.FrontendServers, types, typesOK)
		c.FrontendSinks = parseFrontendNames(l, "RECAP_FRONTEND_SINKS", "sink profile", c.FrontendServers, sinks, sinksOK)
	}
	c.RegistrationTTL = l.duration("RECAP_REGISTRATION_TTL")
	c.IngestStagingTTL = l.duration("RECAP_INGEST_STAGING_TTL")

	// Admin UI: setting RECAP_OIDC_ISSUER makes the other OIDC variables required.
	c.PublicBaseURL = l.get("RECAP_PUBLIC_BASE_URL")
	c.OIDCIssuer = l.get("RECAP_OIDC_ISSUER")
	c.OIDCClientID = l.get("RECAP_OIDC_CLIENT_ID")
	c.OIDCClientSecretFile = l.get("RECAP_OIDC_CLIENT_SECRET_FILE")
	c.OIDCRoleClaim = l.get("RECAP_OIDC_ROLE_CLAIM")
	c.SessionKeyFile = l.get("RECAP_SESSION_KEY_FILE")
	if api && c.OIDCIssuer != "" {
		l.required("RECAP_PUBLIC_BASE_URL")
		l.required("RECAP_OIDC_CLIENT_ID")
		l.required("RECAP_OIDC_ROLE_CLAIM")
		c.OIDCClientSecret = l.secret("RECAP_OIDC_CLIENT_SECRET_FILE")
		c.SessionKey = l.secret("RECAP_SESSION_KEY_FILE")
		if c.SessionKey != "" && len(c.SessionKey) < 32 {
			l.errorf("RECAP_SESSION_KEY_FILE: key must be at least 32 bytes")
		}
	}

	slots, err := strconv.Atoi(l.get("RECAP_WORKER_SLOTS"))
	if err != nil || slots < 1 {
		l.errorf("RECAP_WORKER_SLOTS: want a positive integer")
	}
	c.WorkerSlots = slots
	c.RetentionAfterDelivery = l.duration("RECAP_RETENTION_AFTER_DELIVERY")
	c.JobMetadataRetention = l.duration("RECAP_JOB_METADATA_RETENTION")
	c.ResultTTL = l.duration("RECAP_RESULT_TTL")
	if sinksOK {
		checkFrontendSinks(l, c)
	}
	if c.DisplayTZ, err = time.LoadLocation(l.get("RECAP_DISPLAY_TZ")); err != nil {
		l.errorf("RECAP_DISPLAY_TZ: unknown time zone %q", l.get("RECAP_DISPLAY_TZ"))
	}
	c.DefaultLanguage = l.get("RECAP_DEFAULT_LANGUAGE")
	if !languageRE.MatchString(c.DefaultLanguage) {
		l.errorf("RECAP_DEFAULT_LANGUAGE: want an ISO 639-1 code")
	}
	c.ASRLanguage = l.get("RECAP_ASR_LANGUAGE")
	if c.ASRLanguage != "" && !languageRE.MatchString(c.ASRLanguage) {
		l.errorf("RECAP_ASR_LANGUAGE: want an ISO 639-1 code or empty")
	}

	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	for _, name := range known {
		c.effective[name] = l.get(name)
	}
	return c, nil
}

// checkFrontendSinks checks what a frontend sink profile needs beyond its
// own keys (sinks.md §4a): RECAP_RESULT_TTL above 0.
func checkFrontendSinks(l *loader, c *Config) {
	for i, s := range c.Sinks {
		if s.Type == SinkTypeFrontend && c.ResultTTL == 0 {
			l.errorf("RECAP_SINKS_FILE: sinks[%d]: type frontend needs RECAP_RESULT_TTL above 0 "+
				"(the summary waits for the front end)", i)
		}
	}
}

// checkDSN accepts a postgres:// URL without a password (deployment.md §5.1).
func checkDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("want a postgres:// URL")
	}
	if _, ok := u.User.Password(); ok || u.Query().Has("password") {
		return errors.New("must not contain a password; use RECAP_DB_PASSWORD_FILE")
	}
	return nil
}

func parseFrontendServers(l *loader, v string) []FrontendServer {
	var list []FrontendServer
	for _, item := range strings.Split(v, ",") {
		fe, srv, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || !idRE.MatchString(fe) || !idRE.MatchString(srv) {
			l.errorf("RECAP_FRONTEND_SERVERS: item %q: want <frontend_id>:<bbb_server_id>", item)
			continue
		}
		list = append(list, FrontendServer{FrontendID: fe, BBBServerID: srv})
	}
	return list
}

// parseFrontendNames parses a list of <frontend_id>:<name> with the grammar
// of RECAP_FRONTEND_SERVERS. If RECAP_FRONTEND_SERVERS is set, the front
// end must appear in it; if checkNames is set, the name must be in
// configured, the names of the meeting-types or sinks file
// (metadata-interface.md §3 check 9a). checkNames is false when that file
// has errors, so only its own errors are reported.
func parseFrontendNames(l *loader, variable, kind string, servers []FrontendServer, configured []string,
	checkNames bool) []FrontendName {
	v := l.get(variable)
	if v == "" {
		return nil
	}
	var list []FrontendName
	for _, item := range strings.Split(v, ",") {
		fe, name, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || !idRE.MatchString(fe) || !nameRE.MatchString(name) {
			l.errorf("%s: item %q: want <frontend_id>:<%s>", variable, item, kind)
			continue
		}
		if len(servers) > 0 && !slices.ContainsFunc(servers, func(s FrontendServer) bool { return s.FrontendID == fe }) {
			l.errorf("%s: item %q: front end %q is not in RECAP_FRONTEND_SERVERS", variable, item, fe)
			continue
		}
		if checkNames && !slices.Contains(configured, name) {
			l.errorf("%s: item %q: %s %q is not configured", variable, item, kind, name)
			continue
		}
		list = append(list, FrontendName{FrontendID: fe, Name: name})
	}
	return list
}

// hashFiles are the files config_hash covers, in this order (architecture.md §5.2).
var hashFiles = []string{"RECAP_MEETING_TYPES_FILE", "RECAP_PROVIDERS_FILE", "RECAP_SINKS_FILE"}

// Hash is the config_hash a job pins (architecture.md §5.2): the lowercase
// hex SHA-256 over the raw bytes of the meeting-types, providers and sinks
// files, in that order, each prefixed by its length as an 8-byte big-endian
// integer. api and worker compute the same value. For the migrate role,
// which reads none of the files, it is the hash of three empty files.
func (c *Config) Hash() string {
	h := sha256.New()
	for _, name := range hashFiles {
		b := c.files[name]
		_ = binary.Write(h, binary.BigEndian, uint64(len(b)))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LogBoot writes the boot log line: effective values and file paths, never
// file contents. The blob backend is one of those values (INFO, no warning:
// fs is supported on compose). It warns on an http:// S3 endpoint.
func (c *Config) LogBoot(log *slog.Logger) {
	attrs := make([]any, 0, len(known)+1)
	for _, name := range known {
		attrs = append(attrs, slog.String(name, c.effective[name]))
	}
	attrs = append(attrs, slog.String("config_hash", c.Hash()))
	log.Info("configuration loaded", attrs...)
	if c.BlobBackend == "s3" && strings.HasPrefix(c.S3Endpoint, "http://") {
		log.Warn("S3 endpoint uses plain http")
	}
}
