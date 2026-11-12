// Package llmplan holds the token budget and the map cut of
// docs/reference/providers.md §3.5: tokens are estimated as ceil(chars / 3.0)
// (no tokenizer), the budget leaves a 10 % margin of context_tokens, and a
// transcript is cut into chunks only between lines (one line is one
// segment). A line longer than the budget is cut at its last sentence end,
// else its last space. A chunk can be split in half once (re-split, step 4).
//
// Characters are Unicode code points. Everything here is deterministic.
package llmplan

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Constants of providers.md §1.5.
const (
	CharsPerToken   = 3.0
	MarginPercent   = 10
	MaxReduceLevels = 3
)

// Tokens is the estimate for a text of chars characters: ceil(chars / 3.0).
func Tokens(chars int) int { return int(math.Ceil(float64(chars) / CharsPerToken)) }

// TextTokens is Tokens of the characters of s.
func TextTokens(s string) int { return Tokens(utf8.RuneCountInString(s)) }

// Budget is §3.5 step 1: context − max output − prompt tokens − 10 % of
// context (rounded up, so the margin is never smaller than 10 %).
// promptChars are the characters of the prompt around the data that is cut.
func Budget(contextTokens, maxOutputTokens, promptChars int) int {
	margin := (contextTokens*MarginPercent + 99) / 100
	return contextTokens - maxOutputTokens - Tokens(promptChars) - margin
}

// ChunkTokens is the estimate for a chunk: its lines joined with "\n".
func ChunkTokens(lines []string) int {
	n := max(len(lines)-1, 0)
	for _, l := range lines {
		n += utf8.RuneCountInString(l)
	}
	return Tokens(n)
}

// Chunk cuts lines into consecutive chunks of at most budget tokens, only
// between lines, without overlap (§3.5 step 2). A line over the budget is
// first cut into pieces (Cut). A budget below 1 is treated as 1.
func Chunk(lines []string, budget int) [][]string {
	budget = max(budget, 1)
	var chunks [][]string
	var cur []string
	for _, l := range lines {
		for _, piece := range Cut(l, budget*int(CharsPerToken)) {
			if len(cur) > 0 && ChunkTokens(append(cur[:len(cur):len(cur)], piece)) > budget {
				chunks = append(chunks, cur)
				cur = nil
			}
			cur = append(cur, piece)
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// Cut splits a line into pieces of at most maxChars characters: each piece
// ends at the last sentence end within the limit, else at the last space,
// else (one word longer than the limit) at the limit. The whitespace at a
// cut is dropped. A line within the limit is returned as is.
func Cut(line string, maxChars int) []string {
	maxChars = max(maxChars, 1)
	var pieces []string
	for utf8.RuneCountInString(line) > maxChars {
		head, rest := cutOnce(line, maxChars)
		pieces = append(pieces, head)
		line = rest
	}
	if line != "" || len(pieces) == 0 {
		pieces = append(pieces, line)
	}
	return pieces
}

// cutOnce returns the first piece of at most maxChars characters and the
// rest of s, which is longer than maxChars.
func cutOnce(s string, maxChars int) (head, rest string) {
	r := []rune(s)
	sentence, space := -1, -1 // length of the head for each kind of cut
	for i := 0; i <= maxChars && i < len(r); i++ {
		if !unicode.IsSpace(r[i]) {
			continue
		}
		if i > 0 && !unicode.IsSpace(r[i-1]) {
			space = i
			if strings.ContainsRune(".!?", r[i-1]) {
				sentence = i
			}
		}
	}
	n := sentence
	if n <= 0 {
		n = space
	}
	if n <= 0 {
		return string(r[:maxChars]), string(r[maxChars:])
	}
	return string(r[:n]), strings.TrimLeftFunc(string(r[n:]), unicode.IsSpace)
}

// Halve splits a chunk in two for the re-split of §3.5 step 4: between lines
// where the two halves are closest in size, or, for a chunk of one line,
// that line at the last sentence end (else the last space) within its first
// half (else the last one in the line). ok is false when the chunk cannot be split (one line with no
// whitespace inside, or nothing in it).
func Halve(chunk []string) (a, b []string, ok bool) {
	if len(chunk) >= 2 {
		k, best := 1, -1
		for i := 1; i < len(chunk); i++ {
			d := ChunkTokens(chunk[:i]) - ChunkTokens(chunk[i:])
			if d < 0 {
				d = -d
			}
			if best < 0 || d < best {
				k, best = i, d
			}
		}
		return chunk[:k:k], chunk[k:], true
	}
	if len(chunk) == 0 {
		return nil, nil, false
	}
	line := chunk[0]
	n := utf8.RuneCountInString(line)
	if n < 2 {
		return nil, nil, false
	}
	head, rest := cutOnce(line, (n+1)/2)
	if !hasSpaceCut(line, head) {
		// Nothing to cut at in the first half: the last cut before the end.
		head, rest = cutOnce(line, n-1)
	}
	if rest == "" || head == "" || !hasSpaceCut(line, head) {
		return nil, nil, false
	}
	return []string{head}, []string{rest}, true
}

// hasSpaceCut reports whether head ends where line has whitespace, so the
// cut did not split a word.
func hasSpaceCut(line, head string) bool {
	r, _ := utf8.DecodeRuneInString(line[len(head):])
	return unicode.IsSpace(r)
}

// Resplit replaces each chunk named in indexes (positions in chunks) by its
// two halves (§3.5 step 4). Indexes out of range, repeated, or of chunks
// that cannot be halved are ignored.
func Resplit(chunks [][]string, indexes []int) [][]string {
	split := map[int]bool{}
	for _, i := range indexes {
		split[i] = true
	}
	var out [][]string
	for i, c := range chunks {
		if a, b, ok := Halve(c); split[i] && ok {
			out = append(out, a, b)
			continue
		}
		out = append(out, c)
	}
	return out
}

// Groups packs items in order into groups of at most budget tokens each
// (items joined with "\n"), for the reduce of §3.5 step 3. An item over the
// budget forms a group of its own.
func Groups(items []string, budget int) [][]string {
	var groups [][]string
	var cur []string
	for _, it := range items {
		if len(cur) > 0 && ChunkTokens(append(cur[:len(cur):len(cur)], it)) > budget {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, it)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}
