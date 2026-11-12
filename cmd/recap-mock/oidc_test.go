package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oidcTestSecret = "FIXTURE-NOT-A-SECRET-oidc-11111111111111111111111111111111"

// oidcServer runs mountOIDC on a test server with issuer <url>/oidc.
func oidcServer(t *testing.T, logs *bytes.Buffer) (*httptest.Server, string) {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "oidc-client.secret")
	if err := os.WriteFile(secretFile, []byte(oidcTestSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := config{
		OIDCIssuer:           srv.URL + "/oidc",
		OIDCBrowserURL:       "http://localhost:1/oidc",
		OIDCClientID:         "recap-admin-ui",
		OIDCClientSecretFile: secretFile,
		OIDCRedirectURI:      "https://recap.test/admin/callback",
		OIDCSubject:          "fixture-sub-ada-0001",
		OIDCRoles:            "recap-admin, other",
		Log:                  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if err := mountOIDC(mux, c); err != nil {
		t.Fatal(err)
	}
	return srv, c.OIDCIssuer
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestMockOIDC(t *testing.T) {
	var logs bytes.Buffer
	srv, issuer := oidcServer(t, &logs)

	var disc map[string]any
	getJSON(t, issuer+"/.well-known/openid-configuration", &disc)
	if disc["issuer"] != issuer || disc["token_endpoint"] != issuer+"/token" ||
		disc["authorization_endpoint"] != "http://localhost:1/oidc/authorize" {
		t.Errorf("discovery = %v", disc)
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	getJSON(t, issuer+"/jwks", &jwks)
	if len(jwks.Keys) != 1 || jwks.Keys[0]["kty"] != "RSA" || jwks.Keys[0]["alg"] != "RS256" {
		t.Errorf("jwks = %v", jwks)
	}

	verifier := "fixture-verifier-0000000000000000000000000000000000000"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorize := func(q url.Values) *url.URL {
		t.Helper()
		resp, err := noFollow.Get(srv.URL + "/oidc/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("authorize = %d", resp.StatusCode)
		}
		u, _ := url.Parse(resp.Header.Get("Location"))
		return u
	}
	good := url.Values{
		"response_type": {"code"}, "client_id": {"recap-admin-ui"}, "scope": {"openid"},
		"redirect_uri": {"https://recap.test/admin/callback"}, "state": {"st"}, "nonce": {"nn"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	plain := url.Values{}
	for k, v := range good {
		plain[k] = v
	}
	plain.Set("code_challenge_method", "plain")
	if u := authorize(plain); u.Query().Get("error") != "invalid_request" || u.Query().Get("code") != "" {
		t.Errorf("authorize without S256 = %s, want error=invalid_request", u)
	}

	for _, ru := range []string{"https://evil.recap.test/admin/callback", "https://recap.test/admin/callback?x=1", ""} {
		other := url.Values{}
		for k, v := range good {
			other[k] = v
		}
		other.Set("redirect_uri", ru)
		resp, err := noFollow.Get(srv.URL + "/oidc/authorize?" + other.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("authorize with redirect_uri %q = %d (Location %q), want 400 without redirect",
				ru, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	token := func(code, secret, verifier string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, issuer+"/token", strings.NewReader(url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
			"redirect_uri": {"https://recap.test/admin/callback"},
		}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("recap-admin-ui", secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	u := authorize(good)
	if u.Host != "recap.test" || u.Query().Get("state") != "st" || u.Query().Get("code") == "" {
		t.Fatalf("authorize redirect = %s", u)
	}
	code := u.Query().Get("code")
	if status, body := token(code, "wrong", verifier); status != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Errorf("wrong secret = %d %v", status, body)
	}
	if status, body := token(code, oidcTestSecret, "wrong-verifier"); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("wrong verifier = %d %v", status, body)
	}
	// The failed exchange used the code up.
	code = authorize(good).Query().Get("code")
	status, body := token(code, oidcTestSecret, verifier)
	if status != http.StatusOK {
		t.Fatalf("token = %d %v", status, body)
	}
	if status, _ := token(code, oidcTestSecret, verifier); status != http.StatusBadRequest {
		t.Errorf("second use of a code = %d, want 400", status)
	}
	idToken, _ := body["id_token"].(string)
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		t.Fatalf("id_token is not a JWS")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["iss"] != issuer || claims["sub"] != "fixture-sub-ada-0001" || claims["aud"] != "recap-admin-ui" ||
		claims["nonce"] != "nn" {
		t.Errorf("claims = %v", claims)
	}
	if roles, _ := claims["roles"].([]any); len(roles) != 2 || roles[0] != "recap-admin" || roles[1] != "other" {
		t.Errorf("roles = %v", claims["roles"])
	}

	for _, bad := range []string{oidcTestSecret, "fixture-sub-", code, parts[2], "FIXTURE-NOT-A-SECRET"} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("mock log contains %q", bad)
		}
	}
}

func TestMockOIDCOff(t *testing.T) {
	mux := http.NewServeMux()
	if err := mountOIDC(mux, config{}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/oidc/.well-known/openid-configuration", nil)
	if _, pattern := mux.Handler(r); pattern != "" {
		t.Errorf("without -oidc-issuer, %s is mounted", pattern)
	}
	if err := mountOIDC(http.NewServeMux(), config{OIDCIssuer: "http://mock:8080/oidc", OIDCClientID: "c", OIDCSubject: "s",
		OIDCRedirectURI: "https://recap.test/admin/callback"}); err == nil {
		t.Error("issuer without client secret: want error")
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte(oidcTestSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ru := range []string{"", "/admin/callback"} {
		if err := mountOIDC(http.NewServeMux(), config{OIDCIssuer: "http://mock:8080/oidc", OIDCClientID: "c", OIDCSubject: "s",
			OIDCClientSecretFile: secretFile, OIDCRedirectURI: ru}); err == nil {
			t.Errorf("redirect URI %q: want error", ru)
		}
	}
}
