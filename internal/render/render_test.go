package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
)

const (
	formatsDir = "../../formats"
	renderDir  = "../../testdata/render"
)

func readFile(t *testing.T, elem ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(elem...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// input builds the render input of a testdata/render case: context.json
// (polls counted from responses), transcript.json and summary.json.
func input(t *testing.T, dir, transcriptDir string) Input {
	t.Helper()
	f, err := format.Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	var ctx struct {
		MeetingName string `json:"meeting_name"`
		LLMModel    string `json:"llm_model"`
		GeneratedAt string `json:"generated_at"`
		Polls       []struct {
			Question  string   `json:"question"`
			Options   []string `json:"options"`
			Responses []struct {
				Option int `json:"option"`
			} `json:"responses"`
		} `json:"polls"`
	}
	decode(t, readFile(t, renderDir, dir, "context.json"), &ctx)
	var tr assemble.Transcript
	decode(t, readFile(t, renderDir, transcriptDir, "transcript.json"), &tr)
	var sum summarize.Summary
	decode(t, readFile(t, renderDir, dir, "summary.json"), &sum)
	gen, err := time.Parse(time.RFC3339, ctx.GeneratedAt)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{MeetingName: ctx.MeetingName, Transcript: &tr, Summary: &sum, LLMModel: ctx.LLMModel,
		GeneratedAt: gen, Format: f, Language: "en", Zone: time.UTC}
	for _, p := range ctx.Polls {
		poll := meeting.Poll{Question: p.Question}
		for _, o := range p.Options {
			poll.Options = append(poll.Options, meeting.PollOption{Text: o})
		}
		for _, r := range p.Responses {
			poll.Options[r.Option].Votes++
		}
		in.Polls = append(in.Polls, poll)
	}
	return in
}

func TestGoldens(t *testing.T) {
	for _, c := range []struct{ dir, transcript, lang, md, vtt string }{
		{"F-01", "F-01", "en", "summary.md", "transcript.vtt"},
		{"F-01", "F-01", "de", "summary.de.md", "transcript.de.vtt"},
		{"gap", "gap", "en", "summary.md", "transcript.vtt"},
		{"gap", "gap", "de", "summary.de.md", "transcript.de.vtt"},
		{"gap", "mapping", "en", "", "transcript.vtt"},
	} {
		t.Run(c.transcript+"/"+c.lang, func(t *testing.T) {
			in := input(t, c.dir, c.transcript)
			in.Language = c.lang
			out, err := Render(in)
			if err != nil {
				t.Fatal(err)
			}
			if c.md != "" {
				if want := readFile(t, renderDir, c.dir, c.md); string(out.SummaryMD) != string(want) {
					t.Errorf("%s/%s differs:\n%s", c.dir, c.md, out.SummaryMD)
				}
			}
			if want := readFile(t, renderDir, c.transcript, c.vtt); string(out.TranscriptVTT) != string(want) {
				t.Errorf("%s/%s differs:\n%s", c.transcript, c.vtt, out.TranscriptVTT)
			}
			if out.Title != "Fixture Widget Meeting 2025-10-29" {
				t.Errorf("title %q", out.Title)
			}
		})
	}
}

func TestNoSpeech(t *testing.T) {
	in := input(t, "F-01", "F-01")
	in.Summary, in.LLMModel = nil, ""
	in.Transcript.Segments = nil
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	md := string(out.SummaryMD)
	for _, want := range []string{"\n\nNo speech was recognized in the recorded parts of this meeting.\n\n", "LLM: none ·",
		"## Polls"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary.md lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Overview") {
		t.Errorf("no-speech summary.md has LLM sections:\n%s", md)
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"# x": `\# x`, "[a](b)": `\[a\](b)`, "- [x] done": `\- \[x\] done`, "> q": `\> q`, "= e": `\= e`,
		"12. twelve": `12\. twelve`, "x 1. y - z": "x 1. y - z", "a\r\nb\rc\nd": "a b c d", "a | b": `a \| b`,
	} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitle(t *testing.T) {
	origin := time.Date(2025, 10, 29, 23, 30, 0, 0, time.UTC)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		zone *time.Location
		want string
	}{
		{"A\tB\n\n  C\aD", time.UTC, "A B C D 2025-10-29"},
		{"Late", berlin, "Late 2025-10-30"},
		{strings.Repeat("Ü", 250), time.UTC, strings.Repeat("Ü", 188) + "… 2025-10-29"},
		{strings.Repeat("a", 189), time.UTC, strings.Repeat("a", 189) + " 2025-10-29"},
	} {
		if got := Title(c.name, origin, c.zone); got != c.want {
			t.Errorf("Title(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStringsComplete(t *testing.T) {
	f, err := format.Load(formatsDir, "meeting-summary")
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"en", "de"} {
		if _, err := Strings(f, lang); err != nil {
			t.Errorf("%s: %v", lang, err)
		}
	}
	if _, err := Strings(f, "fr"); err != ErrStrings {
		t.Errorf("fr: err %v, want ErrStrings", err)
	}
}
