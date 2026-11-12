// Package adminui is the admin web UI under /admin/ on the public port
// (docs/reference/architecture.md §11). This file has the login half: OIDC
// authorization-code flow with PKCE against the configured issuer, a
// signed session cookie, a CSRF token for POSTs and the role check.
// views.go has the job list, job page, audit list and the actions; they
// call the admin API's Go functions (internal/admin) directly.
//
// Logging (privacy.md §6): login success and failure at INFO with a reason
// code only. Never the subject, tokens, codes, cookies or the client secret.
package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/virtUOS/bbb-recap/internal/admin"
)

// AdminRole is the role value the role claim must contain (architecture.md §11).
const AdminRole = "recap-admin"

// csrfField is the form field that carries the CSRF token on every POST.
const csrfField = "csrf_token"

// idpTimeout bounds every request to the IdP.
const idpTimeout = 10 * time.Second

// Config configures the UI. All fields but Log, HTTPClient and Admin are
// required.
type Config struct {
	PublicBaseURL string // external URL of the service; the redirect URI is <it>/admin/callback
	Issuer        string
	ClientID      string
	ClientSecret  string
	RoleClaim     string // name of a top-level claim holding a string list
	SessionKey    []byte // at least 32 bytes
	Log           *slog.Logger
	HTTPClient    *http.Client // for the IdP; nil: a client with a timeout
	// Admin serves the views and actions. nil: only the login and a
	// placeholder home page are mounted.
	Admin *admin.API
}

// UI serves /admin/.
type UI struct {
	cfg    Config
	sign   signer
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider // discovered on first use, so api starts while the IdP is down
}

// New checks cfg and returns the UI. It does not contact the IdP.
func New(cfg Config) (*UI, error) {
	if cfg.PublicBaseURL == "" || cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RoleClaim == "" {
		return nil, errors.New("adminui: public base URL, issuer, client ID, client secret and role claim are required")
	}
	if len(cfg.SessionKey) < 32 {
		return nil, errors.New("adminui: session key must be at least 32 bytes")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: idpTimeout}
	}
	return &UI{cfg: cfg, sign: signer{key: cfg.SessionKey}, client: client, now: time.Now}, nil
}

// Mount registers the routes on mux: the login, and with Config.Admin the
// views and actions (views.go).
func (u *UI) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /admin/login", u.login)
	mux.HandleFunc("GET /admin/callback", u.callback)
	mux.Handle("POST /admin/logout", u.Protect(http.HandlerFunc(u.logout)))
	mux.HandleFunc("GET /admin/static/style.css", serveStyle)
	if u.cfg.Admin == nil {
		mux.Handle("GET /admin/{$}", u.Protect(http.HandlerFunc(u.home)))
		return
	}
	u.mountViews(mux)
}

type sessionKey struct{}

// SessionFrom returns the session Protect put into the request context.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// Protect lets a request through only with a valid session. Without one, a
// GET or HEAD is redirected to the login and anything else is 403. A
// request that is not GET or HEAD also needs the session's CSRF token in
// the form field csrf_token, else 403.
func (u *UI) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		s, err := u.sign.readSession(r, u.now())
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if err != nil {
			if safe {
				http.Redirect(w, r, "/admin/login", http.StatusFound)
				return
			}
			u.page(w, http.StatusForbidden, view{Title: "Not logged in", Message: "Your session has expired. Log in again."})
			return
		}
		if !safe {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !equal(r.PostFormValue(csrfField), s.CSRF) {
				u.cfg.Log.Info("admin request rejected", "reason", "csrf-mismatch")
				u.page(w, http.StatusForbidden, view{Title: "Request rejected", Message: "The form was out of date. Reload the page and try again."})
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}

// oidcProvider discovers the issuer once. A failure is retried on the next login.
func (u *UI) oidcProvider(ctx context.Context) (*oidc.Provider, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.provider != nil {
		return u.provider, nil
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, u.client), idpTimeout)
	defer cancel()
	p, err := oidc.NewProvider(ctx, u.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	u.provider = p
	return p, nil
}

func (u *UI) oauth2Config(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     u.cfg.ClientID,
		ClientSecret: u.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  strings.TrimSuffix(u.cfg.PublicBaseURL, "/") + "/admin/callback",
		Scopes:       []string{oidc.ScopeOpenID},
	}
}

// login starts the code flow: state, nonce and PKCE verifier go into a
// short-lived signed cookie, then the browser goes to the IdP.
func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p, err := u.oidcProvider(r.Context())
	if err != nil {
		u.cfg.Log.Warn("admin login failed", "reason", "discovery-failed", "class", discoveryErrorClass(err))
		u.page(w, http.StatusBadGateway, view{Title: "Login unavailable", Message: "The identity provider cannot be reached. Try again later."})
		return
	}
	ls := loginState{
		State:    randomToken(),
		Nonce:    randomToken(),
		Verifier: oauth2.GenerateVerifier(),
		Expires:  u.now().Add(loginTTL).Unix(),
	}
	v, err := u.sign.encode(loginCookie, ls)
	if err != nil {
		u.page(w, http.StatusInternalServerError, view{Title: "Error", Message: "Internal error."})
		return
	}
	setCookie(w, loginCookie, v, int(loginTTL.Seconds()))
	http.Redirect(w, r, u.oauth2Config(p).AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

// discoveryErrorClass reduces a discovery error to a fixed class for the
// log (privacy.md §6 rule 3): go-oidc puts the IdP's response body into the
// error text for a non-200 answer, so the error itself is never logged.
func discoveryErrorClass(err error) string {
	var ne net.Error
	var ue *url.Error
	var im *oidc.IssuerMismatchError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.As(err, &im):
		return "issuer-mismatch"
	case errors.As(err, &ue):
		return "unreachable"
	default:
		return "bad-response" // non-200 status or an invalid discovery document
	}
}

// oauthErrors are the error codes of RFC 6749 §4.1.2.1 and §5.2; only these
// are logged, any other value as "other".
var oauthErrors = []string{
	"invalid_request", "unauthorized_client", "access_denied", "unsupported_response_type", "invalid_scope",
	"server_error", "temporarily_unavailable", "invalid_client", "invalid_grant", "unsupported_grant_type",
}

func oauthError(code string) string {
	if slices.Contains(oauthErrors, code) {
		return code
	}
	return "other"
}

// callback finishes the code flow. Any failure ends without a session.
func (u *UI) callback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	ls, err := u.sign.readLogin(r, u.now())
	clearCookie(w, loginCookie)
	q := r.URL.Query()
	switch {
	case err != nil:
		u.denied(w, "login-state-invalid")
		return
	case !equal(q.Get("state"), ls.State):
		u.denied(w, "state-mismatch")
		return
	case q.Get("error") != "":
		u.denied(w, "idp-error", "oauth_error", oauthError(q.Get("error")))
		return
	}
	p, err := u.oidcProvider(r.Context())
	if err != nil {
		u.denied(w, "discovery-failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), oauth2.HTTPClient, u.client), idpTimeout)
	defer cancel()
	tok, err := u.oauth2Config(p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		code := "other"
		if re := (*oauth2.RetrieveError)(nil); errors.As(err, &re) {
			code = oauthError(re.ErrorCode)
		}
		u.denied(w, "code-exchange-failed", "oauth_error", code)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		u.denied(w, "no-id-token")
		return
	}
	// The provider's key set uses u.client (oidcProvider) and caches the keys.
	idt, err := p.Verifier(&oidc.Config{ClientID: u.cfg.ClientID, Now: u.now}).Verify(ctx, raw)
	if err != nil {
		u.denied(w, "id-token-invalid")
		return
	}
	if !equal(idt.Nonce, ls.Nonce) {
		u.denied(w, "nonce-mismatch")
		return
	}
	if !hasRole(idt, u.cfg.RoleClaim) {
		u.denied(w, "missing-role")
		return
	}
	s := Session{Subject: idt.Subject, CSRF: randomToken(), Expires: u.now().Add(sessionTTL).Unix()}
	v, err := u.sign.encode(sessionCookie, s)
	if err != nil {
		u.page(w, http.StatusInternalServerError, view{Title: "Error", Message: "Internal error."})
		return
	}
	setCookie(w, sessionCookie, v, int(sessionTTL.Seconds()))
	u.cfg.Log.Info("admin login", "outcome", "ok")
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

// hasRole reports whether the top-level claim is a string list that
// contains AdminRole. Nested claims are not supported.
func hasRole(idt *oidc.IDToken, claim string) bool {
	var claims map[string]json.RawMessage
	if idt.Claims(&claims) != nil {
		return false
	}
	var roles []string
	if json.Unmarshal(claims[claim], &roles) != nil {
		return false
	}
	return slices.Contains(roles, AdminRole)
}

// denied logs a failed login with its reason code and answers 403.
func (u *UI) denied(w http.ResponseWriter, reason string, attrs ...any) {
	u.cfg.Log.Info("admin login failed", append([]any{"reason", reason}, attrs...)...)
	d := view{Title: "Login failed", Message: "The login could not be completed. Start again from the admin page."}
	if reason == "missing-role" {
		d = view{Title: "Access denied", Message: "Your account does not have the " + AdminRole + " role."}
	}
	u.page(w, http.StatusForbidden, d)
}

// home is the placeholder home page without Config.Admin.
func (u *UI) home(w http.ResponseWriter, r *http.Request) {
	s, _ := SessionFrom(r.Context())
	u.page(w, http.StatusOK, view{Title: "bbb-recap admin", Session: &s,
		Message: "The admin views are not configured."})
}

func (u *UI) logout(w http.ResponseWriter, _ *http.Request) {
	clearCookie(w, sessionCookie)
	u.cfg.Log.Info("admin logout")
	u.page(w, http.StatusOK, view{Title: "Logged out", Message: "You are logged out of bbb-recap.", LoggedOut: true})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}
