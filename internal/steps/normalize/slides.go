package normalize

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/events"
	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/slidetext"
)

// Constants of templates.md §3.1.
const (
	// minShownMS is how long a page must be shown without a change inside
	// one recording period to count as shown (item 5, inclusive).
	minShownMS = 5000
	// slideReadBytes is how much of a text file is read (item 7).
	slideReadBytes = 64 << 10
	// defaultDeckFile is the original file name of BBB's default deck
	// (item 1; upstream-facts.md "BBB presentations").
	defaultDeckFile = "default.pdf"
	// slidePlaceholder is the text BBB writes when it gets no text from a
	// page (item 6).
	slidePlaceholder = "No text could be retrieved for the slide"
	// utf8BOM is the UTF-8 byte order mark, dropped once at the start of a
	// text file (item 7).
	utf8BOM = "\ufeff"
)

// presentationRE is BBB's own check of a presentation ID before it uses it
// in a path (item 6; upstream-facts.md "BBB presentations").
var presentationRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// pageKey is one page of one deck (deck = presentationName).
type pageKey struct {
	deck string
	page int
}

// pod is what one presentation pod shows while the events are replayed.
type pod struct {
	deck  string
	page  int
	since int64 // meeting axis
	on    bool  // false: the pod shows nothing
}

// slides selects the slides of templates.md §3.1: it replays the
// presentation events in file order, keeps the pages shown inside a
// recording period for at least minShownMS without a change, and reads their
// text from presentation/<pres-id>/textfiles/slide-<page>.txt in fsys. The
// default deck, decks whose ID is not a safe path segment, and pages whose
// file is missing, unreadable, empty or BBB's placeholder give no slide.
// Decks get refs d-<n> in the order of their first SharePresentationEvent,
// counting only decks with a slide. No presentation ID or file name is
// returned. An error is returned for a failed read other than a missing or
// forbidden file (a blob store failure, including a hash mismatch), so the
// step fails as transient and is retried (templates.md §3.1 item 6).
//
// With img set (slide images on, templates.md §3.2) a shown page also gives
// a slide with empty text when it gets an image, and every slide gets the
// image of its page if there is one (images.go).
func slides(fsys fs.FS, files []manifest.File, ev *events.Events, origin int64, ps []meeting.Period,
	img *Images) ([]meeting.Slide, error) {
	first := map[pageKey]int64{} // earliest qualifying start per page
	var shareOrder []string      // decks by first SharePresentationEvent
	shared := map[string]bool{}
	defaults := map[string]bool{}
	deckPage := map[string]int{} // page last set per deck by GotoSlideEvent
	pods := map[string]*pod{}
	var podOrder []string

	// end closes the pod's current showing at t.
	end := func(p *pod, t int64) {
		if p.on {
			for _, from := range shownFrom(p.since, t, ps) {
				k := pageKey{p.deck, p.page}
				if cur, ok := first[k]; !ok || from < cur {
					first[k] = from
				}
			}
		}
		p.on = false
	}
	// show makes the pod show page of deck from t; a showing of the same
	// page goes on unchanged.
	show := func(p *pod, deck string, page int, t int64) {
		if p.on && p.deck == deck && p.page == page {
			return
		}
		end(p, t)
		p.deck, p.page, p.since, p.on = deck, page, t, true
	}
	getPod := func(id string) *pod {
		p, ok := pods[id]
		if !ok {
			p = &pod{}
			pods[id] = p
			podOrder = append(podOrder, id)
		}
		return p
	}

	for _, e := range ev.Slides {
		t := e.TS - origin
		switch e.Kind {
		case events.EventConversionDone:
			if e.OriginalFilename == defaultDeckFile {
				defaults[e.Presentation] = true
			}
		case events.EventSharePresentation:
			if e.Share != "true" {
				continue
			}
			if !shared[e.Presentation] {
				shared[e.Presentation] = true
				shareOrder = append(shareOrder, e.Presentation)
			}
			page := deckPage[e.Presentation]
			if page == 0 {
				page = 1
			}
			show(getPod(e.PodID), e.Presentation, page, t)
		case events.EventGotoSlide:
			n, err := strconv.Atoi(strings.TrimSpace(e.Slide))
			if err != nil || n < 0 {
				continue
			}
			deckPage[e.Presentation] = n + 1
			if p := getPod(e.PodID); p.on && p.deck == e.Presentation {
				show(p, e.Presentation, n+1, t)
			}
		case events.EventRemovePresentation:
			if p := getPod(e.PodID); p.on && p.deck == e.Presentation {
				end(p, t)
			}
		case events.EventRemovePod:
			end(getPod(e.PodID), t)
		}
	}
	if len(ps) > 0 {
		last := ps[len(ps)-1].EndMS
		for _, id := range podOrder {
			end(pods[id], last)
		}
	}

	type found struct {
		meeting.Slide
		deck string
	}
	var list []found
	for k, t := range first {
		if defaults[k.deck] || !presentationRE.MatchString(k.deck) {
			continue
		}
		text, err := slideText(fsys, "presentation/"+k.deck+"/textfiles/slide-"+strconv.Itoa(k.page)+".txt")
		if err != nil {
			return nil, err
		}
		if text == "" && img == nil {
			continue
		}
		list = append(list, found{meeting.Slide{TMS: t, Page: k.page, Text: text}, k.deck})
	}
	// The final order (below) sorts decks by their ref, and refs follow the
	// share order; so the share order gives the same order before the refs
	// are known. Images are made in that order (§3.2 "Dedupe and cap").
	shareIndex := map[string]int{}
	for i, d := range shareOrder {
		shareIndex[d] = i
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.TMS != b.TMS {
			return a.TMS < b.TMS
		}
		if shareIndex[a.deck] != shareIndex[b.deck] {
			return shareIndex[a.deck] < shareIndex[b.deck]
		}
		return a.Page < b.Page
	})
	if img != nil {
		im := newImager(fsys, files, img)
		defer im.close()
		kept := list[:0]
		for _, f := range list {
			ref, err := im.image(f.deck, f.Page)
			if err != nil {
				return nil, err
			}
			if ref == "" && f.Text == "" {
				continue // neither text nor image: no slide
			}
			f.Image = ref
			kept = append(kept, f)
		}
		list = kept
	}
	hasSlide := map[string]bool{}
	for _, f := range list {
		hasSlide[f.deck] = true
	}
	refs := map[string]int{}
	for _, d := range shareOrder {
		if hasSlide[d] {
			refs[d] = len(refs) + 1
		}
	}
	out := make([]meeting.Slide, 0, len(list))
	for _, s := range list {
		s.Deck = "d-" + strconv.Itoa(refs[s.deck])
		out = append(out, s.Slide)
	}
	return out, nil
}

// shownFrom clips the showing [from, to) to each recording period and
// returns the start of every clipped interval of at least minShownMS
// (item 5; separate shorter intervals are not added up).
func shownFrom(from, to int64, ps []meeting.Period) []int64 {
	var out []int64
	for _, p := range ps {
		lo, hi := max(from, p.StartMS), min(to, p.EndMS)
		if hi-lo >= minShownMS {
			out = append(out, lo)
		}
	}
	return out
}

// slideText reads and cleans one slide text file (item 6, 7). One leading
// UTF-8 byte order mark is dropped. A missing or forbidden file, an empty
// cleaned text and BBB's placeholder give "". After the cut to
// slidetext.MaxChars code points, trailing spaces and line feeds are trimmed, so a block never
// ends in an empty line.
func slideText(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, slideReadBytes))
	if err != nil {
		return "", err
	}
	text := slidetext.Clean(bytes.TrimPrefix(b, []byte(utf8BOM)))
	if text == slidePlaceholder {
		return "", nil
	}
	return slidetext.Cut(text), nil
}
