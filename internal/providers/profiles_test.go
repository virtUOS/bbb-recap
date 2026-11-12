package providers

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/logging"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/providers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoadFixture(t *testing.T) {
	s, err := Load("testdata/providers.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	asr, err := s.Get(KindASR, "")
	if err != nil {
		t.Fatal(err)
	}
	if asr.Name != "whisperx-lab" || asr.MaxInflight != 2 || asr.MaxFileBytes != nil ||
		asr.Token() != "FIXTURE-NOT-A-SECRET-providers-unit" || asr.Declaration.DataCategories[0] != "voice" {
		t.Errorf("asr profile = %+v", asr)
	}
	llm, err := s.Get(KindLLM, "")
	if err != nil {
		t.Fatal(err)
	}
	if llm.MaxInflight != 1 || llm.StructuredOutput != "json_object" || llm.ContextTokens != 32768 ||
		llm.Token() != "" || llm.ExtraBody["chat_template_kwargs"] == nil {
		t.Errorf("llm profile defaults = %+v", llm)
	}
	if _, err := s.Get(KindLLM, "whisperx-lab"); err == nil {
		t.Error("Get(llm, whisperx-lab): want a kind error")
	}
}

// Every problem is reported in one pass, each naming its key.
func TestParseErrors(t *testing.T) {
	text := fixture(t)
	text = strings.Replace(text, "    model: large-v3\n", "    model: large-v3\n    fixture_unknown: 1\n", 1)
	text = strings.Replace(text, "      retention: \"No persistence; RESULT_STORE__ENABLED=false\"\n", "", 1)
	text = strings.Replace(text, "base_url: http://llm.recap.test/v1", "base_url: ftp://llm.recap.test", 1)
	text = strings.Replace(text, "max_output_tokens: 4096", "max_output_tokens: 40960", 1)
	text = strings.Replace(text, "credentials_file: testdata/asr.key", "credentials_file: testdata/missing.key", 1)
	text = strings.Replace(text, "dialect: openai-chat", "dialect: whisperx-family", 1)
	text = strings.Replace(text, "location: on_prem", "location: moon", 1)
	text = strings.Replace(text, "max_inflight: 2", "max_inflight: 0", 1)
	_, err := Parse([]byte(text))
	if err == nil {
		t.Fatal("Parse: want errors")
	}
	for _, want := range []string{
		"profiles[0].fixture_unknown: unknown key",
		"profiles[0].declaration.retention: required",
		"profiles[1].base_url:",
		"profiles[1].max_output_tokens: must be below context_tokens",
		"profiles[0].credentials_file: cannot read file",
		"profiles[1].dialect: unknown llm dialect",
		"profiles[0].declaration.location:",
		"profiles[0].max_inflight: want a positive integer",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "FIXTURE-NOT-A-SECRET") {
		t.Error("error contains a credential")
	}
}

func TestParseTopLevel(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"empty", "", "empty file"},
		{"not a mapping", "- 1\n", "file: want a mapping"},
		{"missing everything", "x: 1\n", "x: unknown key"},
		{"bad version", "schema_version: \"2.0.0\"\ndefaults: {asr: a, llm: b}\nprofiles: []\n", "schema_version: want 1.x.y"},
		{"defaults without profiles", "schema_version: \"1.0.0\"\ndefaults: {asr: a, llm: b}\nprofiles: []\n",
			"defaults.asr: no asr profile named \"a\""},
		{"defaults missing llm", "schema_version: \"1.0.0\"\ndefaults: {asr: a}\nprofiles: []\n", "defaults.llm: required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.text))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestDuplicateName(t *testing.T) {
	text := strings.Replace(fixture(t), "name: vllm-lab", "name: whisperx-lab", 1)
	if _, err := Parse([]byte(text)); err == nil || !strings.Contains(err.Error(), "profiles[1].name: duplicate") {
		t.Fatalf("err = %v", err)
	}
}

// privacy.md §7: third-country and training-using profiles warn at start;
// the warning never carries the credential.
func TestLogWarnings(t *testing.T) {
	s, err := Load("testdata/providers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s.LogWarnings(logging.New(&buf, 0))
	out := buf.String()
	if strings.Count(out, `"level":"WARN"`) != 2 || !strings.Contains(out, `"profile":"vllm-lab"`) {
		t.Errorf("warnings:\n%s", out)
	}
	if strings.Contains(out, "whisperx-lab") || strings.Contains(out, "FIXTURE-NOT-A-SECRET") {
		t.Errorf("unexpected warning content:\n%s", out)
	}
}

// Every known ASR dialect passes the check (providers.md §2, §2a).
func TestASRDialects(t *testing.T) {
	for _, d := range []string{"whisperx-family", "transkribierbar"} {
		text := strings.Replace(fixture(t), "dialect: whisperx-family", "dialect: "+d, 1)
		s, err := Parse([]byte(text))
		if err != nil {
			t.Fatalf("dialect %s: %v", d, err)
		}
		if p, err := s.Get(KindASR, ""); err != nil || p.Dialect != d {
			t.Errorf("dialect %s: profile %+v, %v", d, p, err)
		}
	}
}

// privacy.md §7: a profile without a declaration loads and logs one WARN
// naming the profile and its kind; nothing else is logged for it.
func TestNoDeclaration(t *testing.T) {
	s, err := Load("testdata/providers-no-declaration.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, p := range s.Profiles {
		if p.Declaration != nil {
			t.Errorf("profile %s: Declaration = %+v, want nil", p.Name, p.Declaration)
		}
	}
	var buf bytes.Buffer
	s.LogWarnings(logging.New(&buf, 0))
	out := buf.String()
	if n := strings.Count(out, `"level":"WARN"`); n != 2 {
		t.Errorf("%d WARN lines, want 2:\n%s", n, out)
	}
	for _, want := range []string{`"profile":"whisperx-lab","kind":"asr"`, `"profile":"vllm-lab","kind":"llm"`} {
		if strings.Count(out, want) != 1 {
			t.Errorf("want one warning with %s:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "has no declaration") || strings.Contains(out, "FIXTURE-NOT-A-SECRET") {
		t.Errorf("unexpected warning content:\n%s", out)
	}
}

// A declaration that is present is still checked completely.
func TestIncompleteDeclaration(t *testing.T) {
	b, err := os.ReadFile("testdata/providers-no-declaration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(b), "    model: example-model\n",
		"    model: example-model\n    declaration: {operator: o, location: on_prem}\n", 1)
	_, err = Parse([]byte(text))
	if err == nil {
		t.Fatal("Parse: want errors")
	}
	for _, want := range []string{"profiles[1].declaration.retention: required",
		"profiles[1].declaration.data_categories: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}
