package sink

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// createPath is the only endpoint the sink calls (sinks.md §3.2;
// suitenumerique/docs src/backend/core/api/viewsets.py:972-995 @ 6c730f7).
const createPath = "/api/v1.0/documents/create-for-owner/"

// Error bodies of the mock. The shapes {"<field>": ["<message>"]} follow
// suitenumerique/docs src/backend/core/api/serializers.py @ 6c730f7 (content:
// :496-531; email: :485-486). The message texts are the mock's own (not
// checked against a real Docs) and serve as canaries: no log line may contain
// them. The {"detail": "<message>"} shape of the other errors (401, 403, 429,
// 5xx, 404) was not checked against Docs. No test depends on it: the sink
// classifies by status code (sinks.md §1.6).
const (
	bodyBadContent  = `{"content": ["Could not convert content"]}`
	bodyDupEmail    = `{"email": ["mock-docs: two active users share this email"]}`
	bodyBadToken    = `{"detail": "mock-docs: invalid server-to-server token"}`
	bodyForbidden   = `{"detail": "mock-docs: forbidden"}`
	bodyThrottled   = `{"detail": "mock-docs: request was throttled"}`
	bodyServerError = `{"detail": "mock-docs: internal server error"}`
	bodyNotFound    = `{"detail": "mock-docs: not found"}`
)

// responseCanaries are texts from mock response bodies (sinks.md §4: logs
// carry no response body).
var responseCanaries = []string{
	"Could not convert content",
	"two active users share this email",
	"invalid server-to-server token",
	"mock-docs: forbidden",
	"request was throttled",
	"mock-docs: internal server error",
}

// A fault replaces the normal answer of the mock for one request.
type fault struct {
	status int    // answer with this status and body, nothing is created
	body   string //
	// hangAfterCreate: create the document, then answer only after hold or
	// when the client gives up (a timeout after create, sinks.md §4).
	hangAfterCreate bool
	// dropAfterCreate: create the document, then close the connection
	// without an answer (connection lost after sending).
	dropAfterCreate bool
}

func statusFault(status int, body string) fault { return fault{status: status, body: body} }

// docsRequest is one request the mock received.
type docsRequest struct {
	method, path, query string
	header              http.Header
	body                []byte
}

// document is a document the mock created. Exactly one of ownerSub (an
// owner access for a matched user) and invitedEmail (an owner invitation)
// is set (sinks.md §3.3).
type document struct {
	id, title, content     string
	ownerSub, invitedEmail string
}

type docsUser struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// mockDocs serves create-for-owner as sinks.md §3 cites it: bearer key check,
// the eight body fields, 201 {"id"}, sub-then-email matching on the fixture
// users, an invitation for unknown emails, 400 for a duplicate email, and
// faults queued by the test.
type mockDocs struct {
	key   string
	users []docsUser
	hold  time.Duration // how long hangAfterCreate waits at most

	mu       sync.Mutex
	faults   []fault
	requests []docsRequest
	docs     []document
}

func newMockDocs(t *testing.T) *mockDocs {
	t.Helper()
	var u struct {
		Users []docsUser `json:"users"`
	}
	if err := json.Unmarshal(readFile(t, testdataPath(t, "sink", "docs-users.json")), &u); err != nil || len(u.Users) == 0 {
		t.Fatalf("testdata/sink/docs-users.json: %v", err)
	}
	return &mockDocs{key: fixtureKey(t), users: u.Users, hold: 10 * time.Second}
}

// startTLS serves the mock on https://127.0.0.1:<port> until the test ends.
func (m *mockDocs) startTLS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(m)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// queue adds faults for the next requests, in order.
func (m *mockDocs) queue(f ...fault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = append(m.faults, f...)
}

// reset forgets faults, requests and documents (the service-level mock is
// shared by the tests of this package).
func (m *mockDocs) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults, m.requests, m.docs = nil, nil, nil
}

func (m *mockDocs) received() []docsRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]docsRequest(nil), m.requests...)
}

func (m *mockDocs) documents() []document {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]document(nil), m.docs...)
}

func (m *mockDocs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, docsRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
	var f fault
	if len(m.faults) > 0 {
		f, m.faults = m.faults[0], m.faults[1:]
	}
	m.mu.Unlock()

	if r.URL.Path != createPath || r.Method != http.MethodPost {
		answer(w, http.StatusNotFound, bodyNotFound)
		return
	}
	if f.status != 0 {
		answer(w, f.status, f.body)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+m.key {
		answer(w, http.StatusUnauthorized, bodyBadToken)
		return
	}
	var in map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&in); err != nil || in == nil {
		answer(w, http.StatusBadRequest, `{"detail": "mock-docs: body is not a JSON object"}`)
		return
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	for _, k := range []string{"sub", "email"} { // required (Docs serializers.py:453-465)
		if str(k) == "" {
			answer(w, http.StatusBadRequest, fmt.Sprintf(`{%q: ["mock-docs: this field is required"]}`, k))
			return
		}
	}
	if len([]rune(str("sub"))) > 255 {
		answer(w, http.StatusBadRequest, `{"sub": ["mock-docs: at most 255 characters"]}`)
		return
	}

	doc := document{id: newUUID(), title: str("title"), content: str("content")}
	switch u, n := m.match(str("sub"), str("email")); {
	case n > 1:
		answer(w, http.StatusBadRequest, bodyDupEmail)
		return
	case n == 1:
		doc.ownerSub = u.Sub
	default:
		doc.invitedEmail = str("email")
	}
	m.mu.Lock()
	m.docs = append(m.docs, doc)
	m.mu.Unlock()

	switch {
	case f.dropAfterCreate:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler) // closes the connection without an answer
	case f.hangAfterCreate:
		select {
		case <-r.Context().Done():
			return
		case <-time.After(m.hold):
		}
	}
	answer(w, http.StatusCreated, fmt.Sprintf(`{"id": %q}`, doc.id))
}

// match finds the owner as Docs does (suitenumerique/docs
// src/backend/core/models.py:118-133 @ 6c730f7): exact sub first, else case-insensitive email. It returns the
// number of users matched by email when the sub did not match.
func (m *mockDocs) match(sub, email string) (docsUser, int) {
	for _, u := range m.users {
		if u.Sub == sub {
			return u, 1
		}
	}
	var found []docsUser
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			found = append(found, u)
		}
	}
	if len(found) == 1 {
		return found[0], 1
	}
	return docsUser{}, len(found)
}

func answer(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// closedURL returns an https URL on 127.0.0.1 where nothing listens, so a
// connection is refused before anything is sent.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "https://" + addr
}
