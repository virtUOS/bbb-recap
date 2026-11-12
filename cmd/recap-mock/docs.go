package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// docsCreatePath is the only Docs endpoint (docs/reference/sinks.md §3.2).
const docsCreatePath = "/api/v1.0/documents/create-for-owner/"

// docsUser is a user of testdata/sink/docs-users.json (fixtures.md §5).
type docsUser struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// mountDocs serves the mock Docs create-for-owner as contracts/sink/README.md
// "Mock Docs" describes it (sinks.md §3, §4): bearer key check (-docs-key,
// default <fixtures>/secrets/docs-s2s.key), sub and email required, sub at
// most 255 characters, owner matching on the fixture users (exact sub, else
// case-insensitive email), an invitation for an unknown owner, 400
// {"email": [...]} for two users with the email, else 201 {"id"}. Nothing
// is stored. It logs the status and the document ID only.
func mountDocs(mux *http.ServeMux, c config) error {
	keyFile := c.DocsKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(c.Fixtures, "secrets", "docs-s2s.key")
	}
	key, err := readKey(keyFile)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(c.Fixtures, "sink", "docs-users.json"))
	if err != nil {
		return err
	}
	var f struct {
		Users []docsUser `json:"users"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	mux.HandleFunc("POST "+docsCreatePath, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status, body, id, match := docsCreate(r, key, f.Users)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		c.Log.Info("mock docs request", "status", status, "document_id", id, "owner", match,
			"latency_ms", time.Since(start).Milliseconds())
	})
	return nil
}

// docsCreate answers one create-for-owner request. match is "access",
// "invitation" or "" (no document).
func docsCreate(r *http.Request, key string, users []docsUser) (status int, body, id, match string) {
	if r.Header.Get("Authorization") != "Bearer "+key {
		return http.StatusUnauthorized, `{"detail": "mock-docs: invalid server-to-server token"}`, "", ""
	}
	var in map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 21<<20)).Decode(&in); err != nil || in == nil {
		return http.StatusBadRequest, `{"detail": "mock-docs: body is not a JSON object"}`, "", ""
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	for _, k := range []string{"sub", "email"} {
		if str(k) == "" {
			return http.StatusBadRequest, fmt.Sprintf(`{%q: ["mock-docs: this field is required"]}`, k), "", ""
		}
	}
	if len([]rune(str("sub"))) > 255 {
		return http.StatusBadRequest, `{"sub": ["mock-docs: at most 255 characters"]}`, "", ""
	}
	match = "invitation"
	switch n := matchOwner(users, str("sub"), str("email")); {
	case n > 1:
		return http.StatusBadRequest, `{"email": ["mock-docs: two active users share this email"]}`, "", ""
	case n == 1:
		match = "access"
	}
	id = newDocumentID()
	return http.StatusCreated, fmt.Sprintf(`{"id": %q}`, id), id, match
}

// matchOwner returns 1 for an exact sub match, else the number of users
// whose email matches case-insensitively, as Docs does
// (suitenumerique/docs src/backend/core/models.py:118-133 @ 6c730f7).
func matchOwner(users []docsUser, sub, email string) int {
	n := 0
	for _, u := range users {
		if u.Sub == sub {
			return 1
		}
		if strings.EqualFold(u.Email, email) {
			n++
		}
	}
	return n
}

// newDocumentID returns a random UUIDv4.
func newDocumentID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
