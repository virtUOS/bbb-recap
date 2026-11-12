package main

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/logging"
)

func asrServer(t *testing.T, keyFile string, logs io.Writer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	c := config{Fixtures: "../../testdata", ASRKeyFile: keyFile, Log: logging.New(logs, slog.LevelDebug)}
	if err := mountASR(mux, c); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, auth string, withFile bool) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "large-v3")
	if withFile {
		w, _ := mw.CreateFormFile("file", "trk-1.webm")
		_, _ = w.Write([]byte("audio"))
	}
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Request-ID", "0190f5a0-0000-7000-8000-000000000001:trk-1:1")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestMockASR(t *testing.T) {
	key := filepath.Join(t.TempDir(), "asr.key")
	if err := os.WriteFile(key, []byte("FIXTURE-NOT-A-SECRET-mock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	srv := asrServer(t, key, &logs)
	want, _ := os.ReadFile("../../testdata/providers/whisperx/list-form.spec-derived.json")

	if status, b := post(t, srv.URL, "Bearer FIXTURE-NOT-A-SECRET-mock", true); status != 200 || !bytes.Equal(b, want) {
		t.Errorf("authorized: %d %s", status, b)
	}
	if status, _ := post(t, srv.URL, "Bearer wrong", true); status != http.StatusUnauthorized {
		t.Errorf("wrong key: %d", status)
	}
	if status, _ := post(t, srv.URL, "Bearer FIXTURE-NOT-A-SECRET-mock", false); status != http.StatusUnprocessableEntity {
		t.Errorf("no file: %d", status)
	}
	for _, bad := range []string{"FIXTURE-NOT-A-SECRET", "Bearer", "widget"} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("mock log contains %q", bad)
		}
	}
	if !strings.Contains(logs.String(), `"request_id":"0190f5a0-0000-7000-8000-000000000001:trk-1:1"`) {
		t.Errorf("mock log:\n%s", logs.String())
	}
}

func TestMockFlags(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{}, &stderr); code != 2 {
		t.Errorf("no -fixtures: exit %d, want 2", code)
	}
	if code := run([]string{"-fixtures", "/no/such/dir", "-listen", "127.0.0.1:0"}, &stderr); code != 2 {
		t.Errorf("missing fixtures: exit %d, want 2", code)
	}
}
