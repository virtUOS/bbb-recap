// Package mockidp is a minimal OpenID Connect provider for dev and tests,
// never for production: discovery, JWKS, an authorization endpoint that
// approves one fixture user at once, and a token endpoint for the
// authorization-code flow with PKCE (S256). ID tokens are RS256 with a key
// generated at start. recap-mock serves it (-oidc-issuer) and the adminui
// tests run it in-process.
//
// It logs the endpoint and the status only: never codes, tokens, the
// client secret or the subject.
package mockidp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Paths below the issuer URL.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	AuthorizePath = "/authorize"
	TokenPath     = "/token"
	JWKSPath      = "/jwks"
)

// codeTTL is how long an authorization code can be exchanged.
const codeTTL = time.Minute

// Config configures the mock provider.
type Config struct {
	// Issuer is the issuer URL as the relying party sees it, e.g.
	// http://mock:8080/oidc. Its path is the prefix of every endpoint.
	Issuer string
	// AuthorizeBaseURL is the issuer URL as the browser sees it; the
	// authorization endpoint in the discovery document uses it. Empty: Issuer.
	AuthorizeBaseURL string
	ClientID         string
	ClientSecret     string
	// RedirectURI is the only redirect URI the client may use (exact match).
	RedirectURI string
	// Subject is the fixture user every login approves.
	Subject string
	// RoleClaim names the claim that carries Roles; empty: "roles".
	RoleClaim string
	// Roles go into RoleClaim. Empty: the claim is left out.
	Roles []string
	// IDTokenTTL is the lifetime of an ID token; zero: 5 minutes. Negative
	// issues tokens that are already expired (tests).
	IDTokenTTL time.Duration
	// RoleValue, if not nil, is put into RoleClaim instead of Roles, e.g. a
	// bare string or an object (tests).
	RoleValue any
	// Nonce, if set, replaces the nonce of the authorization request in the
	// ID token (tests).
	Nonce string
	Log   *slog.Logger
}

// Server is a running mock provider.
type Server struct {
	cfg    Config
	prefix string
	key    *rsa.PrivateKey
	kid    string

	mu    sync.Mutex
	codes map[string]grant
}

// grant is what an authorization code stands for.
type grant struct {
	redirectURI string
	nonce       string
	challenge   string
	expires     time.Time
}

// New validates cfg and generates the signing key.
func New(cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, errors.New("mockidp: issuer must be an http(s) URL without query")
	}
	if cfg.AuthorizeBaseURL == "" {
		cfg.AuthorizeBaseURL = cfg.Issuer
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Subject == "" {
		return nil, errors.New("mockidp: client ID, client secret and subject are required")
	}
	if ru, err := url.Parse(cfg.RedirectURI); err != nil || !ru.IsAbs() || ru.Fragment != "" {
		return nil, errors.New("mockidp: redirect URI must be an absolute URL without fragment")
	}
	if cfg.RoleClaim == "" {
		cfg.RoleClaim = "roles"
	}
	if cfg.IDTokenTTL == 0 {
		cfg.IDTokenTTL = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		prefix: strings.TrimSuffix(u.Path, "/"),
		key:    key,
		kid:    randomString(8),
		codes:  map[string]grant{},
	}, nil
}

// Mount registers the endpoints on mux below the issuer path.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+s.prefix+DiscoveryPath, s.discovery)
	mux.HandleFunc("GET "+s.prefix+JWKSPath, s.jwks)
	mux.HandleFunc("GET "+s.prefix+AuthorizePath, s.authorize)
	mux.HandleFunc("POST "+s.prefix+TokenPath, s.token)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	issuer := strings.TrimSuffix(s.cfg.Issuer, "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.cfg.Issuer,
		"authorization_endpoint":                strings.TrimSuffix(s.cfg.AuthorizeBaseURL, "/") + AuthorizePath,
		"token_endpoint":                        issuer + TokenPath,
		"jwks_uri":                              issuer + JWKSPath,
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"openid"},
	})
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"alg": "RS256",
		"use": "sig",
		"kid": s.kid,
		"n":   b64(pub.N.Bytes()),
		"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize approves the fixture user at once and redirects back with a
// code. An unknown client or a redirect URI other than the configured one
// is a 400 page (never a redirect);
// the others go back to the client as error=… (RFC 6749 §4.1.2.1).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	ru, err := url.Parse(redirectURI)
	if q.Get("client_id") != s.cfg.ClientID || redirectURI != s.cfg.RedirectURI || err != nil {
		s.cfg.Log.Info("mock oidc authorize", "status", http.StatusBadRequest)
		http.Error(w, "mock-oidc: unknown client or invalid redirect_uri", http.StatusBadRequest)
		return
	}
	back := ru.Query()
	if st := q.Get("state"); st != "" {
		back.Set("state", st)
	}
	switch {
	case q.Get("response_type") != "code":
		back.Set("error", "unsupported_response_type")
	case !strings.Contains(" "+q.Get("scope")+" ", " openid "):
		back.Set("error", "invalid_scope")
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		back.Set("error", "invalid_request")
	default:
		code := randomString(32)
		s.mu.Lock()
		s.codes[code] = grant{
			redirectURI: redirectURI,
			nonce:       q.Get("nonce"),
			challenge:   q.Get("code_challenge"),
			expires:     time.Now().Add(codeTTL),
		}
		s.mu.Unlock()
		back.Set("code", code)
	}
	ru.RawQuery = back.Encode()
	s.cfg.Log.Info("mock oidc authorize", "status", http.StatusFound, "error", back.Get("error"))
	http.Redirect(w, r, ru.String(), http.StatusFound)
}

// token exchanges a code for an ID token (RFC 6749 §4.1.3, RFC 7636 §4.6).
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		// RFC 6749 §2.3.1: form-urlencoded before Basic encoding.
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.cfg.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.ClientSecret)) != 1 {
		s.tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		s.tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	code := r.PostForm.Get("code")
	s.mu.Lock()
	g, found := s.codes[code]
	delete(s.codes, code) // single use
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || time.Now().After(g.expires) ||
		r.PostForm.Get("redirect_uri") != g.redirectURI ||
		subtle.ConstantTimeCompare([]byte(b64(sum[:])), []byte(g.challenge)) != 1 {
		s.tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	idToken, err := s.idToken(g.nonce)
	if err != nil {
		s.tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	s.cfg.Log.Info("mock oidc token", "status", http.StatusOK)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": randomString(32),
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     idToken,
	})
}

func (s *Server) tokenError(w http.ResponseWriter, status int, code string) {
	s.cfg.Log.Info("mock oidc token", "status", status, "error", code)
	writeJSON(w, status, map[string]string{"error": code})
}

// idToken signs an RS256 JWT (RFC 7519) for the fixture user.
func (s *Server) idToken(nonce string) (string, error) {
	if s.cfg.Nonce != "" {
		nonce = s.cfg.Nonce
	}
	now := time.Now()
	iat := now
	if s.cfg.IDTokenTTL < 0 {
		iat = now.Add(2 * s.cfg.IDTokenTTL)
	}
	claims := map[string]any{
		"iss": s.cfg.Issuer,
		"sub": s.cfg.Subject,
		"aud": s.cfg.ClientID,
		"iat": iat.Unix(),
		"exp": now.Add(s.cfg.IDTokenTTL).Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	switch {
	case s.cfg.RoleValue != nil:
		claims[s.cfg.RoleClaim] = s.cfg.RoleValue
	case len(s.cfg.Roles) > 0:
		claims[s.cfg.RoleClaim] = s.cfg.Roles
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomString(n int) string { return b64(randomBytes(n)) }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand docs)
	return b
}
