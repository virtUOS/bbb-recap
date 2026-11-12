// Package htmltext turns an HTML fragment into plain text, with the standard
// library only. It is meant for chat messages and shared notes with the
// media type text/html, not for full documents. The BBB source uses it for
// chat messages (messageAsHtml) and the notes export notes/notes.html.
//
// Rules:
//   - tags are removed; block tags (p, div, br, li, headings, table rows, …)
//     become line breaks;
//   - the content of script and style elements and HTML comments is dropped;
//   - character references are decoded (html.UnescapeString) after the tags
//     are gone, so an escaped "&lt;b&gt;" stays the text "<b>";
//   - whitespace inside a line collapses to one space (as a browser shows
//     it), a line break in the source is a space, lines are trimmed and
//     empty lines are dropped.
package htmltext

import (
	"html"
	"strings"
)

// blockTags become a line break where they open or close.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true, "dd": true, "div": true,
	"dl": true, "dt": true, "figcaption": true, "figure": true, "footer": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tr": true, "ul": true,
}

// skipTags have content that is not text.
var skipTags = map[string]bool{"script": true, "style": true}

// Text returns the plain text of an HTML fragment.
func Text(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(text(s))
			break
		}
		b.WriteString(text(s[:i]))
		s = s[i:]
		if strings.HasPrefix(s, "<!--") {
			end := strings.Index(s, "-->")
			if end < 0 {
				break
			}
			s = s[end+3:]
			continue
		}
		end := strings.IndexByte(s, '>')
		if end < 0 || !isTagStart(s) {
			// Not a tag: a lone "<" is text.
			b.WriteByte('<')
			s = s[1:]
			continue
		}
		name, closing := tagName(s[1:end])
		s = s[end+1:]
		switch {
		case skipTags[name] && !closing:
			s = s[closeTag(s, name):]
		case blockTags[name]:
			b.WriteByte('\n')
		}
	}
	return tidy(b.String())
}

// closeTag returns the offset of the first "</name" in s, matching name
// case-insensitively on the original bytes (so offsets stay valid for any
// UTF-8), or len(s) if there is none.
func closeTag(s, name string) int {
	for i := 0; ; {
		j := strings.Index(s[i:], "</")
		if j < 0 {
			return len(s)
		}
		j += i
		if end := j + 2 + len(name); end <= len(s) && strings.EqualFold(s[j+2:end], name) {
			return j
		}
		i = j + 2
	}
}

// isTagStart reports whether s, which starts with "<", starts a tag: a
// letter, "/" or "!" must follow.
func isTagStart(s string) bool {
	if len(s) < 2 {
		return false
	}
	c := s[1]
	return c == '/' || c == '!' || c == '?' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// tagName returns the lower-case name of a tag body ("p class=x", "/p",
// "br/") and whether it is a closing tag.
func tagName(body string) (string, bool) {
	closing := strings.HasPrefix(body, "/")
	body = strings.TrimPrefix(body, "/")
	end := strings.IndexAny(body, " \t\r\n/")
	if end >= 0 {
		body = body[:end]
	}
	return strings.ToLower(body), closing
}

// text decodes a run of text between tags; its line breaks are spaces.
func text(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(html.UnescapeString(s))
}

// tidy collapses whitespace per line (strings.Fields also splits at
// no-break spaces) and drops empty lines.
func tidy(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
