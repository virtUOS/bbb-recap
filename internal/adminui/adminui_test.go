package adminui

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/admin"
	"github.com/virtUOS/bbb-recap/internal/adminui/mockidp"
)

// Fixture values (docs/reference/fixtures.md §4, §5).
const (
	testSubject      = "fixture-sub-admin-0010" // the fixture admin, never an owner
	testClientID     = "recap-admin-ui"
	testClientSecret = "FIXTURE-NOT-A-SECRET-oidc-00000000000000000000000000000000"
	testSessionKey   = "FIXTURE-NOT-A-SECRET-session-0000000000000000000000000000000"
)

// env is one UI (TLS, so the Secure cookies flow) and one mock IdP.
type env struct {
	ui      *httptest.Server
	idp     *httptest.Server
	client  *http.Client // cookie jar, follows redirects
	logs    *bytes.Buffer
	subject string // of the logged-in admin
}

func newEnv(t *testing.T, idpCfg mockidp.Config) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	return newEnvWith(t, idpCfg, testSubject, nil, logs)
}

// newEnvWith is newEnv with the admin's subject, an admin API for the views
// (nil: login only) and the log buffer the admin API also writes to.
func newEnvWith(t *testing.T, idpCfg mockidp.Config, subject string, api *admin.API, logs *bytes.Buffer) *env {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	uiMux := http.NewServeMux()
	ui := httptest.NewTLSServer(uiMux)
	t.Cleanup(ui.Close)

	idpMux := http.NewServeMux()
	idp := httptest.NewServer(idpMux)
	t.Cleanup(idp.Close)
	idpCfg.Issuer = idp.URL + "/oidc"
	idpCfg.ClientID = testClientID
	idpCfg.ClientSecret = testClientSecret
	idpCfg.RedirectURI = ui.URL + "/admin/callback"
	idpCfg.Subject = subject
	idpCfg.Log = log
	p, err := mockidp.New(idpCfg)
	if err != nil {
		t.Fatal(err)
	}
	p.Mount(idpMux)

	u, err := New(Config{
		PublicBaseURL: ui.URL,
		Issuer:        idpCfg.Issuer,
		ClientID:      testClientID,
		ClientSecret:  testClientSecret,
		RoleClaim:     "roles",
		SessionKey:    []byte(testSessionKey),
		Log:           log,
		Admin:         api,
	})
	if err != nil {
		t.Fatal(err)
	}
	u.Mount(uiMux)

	client := ui.Client()
	client.Jar, _ = cookiejar.New(nil)
	return &env{ui: ui, idp: idp, client: client, logs: logs, subject: subject}
}

func admins() mockidp.Config { return mockidp.Config{Roles: []string{"other", AdminRole}} }

func (e *env) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := e.client.Get(e.ui.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := e.client.PostForm(e.ui.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// noFollow returns a copy of the client that does not follow redirects.
func (e *env) noFollow() *http.Client {
	c := *e.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (e *env) cookie(name string) *http.Cookie {
	u, _ := url.Parse(e.ui.URL + cookiePath)
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var csrfRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// login runs the whole flow from /admin/ and returns the CSRF token of the
// home page.
func (e *env) login(t *testing.T) string {
	t.Helper()
	resp, body := e.get(t, "/admin/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Logged in as <code>"+e.subject+"</code>") {
		t.Fatalf("GET /admin/ = %d, want 200 with the subject; body:\n%s", resp.StatusCode, body)
	}
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token in the home page:\n%s", body)
	}
	return m[1]
}

// checkLogs fails on secrets, the subject or cookie values in the log.
func (e *env) checkLogs(t *testing.T) {
	t.Helper()
	logs := e.logs.String()
	for _, bad := range []string{e.subject, testClientSecret, testSessionKey, "id_token", "code="} {
		if strings.Contains(logs, bad) {
			t.Errorf("log contains %q:\n%s", bad, logs)
		}
	}
	for _, c := range []string{sessionCookie, loginCookie} {
		if ck := e.cookie(c); ck != nil && strings.Contains(logs, ck.Value) {
			t.Errorf("log contains the value of %s", c)
		}
	}
}

func TestLoginRoundTrip(t *testing.T) {
	e := newEnv(t, admins())
	e.login(t)
	if e.cookie(sessionCookie) == nil {
		t.Fatal("no session cookie after login")
	}
	if e.cookie(loginCookie) != nil {
		t.Error("login cookie still set after the callback")
	}
	if !strings.Contains(e.logs.String(), `"msg":"admin login","outcome":"ok"`) {
		t.Errorf("no login success in the log:\n%s", e.logs.String())
	}

	// Cookie attributes, from the raw Set-Cookie of a fresh callback.
	e2 := newEnv(t, admins())
	nf := e2.noFollow()
	resp, err := nf.Get(e2.ui.URL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	resp, err = nf.Get(resp.Header.Get("Location")) // IdP authorize
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	resp, err = nf.Get(resp.Header.Get("Location")) // callback
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback = %d, want 303", resp.StatusCode)
	}
	var set string
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, sessionCookie+"=") {
			set = h
		}
	}
	for _, attr := range []string{"Path=/admin/", "Max-Age=28800", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(set, attr) {
			t.Errorf("session Set-Cookie %q lacks %s", set, attr)
		}
	}

	// The authorization request carried PKCE S256 and a nonce.
	e3 := newEnv(t, admins())
	resp, err = e3.noFollow().Get(e3.ui.URL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
		q.Get("state") == "" || q.Get("redirect_uri") != e3.ui.URL+"/admin/callback" {
		t.Errorf("authorization request %v lacks PKCE, nonce, state or the redirect URI", q)
	}

	e.checkLogs(t)
}

// rejected runs the flow and checks that the callback answers 403 and no
// session exists.
func rejected(t *testing.T, e *env, wantReason string) {
	t.Helper()
	resp, body := e.get(t, "/admin/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login = %d, want 403; body:\n%s", resp.StatusCode, body)
	}
	if e.cookie(sessionCookie) != nil {
		t.Error("session cookie set after a rejected login")
	}
	if !strings.Contains(e.logs.String(), `"reason":"`+wantReason+`"`) {
		t.Errorf("log lacks reason %s:\n%s", wantReason, e.logs.String())
	}
	e.checkLogs(t)
}

func TestLoginRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    mockidp.Config
		reason string
	}{
		{"wrong nonce", mockidp.Config{Roles: []string{AdminRole}, Nonce: "not-the-nonce"}, "nonce-mismatch"},
		{"expired ID token", mockidp.Config{Roles: []string{AdminRole}, IDTokenTTL: -time.Hour}, "id-token-invalid"},
		{"no role claim", mockidp.Config{}, "missing-role"},
		{"role claim without the role", mockidp.Config{Roles: []string{"viewer"}}, "missing-role"},
		{"role under another claim", mockidp.Config{Roles: []string{AdminRole}, RoleClaim: "groups"}, "missing-role"},
		{"role claim a bare string", mockidp.Config{RoleValue: AdminRole}, "missing-role"},
		{"role claim an object", mockidp.Config{RoleValue: map[string]any{"roles": []string{AdminRole}}}, "missing-role"},
		{"role claim a list with a non-string", mockidp.Config{RoleValue: []any{AdminRole, 1}}, "missing-role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejected(t, newEnv(t, tc.cfg), tc.reason)
		})
	}
}

func TestMissingRolePage(t *testing.T) {
	e := newEnv(t, mockidp.Config{Roles: []string{"viewer"}})
	_, body := e.get(t, "/admin/")
	if !strings.Contains(body, "Access denied") {
		t.Errorf("missing role page:\n%s", body)
	}
}

// callbackURL runs /admin/login and the IdP and returns the callback URL
// without requesting it.
func (e *env) callbackURL(t *testing.T) *url.URL {
	t.Helper()
	nf := e.noFollow()
	resp, err := nf.Get(e.ui.URL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	resp, err = nf.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || u.Query().Get("code") == "" {
		t.Fatalf("IdP redirect %q: no code", resp.Header.Get("Location"))
	}
	return u
}

func TestCallbackRejected(t *testing.T) {
	t.Run("wrong state", func(t *testing.T) {
		e := newEnv(t, admins())
		u := e.callbackURL(t)
		q := u.Query()
		q.Set("state", "not-the-state")
		u.RawQuery = q.Encode()
		resp, err := e.client.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || e.cookie(sessionCookie) != nil {
			t.Errorf("callback with wrong state = %d, session %v; want 403 and none", resp.StatusCode, e.cookie(sessionCookie))
		}
		if !strings.Contains(e.logs.String(), `"reason":"state-mismatch"`) {
			t.Errorf("log lacks state-mismatch:\n%s", e.logs.String())
		}
		e.checkLogs(t)
	})
	t.Run("no login cookie", func(t *testing.T) {
		e := newEnv(t, admins())
		u := e.callbackURL(t)
		other := newEnv(t, admins()) // a browser that did not start this login
		resp, err := other.client.Get(strings.Replace(u.String(), e.ui.URL, other.ui.URL, 1))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || other.cookie(sessionCookie) != nil {
			t.Errorf("callback without login cookie = %d; want 403 and no session", resp.StatusCode)
		}
	})
	t.Run("code used twice", func(t *testing.T) {
		e := newEnv(t, admins())
		u := e.callbackURL(t)
		resp, err := e.noFollow().Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("first callback = %d, want 303", resp.StatusCode)
		}
		// Replay with a fresh login cookie: the IdP refuses the used code.
		u2 := e.callbackURL(t)
		q := u2.Query()
		q.Set("code", u.Query().Get("code"))
		u2.RawQuery = q.Encode()
		resp, err = e.noFollow().Get(u2.String())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("replayed code = %d, want 403", resp.StatusCode)
		}
		if !strings.Contains(e.logs.String(), `"oauth_error":"invalid_grant"`) {
			t.Errorf("log lacks invalid_grant:\n%s", e.logs.String())
		}
		e.checkLogs(t)
	})
}

func TestTamperedSession(t *testing.T) {
	other := signer{key: []byte("FIXTURE-NOT-A-SECRET-other-key-000000000000000000")}
	forged, _ := other.encode(sessionCookie, Session{Subject: testSubject, CSRF: "x", Expires: time.Now().Add(time.Hour).Unix()})
	for _, tc := range []struct {
		name   string
		mutate func(string) string
	}{
		{"payload changed", func(v string) string {
			p, s, _ := strings.Cut(v, ".")
			b := []byte(p)
			b[2] ^= 1
			return string(b) + "." + s
		}},
		{"signature changed", func(v string) string {
			p, s, _ := strings.Cut(v, ".")
			b := []byte(s)
			b[0] = map[bool]byte{true: 'A', false: 'B'}[b[0] != 'A']
			return p + "." + string(b)
		}},
		{"no signature", func(v string) string { p, _, _ := strings.Cut(v, "."); return p }},
		{"signed with another key", func(string) string { return forged }},
		{"login cookie as session", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, admins())
			e.login(t)
			c := e.cookie(sessionCookie)
			v := tc.mutate(c.Value)
			if v == "" {
				v, _ = e.uiSigner().encode(loginCookie, Session{Subject: testSubject, CSRF: "x", Expires: time.Now().Add(time.Hour).Unix()})
			}
			req, _ := http.NewRequest(http.MethodGet, e.ui.URL+"/admin/", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
			resp, err := e.noFollow().Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/login" {
				t.Errorf("GET /admin/ with tampered session = %d %q, want 302 to /admin/login",
					resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

func (e *env) uiSigner() signer { return signer{key: []byte(testSessionKey)} }

func TestExpiredSession(t *testing.T) {
	s := signer{key: []byte(testSessionKey)}
	v, _ := s.encode(sessionCookie, Session{Subject: testSubject, CSRF: "x", Expires: time.Now().Add(-time.Second).Unix()})
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
	if _, err := s.readSession(r, time.Now()); err == nil {
		t.Error("expired session accepted")
	}
}

func TestCSRFAndLogout(t *testing.T) {
	e := newEnv(t, admins())
	csrf := e.login(t)
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"no token", url.Values{}},
		{"wrong token", url.Values{csrfField: {"not-the-token"}}},
		{"empty token", url.Values{csrfField: {""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := e.post(t, "/admin/logout", tc.form); resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST /admin/logout = %d, want 403", resp.StatusCode)
			}
			if e.cookie(sessionCookie) == nil {
				t.Error("session gone after a rejected POST")
			}
		})
	}

	resp := e.post(t, "/admin/logout", url.Values{csrfField: {csrf}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/logout with token = %d, want 200", resp.StatusCode)
	}
	if e.cookie(sessionCookie) != nil {
		t.Error("session cookie still set after logout")
	}
	resp, err := e.noFollow().Get(e.ui.URL + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("GET /admin/ after logout = %d, want 302 to login", resp.StatusCode)
	}
	// A POST without a session is 403, not a redirect.
	if resp := e.post(t, "/admin/logout", url.Values{csrfField: {csrf}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without session = %d, want 403", resp.StatusCode)
	}
	e.checkLogs(t)
}

func TestIdPDown(t *testing.T) {
	e := newEnv(t, admins())
	e.idp.Close()
	resp, _ := e.get(t, "/admin/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("login with the IdP down = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(e.logs.String(), `"class":"unreachable"`) {
		t.Errorf("log lacks class unreachable:\n%s", e.logs.String())
	}
	e.checkLogs(t)
}

// TestDiscoveryErrorNotLogged: go-oidc puts the body of a non-200 discovery
// answer into its error; only the class may reach the log.
func TestDiscoveryErrorNotLogged(t *testing.T) {
	const body = "upstream-body-must-not-be-logged"
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, http.StatusInternalServerError)
	}))
	t.Cleanup(idp.Close)
	logs := &bytes.Buffer{}
	u, err := New(Config{PublicBaseURL: "https://recap.test", Issuer: idp.URL, ClientID: testClientID,
		ClientSecret: testClientSecret, RoleClaim: "roles", SessionKey: []byte(testSessionKey),
		Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("login = %d, want 502", rec.Code)
	}
	if strings.Contains(logs.String(), body) || !strings.Contains(logs.String(), `"class":"bad-response"`) {
		t.Errorf("log must hold the class only:\n%s", logs.String())
	}
}

func TestNewChecksConfig(t *testing.T) {
	ok := Config{PublicBaseURL: "https://recap.test", Issuer: "https://idp.recap.test", ClientID: "c",
		ClientSecret: "s", RoleClaim: "roles", SessionKey: []byte(testSessionKey)}
	if _, err := New(ok); err != nil {
		t.Fatalf("New(valid) = %v", err)
	}
	short := ok
	short.SessionKey = []byte("short")
	noClient := ok
	noClient.ClientID = ""
	for _, c := range []Config{short, noClient} {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v): want error", c)
		}
	}
}
