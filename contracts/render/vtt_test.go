package render

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/virtUOS/bbb-recap/contracts/internal/harness"
)

// validateVTT checks a transcript.vtt against the WebVTT file syntax as
// templates.md §6 states it from W3C WebVTT §4.1-4.2
// (https://www.w3.org/TR/webvtt1/), plus the profile §6 fixes for
// transcript.vtt. It returns one message per problem.
//
// W3C rules: UTF-8; starts with "WEBVTT" followed by a blank line; blocks are
// separated by blank lines; a NOTE block contains no "-->"; a cue identifier
// contains no "-->" and no line break; timing "hh:mm:ss.ttt --> hh:mm:ss.ttt"
// with end > start; each start >= all earlier starts; cue payload lines are
// not empty and contain no "-->"; cue text has no raw "<" outside tags and
// no "&" outside character references; a voice annotation has no LF, CR,
// "&" (outside references) or ">".
//
// Profile (templates.md §6): no BOM, LF only, exactly one blank line between
// blocks, file ends with one LF, NOTE blocks are one line, every cue has a
// unique identifier, no cue settings, no STYLE or REGION blocks, one payload
// line of the form <v name>text</v> with "<" and ">" escaped in both parts.
func validateVTT(b []byte) []string {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	s := string(b)
	switch {
	case !utf8.ValidString(s):
		return []string{"not valid UTF-8"}
	case strings.HasPrefix(s, "\uFEFF"):
		return []string{"starts with a BOM"}
	case strings.ContainsAny(s, "\r\x00"):
		return []string{"contains CR or NUL"}
	case !strings.HasPrefix(s, "WEBVTT\n\n"):
		return []string{`does not start with "WEBVTT" and one blank line`}
	case !strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\n\n"):
		return []string{"does not end with exactly one LF"}
	}
	body := strings.TrimSuffix(strings.TrimPrefix(s, "WEBVTT\n\n"), "\n")
	ids := map[string]bool{}
	lastStart := int64(-1)
	for i, block := range strings.Split(body, "\n\n") {
		where := fmt.Sprintf("block %d", i+1)
		if block == "" || strings.HasPrefix(block, "\n") {
			bad("%s: more than one blank line between blocks", where)
			continue
		}
		ls := strings.Split(block, "\n")
		if ls[0] == "NOTE" || strings.HasPrefix(ls[0], "NOTE ") || strings.HasPrefix(ls[0], "NOTE\t") {
			if strings.Contains(block, "-->") {
				bad("%s: NOTE contains \"-->\": %q", where, block)
			}
			if len(ls) != 1 {
				bad("%s: NOTE spans %d lines, want one", where, len(ls))
			}
			continue
		}
		if strings.HasPrefix(ls[0], "STYLE") || strings.HasPrefix(ls[0], "REGION") {
			bad("%s: STYLE or REGION block", where)
			continue
		}
		if len(ls) != 3 {
			bad("%s: cue has %d lines, want identifier, timing and one payload line: %q", where, len(ls), block)
			continue
		}
		id, timing, payload := ls[0], ls[1], ls[2]
		if strings.Contains(id, "-->") {
			bad("%s: no cue identifier (first line has \"-->\"): %q", where, block)
			continue
		}
		if ids[id] {
			bad("%s: duplicate cue identifier %q", where, id)
		}
		ids[id] = true
		start, end, ok := parseTiming(timing)
		if !ok {
			bad("%s: bad timing line %q", where, timing)
			continue
		}
		if end <= start {
			bad("%s: end %d ms not after start %d ms", where, end, start)
		}
		if start < lastStart {
			bad("%s: start %d ms before an earlier start %d ms", where, start, lastStart)
		}
		lastStart = start
		if strings.Contains(payload, "-->") {
			bad("%s: payload contains \"-->\"", where)
		}
		m := voiceRE.FindStringSubmatch(payload)
		if m == nil {
			bad("%s: payload is not <v name>text</v>: %q", where, payload)
			continue
		}
		for _, part := range []string{m[1], m[2]} {
			if strings.ContainsAny(part, "<>") {
				bad("%s: raw < or > in %q", where, part)
			}
			if strings.Count(part, "&") != len(charRefRE.FindAllString(part, -1)) {
				bad("%s: raw & in %q", where, part)
			}
		}
	}
	return errs
}

var (
	timingRE  = regexp.MustCompile(`^([0-9]{2,}):([0-5][0-9]):([0-5][0-9])\.([0-9]{3}) --> ([0-9]{2,}):([0-5][0-9]):([0-5][0-9])\.([0-9]{3})$`)
	voiceRE   = regexp.MustCompile(`^<v ([^\n]*?)>(.*)</v>$`)
	charRefRE = regexp.MustCompile(`&(amp|lt|gt|nbsp|lrm|rlm|#[0-9]+|#[xX][0-9a-fA-F]+);`)
)

// parseTiming returns start and end in ms.
func parseTiming(line string) (start, end int64, ok bool) {
	m := timingRE.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	ms := func(h, mi, s, t string) int64 {
		var v [4]int64
		for i, x := range []string{h, mi, s, t} {
			v[i], _ = strconv.ParseInt(x, 10, 64)
		}
		return ((v[0]*60+v[1])*60+v[2])*1000 + v[3]
	}
	return ms(m[1], m[2], m[3], m[4]), ms(m[5], m[6], m[7], m[8]), true
}

// adversarial strings for names and text (templates.md §8: "&", "<b>",
// "-->", ">", newlines; "--->" and "---->" for the repeated "-->" -> "->"
// rule of gap NOTE names, §6).
var adversarial = []string{
	"&", "&amp;", "<b>bold</b>", "-->", "--->", "---->", "a --> b", "a -- > b", ">", "a\nb", "a\r\nb", "x\n\nseg-000099\n00:00:00.000 --> 00:00:01.000\ny",
	"NOTE x", "WEBVTT", "\ttab\t", "<v Eve>hi</v>", "&#60;", "a&b;c",
}

// WebVTT syntax: the goldens and every rendered file, including adversarial
// names and text, pass the validator above.
func TestVTTSyntax(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-09", func(t *testing.T) {
		for _, g := range []string{"F-01/transcript.vtt", "F-01/transcript.de.vtt", "gap/transcript.vtt", "gap/transcript.de.vtt", "mapping/transcript.vtt"} {
			if errs := validateVTT(golden(t, g)); len(errs) > 0 {
				t.Fatalf("fixture: testdata/render/%s fails the validator: %v", g, errs)
			}
		}
		base := loadCase(t, "F-01")
		for i, s := range adversarial {
			t.Run(fmt.Sprintf("case-%02d", i+1), func(t *testing.T) {
				c := base.clone(t)
				for _, sp := range c.transcript["speakers"].([]any) {
					sp.(object)["display_name"] = "Ada " + s
				}
				for _, seg := range c.transcript["segments"].([]any) {
					seg.(object)["text"] = s + " end"
				}
				// Ada's first track becomes a gap too, so a gap NOTE carries the name.
				c.transcript["tracks"].([]any)[0].(object)["status"] = "failed"
				c.transcript["gaps"] = []any{object{
					"track_ref": "trk-1", "speaker_ref": "spk-1", "start_ms": 200, "end_ms": 55000, "reason": "asr_failed",
				}}
				res := renderOK(t, bin, run{input: c.input()}) // renderOK runs the validator
				for _, l := range lines(res.transcript) {
					if strings.HasPrefix(l, "NOTE Gap ") {
						return
					}
				}
				t.Errorf("no gap NOTE in:\n%s", res.transcript)
			})
		}
	})
}

// WebVTT mapping: pause collapse and pause NOTE, gap NOTE placement for a
// failed-track gap (gap fixture: ASR failed on Ada's first track), a gap NOTE
// tie with a cue start, pause NOTE before gap NOTE for the same cue, gap NOTE
// at the end, zero-length segment (+1 ms), segments crossing a period
// boundary (clamped), "Speaker n", escaping, and the gap NOTE name rule of
// §6: whitespace cleaned, & < > kept literally, "-->" -> "->" until none is left.
func TestVTTMapping(t *testing.T) {
	bin := harness.Bin(t)
	t.Run("CT-TPL-10", func(t *testing.T) {
		gap := loadCase(t, "gap")
		t.Run("gap", func(t *testing.T) {
			res := renderOK(t, bin, run{input: gap.input()})
			wantBytes(t, "gap transcript.vtt", res.transcript, golden(t, "gap", "transcript.vtt"))
		})
		t.Run("gap-de", func(t *testing.T) {
			c := gap.clone(t)
			c.transcript["language"] = "de"
			res := renderOK(t, bin, run{input: c.input()})
			wantBytes(t, "gap transcript.de.vtt", res.transcript, golden(t, "gap", "transcript.de.vtt"))
		})
		t.Run("mapping", func(t *testing.T) {
			c := renderCase{
				context:    gap.context,
				transcript: loadObject(t, renderDir, "mapping", "transcript.json"),
				summary:    gap.summary,
			}
			res := renderOK(t, bin, run{input: c.input()})
			wantBytes(t, "mapping transcript.vtt", res.transcript, golden(t, "mapping", "transcript.vtt"))
		})
	})
}
