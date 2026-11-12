package render

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// Golden summary.md (templates.md §5): F-01 in en and de (with polls and all
// sections filled) and the gap variant (gap notice, empty sections, no polls).
func TestSummaryGolden(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-02", func(t *testing.T) {
		for _, c := range []struct{ name, dir, language, golden string }{
			{"F-01-en", "F-01", "", "summary.md"},
			{"F-01-de", "F-01", "de", "summary.de.md"},
			{"gap-en", "gap", "", "summary.md"},
			{"gap-de", "gap", "de", "summary.de.md"},
		} {
			t.Run(c.name, func(t *testing.T) {
				in := loadCase(t, c.dir).clone(t)
				want := "en"
				if c.language != "" {
					in.transcript["language"] = c.language
					want = c.language
				}
				res := renderOK(t, bin, run{input: in.input()})
				if res.Language != want {
					t.Errorf("language = %q, want %q", res.Language, want)
				}
				wantBytes(t, c.dir+"/"+c.golden, res.summaryMD, golden(t, c.dir, c.golden))
				// Same input, same bytes.
				again := renderOK(t, bin, run{input: in.input()})
				wantBytes(t, "second run of "+c.dir+"/"+c.golden, again.summaryMD, res.summaryMD)
			})
		}
	})
}

// escapeCase is one string from meeting content or the LLM and its escaped
// form (templates.md §5.2). The form is the same in every position: leading
// characters are escaped at the start of every value, and newlines in any
// value become spaces (CRLF, CR and LF each count as one newline, §5.2).
type escapeCase struct{ in, want string }

var escapeCases = []escapeCase{
	{"# x", `\# x`},
	{"[a](b)", `\[a\](b)`},
	{"<script>alert(1)</script>", `\<script\>alert(1)\</script\>`},
	{"- [x] done", `\- \[x\] done`},
	{"a | b", `a \| b`},
	{"*b* _i_ ~~s~~ `c` \\ end", "\\*b\\* \\_i\\_ \\~\\~s\\~\\~ \\`c\\` \\\\ end"},
	{"+ plus", `\+ plus`},
	{"> quote", `\> quote`},
	{"= equals", `\= equals`},
	{"1. one", `1\. one`},
	{"12. twelve", `12\. twelve`},
	{"x 1. y - z", `x 1. y - z`},
	{"first\nsecond", "first second"},
	{"first\n# not a heading\n- not a list", `first \# not a heading - not a list`},
	{"a\r\nb", "a b"},
	{"a\rb", "a b"},
}

// escapeFields are the escaped values of summary.md, each with its own
// suffix so every field is checked on its own line.
var escapeFields = []string{
	"meeting", "overview", "title", "topic", "decision", "task", "owner", "due", "question", "poll", "option", "speaker",
}

// Markdown escaping: each string renders as literal text in every escaped
// field of summary.md (templates.md §5.2, §5.4): meeting name, overview,
// topic heading and paragraph, decision, action-item task, owner and due,
// open question, poll question and option, gap-notice speaker. Every field
// gets the case string plus its own suffix, and each expected line must
// occur exactly once.
func TestMarkdownEscaping(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-04", func(t *testing.T) {
		base := loadCase(t, "F-01")
		for i, ec := range escapeCases {
			t.Run(fmt.Sprintf("case-%02d", i+1), func(t *testing.T) {
				in, want := map[string]string{}, map[string]string{}
				for _, f := range escapeFields {
					in[f], want[f] = ec.in+" "+f, ec.want+" "+f
				}
				c := base.clone(t)
				c.context["meeting_name"] = in["meeting"]
				c.summary = object{
					"overview":       in["overview"],
					"topics":         []any{object{"title": in["title"], "summary": in["topic"]}},
					"decisions":      []any{object{"text": in["decision"]}},
					"action_items":   []any{object{"task": in["task"], "owner": in["owner"], "due": in["due"]}},
					"open_questions": []any{object{"text": in["question"]}},
				}
				c.context["polls"] = []any{object{
					"question": in["poll"],
					"options":  []any{in["option"], "Other"},
					"responses": []any{
						object{"user_id": "w_fixada0001", "name": "Ada Testperson", "option": 0},
					},
				}}
				// Ada's first track failed: the gap notice names her (§5.4 block 3).
				c.transcript["speakers"].([]any)[0].(object)["display_name"] = in["speaker"]
				c.transcript["tracks"].([]any)[0].(object)["status"] = "failed"
				c.transcript["gaps"] = []any{object{
					"track_ref": "trk-1", "speaker_ref": "spk-1", "start_ms": 200, "end_ms": 55000, "reason": "asr_failed",
				}}
				res := renderOK(t, bin, run{input: c.input()})
				got := lines(res.summaryMD)
				if !strings.HasPrefix(got[0], "*"+want["meeting"]+" · 2025-10-29 · ") {
					t.Errorf("meeting name: header %q does not start with %q", got[0], "*"+want["meeting"]+" · ")
				}
				for where, line := range map[string]string{
					"overview":       want["overview"],
					"topic heading":  "### " + want["title"],
					"topic summary":  want["topic"],
					"decision":       "- " + want["decision"],
					"action item":    "- [ ] " + want["task"] + " — " + want["owner"] + " — due: " + want["due"],
					"open question":  "- " + want["question"],
					"poll question":  "**" + want["poll"] + "**",
					"poll table row": "| " + want["option"] + " | 1 |",
					"gap notice":     "> **Incomplete transcript:** no text for " + want["speaker"] + " from [00:00:00] to [00:00:55] (transcription failed).",
				} {
					if n := count(got, line); n != 1 {
						t.Errorf("%s: line %q occurs %d times, want 1 (input %q)", where, line, n, ec.in)
					}
				}
				if t.Failed() {
					t.Logf("summary.md:\n%s", res.summaryMD)
				}
			})
		}
	})
}

func count(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

// Block types of the F-01 golden as Docs should see them (templates.md §5.2,
// sinks.md §2). The y-provider conversion itself is a manual check (README.md).
var f01Blocks = []string{
	"paragraph", "paragraph", // header, notice
	"h2", "paragraph", // overview
	"h2", "h3", "paragraph", "h3", "paragraph", "h3", "paragraph", // topics
	"h2", "list", // decisions
	"h2", "tasklist", // action items
	"h2", "list", // open questions
	"h2", "paragraph", "table", // polls
	"hr", "paragraph", // footer
}

// classify splits Markdown into blocks and names their types; it reports
// constructs outside the dialect of templates.md §5.2 / sinks.md §2.2.
func classify(md string) (kinds, problems []string) {
	for _, l := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(l, "# "), strings.HasPrefix(l, "####"), strings.HasPrefix(l, "```"),
			strings.HasPrefix(l, "    "), strings.HasPrefix(l, "\t"), strings.HasPrefix(l, "==="):
			problems = append(problems, "line outside the dialect: "+l)
		}
		for _, bad := range []string{"](", "![", "[^", "<http", "~~~"} {
			if strings.Contains(l, bad) {
				problems = append(problems, fmt.Sprintf("%q in line %q", bad, l))
			}
		}
		text := strings.TrimPrefix(l, "> ") // a blockquote marker is allowed
		for i := 0; i < len(text); i++ {
			if (text[i] == '<' || text[i] == '>') && (i == 0 || text[i-1] != '\\') {
				problems = append(problems, "raw < or > in line "+l)
				break
			}
		}
	}
	for _, b := range strings.Split(strings.TrimSuffix(md, "\n"), "\n\n") {
		ls := strings.Split(b, "\n")
		all := func(p string) bool {
			return !slices.ContainsFunc(ls, func(l string) bool { return !strings.HasPrefix(l, p) })
		}
		switch {
		case b == "":
			problems = append(problems, "more than one blank line between blocks")
		case len(ls) == 1 && strings.HasPrefix(b, "### "):
			kinds = append(kinds, "h3")
		case len(ls) == 1 && strings.HasPrefix(b, "## "):
			kinds = append(kinds, "h2")
		case b == "---":
			kinds = append(kinds, "hr")
		case all("- [ ] "):
			kinds = append(kinds, "tasklist")
		case all("- "):
			kinds = append(kinds, "list")
		case all("| ") && len(ls) >= 2 && ls[1] == "| --- | --- |":
			kinds = append(kinds, "table")
		case all("> "):
			kinds = append(kinds, "blockquote")
		case len(ls) == 1 && !strings.ContainsAny(b[:1], "#-+>|=0123456789"):
			kinds = append(kinds, "paragraph")
		default:
			problems = append(problems, "unclassified block: "+b)
		}
	}
	return kinds, problems
}

// Docs dialect of the rendered goldens: only allowed constructs, and the
// expected block types for F-01. Converting through a pinned y-provider
// /convert is the manual check in README.md, so this test ends with a skip.
func TestDocsDialect(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-07", func(t *testing.T) {
		for _, c := range []struct{ dir, language, golden string }{
			{"F-01", "", "summary.md"}, {"F-01", "de", "summary.de.md"}, {"gap", "", "summary.md"}, {"gap", "de", "summary.de.md"},
		} {
			in := loadCase(t, c.dir).clone(t)
			if c.language != "" {
				in.transcript["language"] = c.language
			}
			res := renderOK(t, bin, run{input: in.input()})
			wantBytes(t, c.dir+"/"+c.golden, res.summaryMD, golden(t, c.dir, c.golden))
			kinds, problems := classify(string(res.summaryMD))
			for _, p := range problems {
				t.Errorf("%s/%s: %s", c.dir, c.golden, p)
			}
			if c.dir == "F-01" && !slices.Equal(kinds, f01Blocks) {
				t.Errorf("%s/%s block types:\ngot  %v\nwant %v", c.dir, c.golden, kinds, f01Blocks)
			}
		}
		if t.Failed() {
			return
		}
		t.Skip("y-provider /convert of the goldens is a manual check (contracts/render/README.md); the local checks passed")
	})
}

// Golden transcript.vtt for F-01 (fixtures.md §7.1), byte-identical and
// deterministic; with output language de the NOTE texts come from the de
// string table (templates.md §6, §7): golden transcript.de.vtt.
func TestTranscriptGolden(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-08", func(t *testing.T) {
		want := golden(t, "F-01", "transcript.vtt")
		c := loadCase(t, "F-01")
		first := renderOK(t, bin, run{input: c.input()})
		wantBytes(t, "F-01 transcript.vtt", first.transcript, want)
		second := renderOK(t, bin, run{input: c.input()})
		wantBytes(t, "second run of F-01 transcript.vtt", second.transcript, want)
		de := c.clone(t)
		de.transcript["language"] = "de"
		third := renderOK(t, bin, run{input: de.input()})
		wantBytes(t, "F-01 transcript.de.vtt", third.transcript, golden(t, "F-01", "transcript.de.vtt"))
	})
}

// Title (templates.md §5.1) and output language (§7).
func TestTitleAndLanguage(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-11", func(t *testing.T) {
		base := loadCase(t, "F-01")
		const date = " 2025-10-29"
		titles := []struct {
			name, meeting, origin, tz, want string
		}{
			{"f01", "Fixture Widget Meeting", "", "", "Fixture Widget Meeting" + date},
			{"control-chars", "Fixture\tWidget\n\n  Meeting\aX", "", "", "Fixture Widget Meeting X" + date},
			{"200-chars-kept", strings.Repeat("a", 189), "", "", strings.Repeat("a", 189) + date},
			{"201-chars-cut", strings.Repeat("a", 190), "", "", strings.Repeat("a", 188) + "…" + date},
			{"cut-counts-characters", strings.Repeat("Ü", 250), "", "", strings.Repeat("Ü", 188) + "…" + date},
			{"date-in-display-tz", "Late Meeting", "2025-10-29T23:30:00Z", "Europe/Berlin", "Late Meeting 2025-10-30"},
			{"date-in-utc", "Late Meeting", "2025-10-29T23:30:00Z", "UTC", "Late Meeting 2025-10-29"},
		}
		for _, tc := range titles {
			t.Run("title/"+tc.name, func(t *testing.T) {
				c := base.clone(t)
				c.context["meeting_name"] = tc.meeting
				env := map[string]string{}
				if tc.origin != "" {
					c.transcript["time_base"].(object)["origin_utc"] = tc.origin
				}
				if tc.tz != "" {
					env["RECAP_DISPLAY_TZ"] = tc.tz
				}
				res := renderOK(t, bin, run{input: c.input(), env: env})
				if res.Title != tc.want {
					t.Errorf("title = %q, want %q", res.Title, tc.want)
				}
				if n := utf8.RuneCountInString(res.Title); n > 200 {
					t.Errorf("title has %d characters, want at most 200", n)
				}
				if strings.Contains(string(res.summaryMD), res.Title) {
					// The title is passed to the sink; it is not part of the body (§5.1).
					t.Errorf("summary.md contains the title %q", res.Title)
				}
			})
		}
		languages := []struct {
			name     string
			language any // transcript.language
			fallback string
			want     string
			golden   string
		}{
			{"de", "de", "en", "de", "summary.de.md"},
			{"en", "en", "de", "en", "summary.md"},
			{"unsupported-fr-default-en", "fr", "en", "en", "summary.md"},
			{"unsupported-fr-default-de", "fr", "de", "de", "summary.de.md"},
			{"null-default-de", nil, "de", "de", "summary.de.md"},
		}
		for _, tc := range languages {
			t.Run("language/"+tc.name, func(t *testing.T) {
				c := base.clone(t)
				c.transcript["language"] = tc.language
				res := renderOK(t, bin, run{input: c.input(), env: map[string]string{"RECAP_DEFAULT_LANGUAGE": tc.fallback}})
				if res.Language != tc.want {
					t.Errorf("language = %q, want %q", res.Language, tc.want)
				}
				wantBytes(t, "summary.md", res.summaryMD, golden(t, "F-01", tc.golden))
			})
		}
		// "Speaker n" from the string table of the output language (§5.4, §6).
		for lang, want := range map[string]string{"en": "<v Speaker 1>", "de": "<v Sprecher 1>"} {
			t.Run("speaker-label/"+lang, func(t *testing.T) {
				c := base.clone(t)
				c.transcript["language"] = lang
				delete(c.transcript["speakers"].([]any)[0].(object), "display_name")
				res := renderOK(t, bin, run{input: c.input()})
				if !strings.Contains(string(res.transcript), want+"Welcome to the fixture meeting.") {
					t.Errorf("transcript.vtt has no cue %q for the unnamed spk-1:\n%s", want, res.transcript)
				}
			})
		}
	})
}
