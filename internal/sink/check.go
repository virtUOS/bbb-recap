package sink

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitle is the longest title in characters (sinks.md §2.3).
const MaxTitle = 200

// DefaultMaxDocumentBytes is the default max_document_bytes of a sink
// profile (sinks.md §2.4: 2 MiB).
const DefaultMaxDocumentBytes = 2 << 20

// Check runs the checks before sending (sinks.md §2.4): the summary is
// UTF-8, matches its SHA-256, is at most maxBytes long (0: the default) and
// has none of the forbidden constructs of §2.2 that can be told apart
// reliably; the title is one line of at most MaxTitle characters without
// control characters (§2.3). A violation is a Rejected *Error.
func Check(req Request, maxBytes int64) *Error {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDocumentBytes
	}
	md := req.SummaryMD
	switch {
	case !utf8.ValidString(md):
		return rejected("invalid_utf8")
	case int64(len(md)) > maxBytes:
		return rejected("document_too_large")
	case hexSHA256(md) != req.SummarySHA256:
		return rejected("summary_sha256_mismatch")
	}
	if code := dialectViolation(md); code != "" {
		return rejected(code)
	}
	t := req.Title
	if !utf8.ValidString(t) || strings.TrimSpace(t) == "" || strings.ContainsFunc(t, unicode.IsControl) {
		return rejected("title_invalid")
	}
	if utf8.RuneCountInString(t) > MaxTitle {
		return rejected("title_too_long")
	}
	return nil
}

func rejected(code string) *Error { return &Error{Class: Rejected, Code: code} }

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Patterns of the §2.2 scan. They run on a line with backslash escapes and
// code spans removed; fenced code is skipped.
var (
	escapedChar = regexp.MustCompile(`\\.`)
	codeSpan    = regexp.MustCompile("`[^`]*`")
	fence       = regexp.MustCompile("^ {0,3}(```|~~~)")
	setextLine  = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	blockStart  = regexp.MustCompile(`^ {0,3}([-*+>|#]|[0-9]+[.)])`)
	forbidden   = []struct {
		code string
		re   *regexp.Regexp
	}{
		{"raw_html", regexp.MustCompile(`<[A-Za-z!/?]`)}, // tags, comments and autolinks <https://…>
		{"image", regexp.MustCompile(`!\[`)},
		{"footnote", regexp.MustCompile(`\[\^`)},
		{"reference_link", regexp.MustCompile(`\]\[`)},
		{"reference_link", regexp.MustCompile(`^ {0,3}\[[^\]]+\]:`)}, // link reference definition
	}
)

// dialectViolation returns the code of the first forbidden construct of
// sinks.md §2.2 in md, or "". Not detected (no unambiguous form): definition
// lists, math and block content in table cells.
func dialectViolation(md string) string {
	inFence := false
	prev := ""
	for _, line := range strings.Split(md, "\n") {
		if fence.MatchString(line) {
			inFence = !inFence
			prev = ""
			continue
		}
		if inFence {
			continue
		}
		s := codeSpan.ReplaceAllString(escapedChar.ReplaceAllString(line, ""), "")
		for _, f := range forbidden {
			if f.re.MatchString(s) {
				return f.code
			}
		}
		if setextLine.MatchString(line) && strings.TrimSpace(prev) != "" && !blockStart.MatchString(prev) {
			return "setext_heading"
		}
		prev = line
	}
	return ""
}
