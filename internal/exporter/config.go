// Package exporter is the recap-exporter daemon for BBB servers
// (docs/reference/bbb-exporter.md §4–§8): it claims spool entries written by the
// post_archive hook, builds the package manifest, pushes the package to the
// service's ingest API, and deletes raw data per policy.
package exporter

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/logging"
)

// DefaultEnvFile is read unless RECAP_EXPORTER_ENV_FILE names another file (§8.1).
const DefaultEnvFile = "/etc/recap-exporter/exporter.env"

// Configuration keys (docs/reference/deployment.md §5.4).
const (
	keyEnvFile      = "RECAP_EXPORTER_ENV_FILE"
	keyServiceURL   = "RECAP_EXPORTER_SERVICE_URL"
	keyServerID     = "RECAP_EXPORTER_BBB_SERVER_ID"
	keyTokenFile    = "RECAP_EXPORTER_TOKEN_FILE"
	keyMetaPrefix   = "RECAP_EXPORTER_META_PREFIX"
	keyRawDir       = "RECAP_EXPORTER_RAW_DIR"
	keyStatusDir    = "RECAP_EXPORTER_RECORDING_STATUS_DIR"
	keyFormats      = "RECAP_EXPORTER_RECORDING_FORMATS"
	keySpoolDir     = "RECAP_EXPORTER_SPOOL_DIR"
	keyStateDir     = "RECAP_EXPORTER_STATE_DIR"
	keyDeletePolicy = "RECAP_EXPORTER_DELETE_POLICY"
	keyFailureDays  = "RECAP_EXPORTER_DELETE_AFTER_FAILURE_DAYS"
	keyListenAddr   = "RECAP_EXPORTER_LISTEN_ADDR"
	keyLogLevel     = "RECAP_EXPORTER_LOG_LEVEL"
)

// defaults of deployment.md §5.4; keys without a default are required or optional.
var defaults = map[string]string{
	keyTokenFile:    "/etc/recap-exporter/secrets/ingest-token",
	keyMetaPrefix:   "recap",
	keyRawDir:       "/var/bigbluebutton/recording/raw",
	keySpoolDir:     "/var/lib/recap-exporter/spool",
	keyStateDir:     "/var/lib/recap-exporter/state",
	keyDeletePolicy: PolicyNever,
	keyFailureDays:  "0",
	keyListenAddr:   "127.0.0.1:9466",
	keyLogLevel:     "info",
}

// knownKeys are all RECAP_EXPORTER_* keys; any other one is a config error.
var knownKeys = map[string]bool{
	keyEnvFile: true, keyServiceURL: true, keyServerID: true, keyTokenFile: true, keyMetaPrefix: true,
	keyRawDir: true, keyStatusDir: true, keyFormats: true, keySpoolDir: true, keyStateDir: true,
	keyDeletePolicy: true, keyFailureDays: true, keyListenAddr: true, keyLogLevel: true,
}

// Delete policies (§7).
const (
	PolicyNever    = "never"
	PolicyAfterAck = "after_ack"
)

// Grammars (docs/reference/README.md "Identifiers", "Grammars"). The format
// grammar is ours: a format name becomes part of a marker file name.
var (
	serverIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	metaPrefixRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)
	recordIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$`)
	formatRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
)

// ValidRecordID reports whether id matches the record_id grammar. Every
// path built from a record_id goes through it, so an ID never leaves its
// directory.
func ValidRecordID(id string) bool { return recordIDRE.MatchString(id) }

// Config is the validated exporter configuration (§8.1).
type Config struct {
	ServiceURL   string // https://…, without a trailing slash
	BBBServerID  string
	TokenFile    string
	MetaPrefix   string
	RawDir       string
	StatusDir    string   // BBB's recording status dir, parent of published/
	Formats      []string // formats whose published marker must exist before deletion
	SpoolDir     string
	StateDir     string
	DeletePolicy string
	FailureDays  int // 0 = failure expiry off
	ListenAddr   string
	LogLevel     slog.Level
}

// ConfigError lists every problem found in one pass.
type ConfigError []string

func (e ConfigError) Error() string { return strings.Join(e, "\n") }

// LoadConfig reads the env file (RECAP_EXPORTER_ENV_FILE or DefaultEnvFile)
// and the process environment (which wins) and validates the result. environ
// is os.Environ(). On problems it returns a ConfigError naming each key,
// together with the partly filled config.
func LoadConfig(environ []string) (*Config, error) {
	var errs ConfigError
	env := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, "RECAP_EXPORTER_") {
			env[k] = v
		}
	}

	path := DefaultEnvFile
	if p, ok := env[keyEnvFile]; ok && p != "" {
		path = p
	}
	file, bad, err := readEnvFile(path)
	fileMissing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !fileMissing {
		errs = append(errs, err.Error())
	}
	errs = append(errs, bad...)

	vals := map[string]string{}
	for k, v := range file {
		vals[k] = v
	}
	for k, v := range env {
		vals[k] = v
	}
	for _, k := range sortedKeys(vals) {
		if !knownKeys[k] {
			errs = append(errs, k+": unknown key")
		}
	}
	if _, ok := file[keyEnvFile]; ok {
		errs = append(errs, keyEnvFile+": only valid in the environment, not in the env file")
	}
	get := func(k string) string {
		if v := vals[k]; v != "" { // an empty value means the default
			return v
		}
		return defaults[k]
	}

	c := &Config{
		BBBServerID:  get(keyServerID),
		TokenFile:    get(keyTokenFile),
		MetaPrefix:   get(keyMetaPrefix),
		DeletePolicy: get(keyDeletePolicy),
		ListenAddr:   get(keyListenAddr),
	}

	required := func(k string) bool {
		if get(k) != "" {
			return true
		}
		msg := k + ": required"
		if fileMissing {
			msg += fmt.Sprintf(" (env file %s not found)", path)
		}
		errs = append(errs, msg)
		return false
	}

	if required(keyServiceURL) {
		u, err := url.Parse(get(keyServiceURL))
		switch {
		case err != nil || u.Host == "":
			errs = append(errs, keyServiceURL+": not a URL with a host")
		case u.Scheme != "https":
			errs = append(errs, keyServiceURL+": must start with https://")
		case u.User != nil || u.RawQuery != "" || u.Fragment != "":
			errs = append(errs, keyServiceURL+": must not contain user info, a query or a fragment")
		default:
			c.ServiceURL = strings.TrimRight(u.String(), "/")
		}
	}
	if required(keyServerID) && !serverIDRE.MatchString(c.BBBServerID) {
		errs = append(errs, keyServerID+": must match "+serverIDRE.String())
	}
	if !metaPrefixRE.MatchString(c.MetaPrefix) {
		errs = append(errs, keyMetaPrefix+": must match "+metaPrefixRE.String())
	}

	absDir := func(k string) string {
		v := get(k)
		if v == "" {
			return ""
		}
		if !filepath.IsAbs(v) {
			errs = append(errs, k+": must be an absolute path")
			return ""
		}
		return filepath.Clean(v)
	}
	c.RawDir = absDir(keyRawDir)
	c.SpoolDir = absDir(keySpoolDir)
	c.StateDir = absDir(keyStateDir)
	c.StatusDir = absDir(keyStatusDir)
	if t := get(keyTokenFile); t != "" && !filepath.IsAbs(t) {
		errs = append(errs, keyTokenFile+": must be an absolute path")
	}

	if v := get(keyFormats); v != "" {
		for _, f := range strings.Split(v, ",") {
			f = strings.TrimSpace(f)
			if !formatRE.MatchString(f) {
				errs = append(errs, keyFormats+": each format must match "+formatRE.String())
				c.Formats = nil
				break
			}
			c.Formats = append(c.Formats, f)
		}
	}

	if c.DeletePolicy != PolicyNever && c.DeletePolicy != PolicyAfterAck {
		errs = append(errs, keyDeletePolicy+": must be never or after_ack")
	}
	n, err := strconv.Atoi(get(keyFailureDays))
	if err != nil || n < 0 {
		errs = append(errs, keyFailureDays+": must be a whole number of days, 0 or more")
	}
	c.FailureDays = n

	// Every deletion needs BBB's completion markers (§7 condition 2).
	if c.DeletePolicy == PolicyAfterAck || c.FailureDays > 0 {
		why := "required with " + keyDeletePolicy + "=after_ack or " + keyFailureDays + " > 0"
		if get(keyStatusDir) == "" {
			errs = append(errs, keyStatusDir+": "+why)
		}
		if get(keyFormats) == "" {
			errs = append(errs, keyFormats+": "+why)
		}
	}

	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, keyListenAddr+": must be host:port")
	}
	lvl, err := logging.ParseLevel(get(keyLogLevel))
	if err != nil {
		errs = append(errs, keyLogLevel+": "+err.Error())
	}
	c.LogLevel = lvl

	if len(errs) > 0 {
		return c, errs // c is incomplete; callers use it only to report more errors
	}
	return c, nil
}

// readEnvFile parses the env file grammar of §8.1: one KEY=value per line,
// the line's surrounding whitespace trimmed, empty lines and lines starting
// with # ignored, split at the first =, no spaces around it, no quote
// processing. An empty value means the default (see get in LoadConfig). A missing
// file returns an error wrapping fs.ErrNotExist; bad lines are all listed.
func readEnvFile(path string) (map[string]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("env file %s: cannot be read", path)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	var bad []string
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok || !envKeyRE.MatchString(k) || strings.HasPrefix(v, " ") || strings.HasPrefix(v, "\t") {
			bad = append(bad, fmt.Sprintf("env file %s line %d: want KEY=value without spaces around =", path, line))
			continue
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("env file %s: cannot be read", path)
	}
	return out, bad, nil
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReadToken reads the bearer token from TokenFile, without its trailing
// newline. The error never contains the token.
func (c *Config) ReadToken() (string, error) {
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("%s: token file %s cannot be read", keyTokenFile, c.TokenFile)
	}
	tok := strings.TrimRight(string(b), "\r\n")
	if tok == "" || strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("%s: token file %s must hold one non-empty line", keyTokenFile, c.TokenFile)
	}
	return tok, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
