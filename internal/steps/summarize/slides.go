package summarize

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// promptSlides is the format's slide rule, prompts/slides.md (templates.md
// §3.1 "Prompt").
const promptSlides = "slides"

// Size constants of templates.md §3.1 "Size"; constants, not settings.
const (
	slideMaxChars     = 1000 // per slide text
	slidesMinChars    = 2000 // the section limit is never below this
	slidesBudgetPct   = 15   // the section may use this share of the call budget
	slidesCharsPerTok = 3    // characters per token of the budget (providers.md §3.5)
)

// slideBlock is one slide of the SLIDES section: the full block and the
// title-only block.
type slideBlock struct {
	full, title string
}

// slideBlocks renders the slides of the input as blocks
// "[hh:mm:ss] Slide d-1/2:" plus the text, cut to slideMaxChars code points.
// Slides outside every recording period are left out, as chat is.
func (r *run) slideBlocks() []slideBlock {
	if len(r.in.Slides) == 0 || r.in.Transcript == nil {
		return nil
	}
	periods := r.in.Transcript.RecordingPeriods
	var out []slideBlock
	for _, s := range r.in.Slides {
		if !inPeriods(s.TMS, periods) {
			continue
		}
		text := s.Text
		if utf8.RuneCountInString(text) > slideMaxChars {
			text = string([]rune(text)[:slideMaxChars])
		}
		head := "[" + hms(recorded(s.TMS, periods)) + "] Slide " + oneLine(s.Deck) + "/" + strconv.Itoa(s.Page) + ":\n"
		first, _, _ := strings.Cut(text, "\n")
		out = append(out, slideBlock{full: head + text, title: head + first})
	}
	return out
}

// slidesLimit is the most characters the SLIDES data may have in a call
// with the given token budget: max(2 000, floor(0.15 × budget × 3)).
func slidesLimit(budget int) int {
	return max(slidesMinChars, budget*slidesBudgetPct*slidesCharsPerTok/100)
}

// slidesSection returns the SLIDES data for a call with the given budget
// and the number of slides in it (templates.md §3.1 "Size"): all blocks in
// full if they fit the limit, else the title-only blocks, dropping the
// latest slides until they fit. Lengths are code points before the fence
// markers are broken up. No slides give "".
func (r *run) slidesSection(budget int) (string, int) {
	if len(r.slides) == 0 {
		return "", 0
	}
	limit := slidesLimit(budget)
	full := make([]string, len(r.slides))
	for i, b := range r.slides {
		full[i] = b.full
	}
	if s := strings.Join(full, "\n\n"); utf8.RuneCountInString(s) <= limit {
		return s, len(full)
	}
	var titles []string
	n := 0
	for _, b := range r.slides {
		add := utf8.RuneCountInString(b.title)
		if len(titles) > 0 {
			add += 2 // the blank line between blocks
		}
		if n+add > limit {
			break
		}
		titles = append(titles, b.title)
		n += add
	}
	return strings.Join(titles, "\n\n"), len(titles)
}
