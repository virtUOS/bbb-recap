package adminui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Cookie names. Both have Path=/admin/.
const (
	sessionCookie = "recap_admin_session"
	loginCookie   = "recap_admin_login"
	cookiePath    = "/admin/"
)

// Lifetimes.
const (
	sessionTTL = 8 * time.Hour
	loginTTL   = 10 * time.Minute
)

// Session is a logged-in admin. It lives only in the signed cookie, so
// every api replica with the same key file accepts it.
type Session struct {
	Subject string `json:"sub"`
	CSRF    string `json:"csrf"`
	Expires int64  `json:"exp"` // Unix seconds
}

// loginState is what the callback checks against the authorization response.
type loginState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"exp"`
}

var errBadCookie = errors.New("adminui: missing, invalid or expired cookie")

// signer signs cookie values: base64url(JSON) "." base64url(HMAC-SHA256).
// The MAC covers the cookie name too, so one cookie cannot stand in for
// the other.
type signer struct{ key []byte }

func (s signer) mac(name, payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(name + "." + payload))
	return m.Sum(nil)
}

func (s signer) encode(name string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(name, payload)), nil
}

// decode verifies the MAC and unmarshals the payload into v.
func (s signer) decode(name, value string, v any) error {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return errBadCookie
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(name, payload)) {
		return errBadCookie
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(b, v) != nil {
		return errBadCookie
	}
	return nil
}

// read returns the verified, unexpired value of cookie name.
func (s signer) read(r *http.Request, name string, v any, expires func() int64, now time.Time) error {
	c, err := r.Cookie(name)
	if err != nil {
		return errBadCookie
	}
	if err := s.decode(name, c.Value, v); err != nil {
		return err
	}
	if now.Unix() >= expires() {
		return errBadCookie
	}
	return nil
}

func (s signer) readSession(r *http.Request, now time.Time) (Session, error) {
	var ss Session
	err := s.read(r, sessionCookie, &ss, func() int64 { return ss.Expires }, now)
	if err == nil && (ss.Subject == "" || ss.CSRF == "") {
		err = errBadCookie
	}
	return ss, err
}

func (s signer) readLogin(r *http.Request, now time.Time) (loginState, error) {
	var ls loginState
	err := s.read(r, loginCookie, &ls, func() int64 { return ls.Expires }, now)
	return ls, err
}

// setCookie writes a Secure, HttpOnly, SameSite=Lax cookie on /admin/.
// maxAge < 0 deletes it.
func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     cookiePath,
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) { setCookie(w, name, "", -1) }

// randomToken returns 32 random bytes, base64url.
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand docs)
	return base64.RawURLEncoding.EncodeToString(b)
}

// equal compares two secrets in constant time.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// flash is the one-line result of an action, shown once by the next page.
// It lives in a signed cookie for one minute, so no value travels in a URL.
type flash struct {
	Action      string  `json:"action"`
	JobID       string  `json:"job_id"`         // the job whose page shows it
	From        string  `json:"from,omitempty"` // rerun: the old job
	DocumentID  *string `json:"document_id,omitempty"`
	SinkProfile *string `json:"sink_profile,omitempty"`
	Expires     int64   `json:"exp"`
}

const (
	flashCookie = "recap_admin_flash"
	flashTTL    = time.Minute
)
