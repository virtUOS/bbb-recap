// Package httpapi holds the HTTP plumbing shared by the ingest, registration
// and admin APIs: client credentials and bearer auth, RFC 9457 problems, a
// size-limited body reader, request logging and the server lifecycle
// (docs/reference/README.md "HTTP", architecture.md §9).
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is the kind of a client, taken from its credential file name.
type Kind string

// Client kinds (README "HTTP").
const (
	KindBBBServer Kind = "bbb_server"
	KindFrontend  Kind = "frontend"
	KindAdmin     Kind = "admin"
)

// Client is an authenticated caller. Kind and ID come only from the credential.
type Client struct {
	Kind Kind
	ID   string
}

var clientIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type credential struct {
	client Client
	file   string
	sum    [sha256.Size]byte // SHA-256 of the token, so all comparisons have the same length
}

// Credentials are the client tokens of RECAP_CLIENT_SECRETS_DIR.
type Credentials struct {
	list []credential
}

// LoadCredentials reads one file per client from dir: bbb_server.<id>,
// frontend.<id>, admin.<name>, and <file>.next for rotation. Each file holds
// the token, trimmed of one trailing newline. Names starting with "." and
// directories are skipped (Kubernetes secret volumes add "..data" entries).
// The same token in two files is an error, since the caller's identity must
// come from the credential alone. All errors are returned together.
func LoadCredentials(dir string) (*Credentials, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("client secrets: %w", err)
	}
	c := &Credentials{}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			continue
		}
		client, ok := parseCredentialName(name)
		if !ok {
			errs = append(errs, fmt.Errorf("client secrets: %s: want bbb_server.<id>, frontend.<id> or admin.<name> (optionally .next)", name))
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("client secrets: cannot read %s", name))
			continue
		}
		token := strings.TrimSuffix(string(b), "\n")
		if token == "" {
			errs = append(errs, fmt.Errorf("client secrets: %s is empty", name))
			continue
		}
		cr := credential{client: client, file: name, sum: sha256.Sum256([]byte(token))}
		for _, other := range c.list {
			if other.sum == cr.sum {
				errs = append(errs, fmt.Errorf("client secrets: %s and %s hold the same token", other.file, name))
			}
		}
		c.list = append(c.list, cr)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func parseCredentialName(name string) (Client, bool) {
	name = strings.TrimSuffix(name, ".next")
	kind, id, ok := strings.Cut(name, ".")
	switch Kind(kind) {
	case KindBBBServer, KindFrontend, KindAdmin:
	default:
		return Client{}, false
	}
	if !ok || !clientIDRE.MatchString(id) {
		return Client{}, false
	}
	return Client{Kind: Kind(kind), ID: id}, true
}

// Authenticate returns the client whose credential equals token. Every
// credential is compared in constant time.
func (c *Credentials) Authenticate(token string) (Client, bool) {
	sum := sha256.Sum256([]byte(token))
	var found Client
	ok := false
	for _, cr := range c.list {
		if subtle.ConstantTimeCompare(sum[:], cr.sum[:]) == 1 {
			found, ok = cr.client, true
		}
	}
	return found, ok && token != ""
}

type clientKey struct{}

// ClientFrom returns the client that Require put into the request context.
func ClientFrom(ctx context.Context) (Client, bool) {
	c, ok := ctx.Value(clientKey{}).(Client)
	return c, ok
}

// Require wraps next so that only a client of the given kind gets through.
// A missing, unknown or wrong-kind credential gets 401 unauthorized.
func (c *Credentials) Require(kind Kind, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		client, found := c.Authenticate(token)
		if !ok || !found || client.Kind != kind {
			w.Header().Set("WWW-Authenticate", "Bearer")
			WriteProblem(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, client)))
	})
}
