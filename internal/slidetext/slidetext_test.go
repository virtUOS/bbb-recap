package slidetext

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Title\r\n\r\n  a   b  \r c\n", "Title\na b\nc"},
		{"\tTotal\t\t  120 EUR\f", "Total 120 EUR"},
		{"bad \xff\xfe byte", "bad �� byte"},
		{"\x00\x1f\x7f", ""},
		{"Zeile\u0085zwei x", "Zeile zwei x"}, // NEL is Cc; NBSP is not
	} {
		if got := Clean([]byte(c.in)); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCut(t *testing.T) {
	if got := Cut(strings.Repeat("ä", 1000)); got != strings.Repeat("ä", 1000) {
		t.Errorf("1 000 code points are cut")
	}
	if got := Cut(strings.Repeat("a", 999) + "\nmore"); got != strings.Repeat("a", 999) {
		t.Errorf("Cut leaves %q at the end, want no line feed", got[len(got)-3:])
	}
}
