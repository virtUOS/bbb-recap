package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/logging"
)

func TestMockDocs(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	c := config{Fixtures: "../../testdata", Log: logging.New(&logs, slog.LevelDebug)}
	if err := mountDocs(mux, c); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, err := readKey("../../testdata/secrets/docs-s2s.key")
	if err != nil {
		t.Fatal(err)
	}
	post := func(auth string, body map[string]string) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+docsCreatePath, bytes.NewReader(b))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	doc := func(sub, email string) map[string]string {
		return map[string]string{"title": "Fixture Widget Meeting", "content": "# x\n", "sub": sub, "email": email}
	}
	for _, c := range []struct {
		name   string
		auth   string
		body   map[string]string
		status int
		want   string
	}{
		{"sub match", "Bearer " + key, doc("fixture-sub-ada-0001", "other.testperson@example.org"), 201, `"id"`},
		{"email match", "Bearer " + key, doc("fixture-sub-unknown-0009", "ADA.testperson@example.org"), 201, `"id"`},
		{"invitation", "Bearer " + key, doc("fixture-sub-unknown-0009", "nobody.testperson@example.org"), 201, `"id"`},
		{"duplicate email", "Bearer " + key, doc("fixture-sub-unknown-0009", "dup.testperson@example.org"), 400, `"email"`},
		{"sub of a dup user", "Bearer " + key, doc("fixture-sub-dup-0007", "dup.testperson@example.org"), 201, `"id"`},
		{"wrong key", "Bearer FIXTURE-NOT-A-SECRET-wrong", doc("fixture-sub-ada-0001", "ada.testperson@example.org"), 401, "detail"},
		{"no sub", "Bearer " + key, doc("", "ada.testperson@example.org"), 400, `"sub"`},
		{"long sub", "Bearer " + key, doc(strings.Repeat("s", 256), "ada.testperson@example.org"), 400, `"sub"`},
	} {
		status, body := post(c.auth, c.body)
		if status != c.status || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s, want %d with %s", c.name, status, body, c.status, c.want)
		}
	}
	for _, s := range []string{"testperson", "fixture-sub", "Fixture Widget", key} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains %q", s)
		}
	}
}
