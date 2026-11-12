package htmltext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain text", "plain text"},
		{"Fish &amp; chips &lt;b&gt; &#39;x&#39; &quot;y&quot;", `Fish & chips <b> 'x' "y"`},
		{"<b>bold</b> and <a href=\"https://example.org\">link</a>", "bold and link"},
		{"line one<br>line two<br/>line three", "line one\nline two\nline three"},
		{"<p>first</p><p>second</p>", "first\nsecond"},
		{"<ul><li>a</li><li>b</li></ul>", "a\nb"},
		{"  spaced \t\n out  ", "spaced out"},
		{"a&nbsp;&nbsp;b", "a b"},
		{"x <!-- hidden --> y", "x y"},
		{"<style>p{color:red}</style>text<script>alert(1)</script>", "text"},
		{"1 < 2 and 3 > 2", "1 < 2 and 3 > 2"},
		{"<P CLASS=x>Upper</P>", "Upper"},
		{"<br><br><br>", ""},
		{"unterminated <b", "unterminated <b"},
		// Non-ASCII content must not shift the closing-tag search.
		{"<script>" + strings.Repeat("İ", 10) + "</script>x", "x"},
		{"<SCRIPT>" + strings.Repeat("İ", 29) + "</SCRIPT>", ""},
		{"<style>a</b></STYLE>y", "y"},
	} {
		if got := Text(c.in); got != c.want || !utf8.ValidString(got) {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
