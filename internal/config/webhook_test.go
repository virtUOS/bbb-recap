package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Webhook and Matrix sink profiles (sinks.md §4b.1, §4b.4, §4c; ADR-016).

// fixtureWebhookSecret is whsec_ + base64 of 40 synthetic bytes (fixtures.md).
var fixtureWebhookSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("FIXTURE-NOT-A-SECRET-config-webhook-0001"))

const webhookProfile = `schema_version: "1.0.0"
sinks:
  - name: webhook-main
    type: webhook
    url: https://hooks.recap.test/recap?channel=x
    signing_secret_file: webhook.key
    content: notice
    request_timeout: 20s
    declaration:
      data_categories: [title]
      operator: "operator's webhook receiver"
      location: on_prem
      retention: "Receiver's policy"
      deletion: manual
`

// webhookFile writes text as sinks.yaml next to webhook.key with secret.
func webhookFile(t *testing.T, text, secret string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"sinks.yaml": text, "webhook.key": secret} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "sinks.yaml")
}

func TestLoadWebhookSink(t *testing.T) {
	old := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 24))
	sinks, err := LoadSinks(webhookFile(t, webhookProfile, fixtureWebhookSecret+"\n"+old+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := sinks[0]
	if s.URL != "https://hooks.recap.test/recap?channel=x" || s.Content != WebhookContentNotice ||
		s.Timeout != 20*time.Second || len(s.SigningKeys) != 2 || len(s.SigningKeys[1]) != 24 ||
		s.MaxInflight != DefaultSinkMaxInflight || s.MaxDocumentBytes != DefaultSinkMaxDocumentBytes {
		t.Errorf("profile = %+v", s)
	}
	if string(s.SigningKeys[0]) != "FIXTURE-NOT-A-SECRET-config-webhook-0001" {
		t.Error("first key is not the decoded first line")
	}
}

func TestLoadWebhookDefaults(t *testing.T) {
	text := strings.NewReplacer("    content: notice\n", "", "    request_timeout: 20s\n", "").Replace(webhookProfile)
	sinks, err := LoadSinks(webhookFile(t, text, fixtureWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if s := sinks[0]; s.Content != WebhookContentNotice || s.Timeout != DefaultWebhookRequestTimeout {
		t.Errorf("defaults not applied: %+v", s)
	}
}

// check-config names each bad webhook key and never quotes the URL or a
// secret (sinks.md §4b.1, §4b.4).
func TestLoadWebhookBadKeys(t *testing.T) {
	short := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 23))
	long := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 65))
	for _, c := range []struct {
		name, from, to, secret, want string
	}{
		{"url-relative", "https://hooks.recap.test/recap?channel=x", "/recap", "", "sinks[0].url"},
		{"url-ftp", "https://hooks.recap.test/recap?channel=x", "ftp://hooks.recap.test/x", "", "sinks[0].url"},
		{"url-user-info", "https://hooks.recap.test/recap?channel=x", "https://u:p@hooks.recap.test/x", "", "sinks[0].url"},
		{"content", "content: notice", "content: everything", "", "sinks[0].content"},
		{"no-secret-file", "    signing_secret_file: webhook.key\n", "", "", "sinks[0].signing_secret_file: required"},
		{"secret-no-prefix", "", "", "c2VjcmV0", "sinks[0].signing_secret_file: file webhook.key: line 1"},
		{"secret-not-base64", "", "", "whsec_!!!", "not valid base64"},
		{"secret-too-short", "", "", short, "24 to 64 bytes"},
		{"secret-too-long", "", "", long, "24 to 64 bytes"},
		{"three-secrets", "", "", strings.Repeat(fixtureWebhookSecret+"\n", 3), "one or two secret lines"},
		{"empty-secret", "", "", "\n", "one or two secret lines"},
		{"docs-key", "    content: notice\n", "    content: notice\n    api_key_file: x.key\n", "", "sinks[0].api_key_file: not allowed"},
		{"matrix-key", "    content: notice\n", "    content: notice\n    homeserver_url: https://m.test\n", "", "sinks[0].homeserver_url: not allowed"},
		{"unknown-key", "    declaration:", "    x_declaration:", "", "field x_declaration"},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := webhookProfile
			if c.from != "" {
				text = strings.Replace(text, c.from, c.to, 1)
			}
			secret := fixtureWebhookSecret
			if c.secret != "" {
				secret = c.secret
			}
			_, err := LoadSinks(webhookFile(t, text, secret))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
			for _, v := range []string{"FIXTURE-NOT-A-SECRET", "channel=x", strings.TrimPrefix(fixtureWebhookSecret, "whsec_")} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error quotes %q", v)
				}
			}
		})
	}
}

// A webhook profile needs a declaration (privacy.md §7, §11).
func TestLoadWebhookDeclarationRequired(t *testing.T) {
	text := webhookProfile[:strings.Index(webhookProfile, "    declaration:")]
	_, err := LoadSinks(webhookFile(t, text, fixtureWebhookSecret))
	if err == nil || !strings.Contains(err.Error(), "sinks[0].declaration: required for type webhook") {
		t.Fatalf("got %v", err)
	}
}

// The secret file is read for the worker role only.
func TestWebhookSecretReadByWorkerOnly(t *testing.T) {
	path := webhookFile(t, webhookProfile, fixtureWebhookSecret)
	if err := os.Remove(filepath.Join(filepath.Dir(path), "webhook.key")); err != nil {
		t.Fatal(err)
	}
	l := &loader{env: map[string]string{"RECAP_SINKS_FILE": path}}
	loadSinks(l, &Config{files: map[string][]byte{}}, "RECAP_SINKS_FILE", false)
	if len(l.errs) != 0 {
		t.Errorf("api role: %v", l.errs)
	}
	if _, err := LoadSinks(path); err == nil || !strings.Contains(err.Error(), "signing_secret_file: cannot read file") {
		t.Errorf("worker role: got %v", err)
	}
}

// A matrix profile is not supported yet: the configuration is refused with
// a clear message.
func TestLoadMatrixSinkNotSupported(t *testing.T) {
	text := `schema_version: "1.0.0"
sinks:
  - name: matrix-main
    type: matrix
    homeserver_url: https://matrix.recap.test
    access_token_file: matrix-bot.token
    declaration:
      data_categories: [title]
      operator: "operator's Matrix homeserver"
      location: on_prem
      retention: "Room history"
      deletion: manual
`
	_, err := LoadSinks(webhookFile(t, text, ""))
	if err == nil || !strings.Contains(err.Error(), "sinks[0].type: matrix is not supported yet") {
		t.Fatalf("got %v", err)
	}
}

func TestLogWebhookWarnings(t *testing.T) {
	text := strings.Replace(webhookProfile, "https://hooks.recap.test/recap?channel=x", "http://hooks.recap.test/recap?channel=x", 1)
	sinks, err := LoadSinks(webhookFile(t, text, fixtureWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	LogSinkWarnings(slog.New(slog.NewJSONHandler(&buf, nil)), sinks)
	if strings.Count(buf.String(), `"level":"WARN"`) != 1 || strings.Contains(buf.String(), "hooks.recap.test") {
		t.Errorf("warnings: %s", buf.String())
	}
}
