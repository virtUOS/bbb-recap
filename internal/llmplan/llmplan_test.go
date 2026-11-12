package llmplan

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestTokensAndBudget(t *testing.T) {
	for chars, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 2, 300: 100} {
		if got := Tokens(chars); got != want {
			t.Errorf("Tokens(%d) = %d, want %d", chars, got, want)
		}
	}
	if got := TextTokens("äöü"); got != 1 {
		t.Errorf("TextTokens counts bytes, not characters: %d", got)
	}
	// 10000 − 2000 − ceil(3000/3) − 1000 (contracts/providers CT-PROV-11).
	if got := Budget(10000, 2000, 3000); got != 6000 {
		t.Errorf("Budget = %d, want 6000", got)
	}
	// 32768: the margin rounds up (3276.8 → 3277).
	if got := Budget(32768, 4096, 0); got != 32768-4096-3277 {
		t.Errorf("Budget = %d", got)
	}
}

func TestChunk(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("[00:00:%02d] Speaker 1: line %02d of the fixture.", i, i)) // 44 chars
	}
	chunks := Chunk(lines, 50) // 150 chars: three lines per chunk
	var flat []string
	for _, c := range chunks {
		if ChunkTokens(c) > 50 {
			t.Errorf("chunk over budget: %v", c)
		}
		flat = append(flat, c...)
	}
	if !slices.Equal(flat, lines) || len(chunks[0]) != 3 {
		t.Errorf("chunks %v", chunks)
	}
	if got := Chunk(lines, 10000); len(got) != 1 {
		t.Errorf("%d chunks under budget, want 1", len(got))
	}
}

func TestCut(t *testing.T) {
	for _, c := range []struct {
		line string
		max  int
		want []string
	}{
		{"Short.", 10, []string{"Short."}},
		{"One. Two words here and more", 15, []string{"One.", "Two words here", "and more"}},
		{"aaaa bbbb cccc", 9, []string{"aaaa bbbb", "cccc"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"Ünïcödé wörds hère", 7, []string{"Ünïcödé", "wörds", "hère"}},
	} {
		if got := Cut(c.line, c.max); !slices.Equal(got, c.want) {
			t.Errorf("Cut(%q, %d) = %q, want %q", c.line, c.max, got, c.want)
		}
	}
}

func TestHalveAndResplit(t *testing.T) {
	a, b, ok := Halve([]string{"one", "two", "three", "four"})
	if !ok || !slices.Equal(a, []string{"one", "two"}) || !slices.Equal(b, []string{"three", "four"}) {
		t.Errorf("Halve lines = %v %v %v", a, b, ok)
	}
	a, b, ok = Halve([]string{"First sentence. Second sentence goes on."})
	if !ok || a[0] != "First sentence." || b[0] != "Second sentence goes on." {
		t.Errorf("Halve one line = %q %q %v", a, b, ok)
	}
	a, b, ok = Halve([]string{"longwordwithoutspace end"})
	if !ok || a[0] != "longwordwithoutspace" || b[0] != "end" {
		t.Errorf("Halve one line, space in the second half = %q %q %v", a, b, ok)
	}
	if _, _, ok := Halve([]string{"nospace"}); ok {
		t.Errorf("Halve split a single word")
	}
	if _, _, ok := Halve(nil); ok {
		t.Errorf("Halve split nothing")
	}
	chunks := [][]string{{"a"}, {"b c", "d"}, {"e"}}
	got := Resplit(chunks, []int{1, 7})
	if len(got) != 4 || !slices.Equal(got[1], []string{"b c"}) || !slices.Equal(got[2], []string{"d"}) {
		t.Errorf("Resplit = %v", got)
	}
}

func TestGroups(t *testing.T) {
	items := []string{strings.Repeat("a", 60), strings.Repeat("b", 60), strings.Repeat("c", 200), "d"}
	got := Groups(items, 50) // 150 chars
	want := [][]string{items[:2], items[2:3], items[3:]}
	if len(got) != len(want) {
		t.Fatalf("Groups = %v", got)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("group %d = %v, want %v", i, got[i], want[i])
		}
	}
}
