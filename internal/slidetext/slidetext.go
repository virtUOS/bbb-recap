// Package slidetext cleans slide text (docs/reference/templates.md §3.1
// item 7). It is source neutral (ADR-013): the BBB source cleans the text
// files of a deck with it, the describe step the description of a slide
// image (§3.2).
package slidetext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxChars is the number of code points kept of a cleaned text (§3.1 item 7;
// the most the LLM input uses).
const MaxChars = 1000

// Clean applies the cleaning of §3.1 item 7: invalid UTF-8 bytes become
// U+FFFD, CRLF and CR become LF, every other control character becomes a
// space, in each line runs of spaces become one space and the line is
// trimmed, empty lines are dropped, lines are joined with LF.
func Clean(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		switch {
		case r == '\r':
			sb.WriteByte('\n')
			if i+1 < len(b) && b[i+1] == '\n' {
				n = 2
			}
		case r == '\n':
			sb.WriteByte('\n')
		case unicode.Is(unicode.Cc, r):
			sb.WriteByte(' ')
		default:
			sb.WriteRune(r) // utf8.RuneError for an invalid byte
		}
		i += n
	}
	var lines []string
	for _, l := range strings.Split(sb.String(), "\n") {
		l = strings.Join(strings.FieldsFunc(l, func(r rune) bool { return r == ' ' }), " ")
		if l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// Cut cuts a cleaned text to its first MaxChars code points; trailing spaces
// and line feeds left by the cut are trimmed, so a block never ends in an
// empty line.
func Cut(text string) string {
	if utf8.RuneCountInString(text) <= MaxChars {
		return text
	}
	return strings.TrimRight(string([]rune(text)[:MaxChars]), " \n")
}
