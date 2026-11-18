package normalize

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/virtUOS/bbb-recap/internal/events"
	"github.com/virtUOS/bbb-recap/internal/meeting"
)

// Synthetic deck IDs in BBB's form (sha1-ms); not real presentations.
const (
	deckA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1761732000100"
	deckB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-1761732000200"
)

func textFile(deck string, page, text string) (string, *fstest.MapFile) {
	return "presentation/" + deck + "/textfiles/slide-" + page + ".txt", &fstest.MapFile{Data: []byte(text)}
}

func slideFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for k, v := range files {
		deck, page, _ := strings.Cut(k, "/")
		p, f := textFile(deck, page, v)
		fsys[p] = f
	}
	return fsys
}

func share(ts int64, podID, deck string) events.Slide {
	return events.Slide{TS: ts, Kind: events.EventSharePresentation, PodID: podID, Presentation: deck, Share: "true"}
}

func gotoSlide(ts int64, podID, deck, slide string) events.Slide {
	return events.Slide{TS: ts, Kind: events.EventGotoSlide, PodID: podID, Presentation: deck, Slide: slide}
}

// One period 0–100 000 ms on the meeting axis; origin 0.
var onePeriod = []meeting.Period{{StartMS: 0, EndMS: 100000}}

func TestSlidesReplay(t *testing.T) {
	fsys := slideFS(map[string]string{
		deckA + "/1": "A one", deckA + "/2": "A two", deckA + "/3": "A three",
		deckB + "/1": "B one", deckB + "/2": "B two",
	})
	ev := &events.Events{Slides: []events.Slide{
		share(1000, "pod1", deckA),                                                            // A/1 1 000–6 000: 5 000 ms, shown
		gotoSlide(3000, "pod1", deckA, "0"),                                                   // same page: the showing goes on
		gotoSlide(4000, "pod2", deckA, "0"),                                                   // other pod, deck not current there: nothing
		gotoSlide(6000, "pod1", deckA, "1"),                                                   // A/2 from 6 000
		gotoSlide(10999, "pod1", deckB, "1"),                                                  // other deck: sets B's page, pod 1 stays on A/2
		share(11000, "pod2", deckB),                                                           // pod 2 shows B/2 from 11 000 (its last page)
		gotoSlide(14000, "pod1", deckA, "2"),                                                  // A/2 6 000–14 000: 8 000 ms, shown at 6 000
		{TS: 16000, Kind: events.EventRemovePod, PodID: "pod1"},                               // A/3 2 000 ms only
		{TS: 20000, Kind: events.EventRemovePresentation, PodID: "pod2", Presentation: deckA}, // not current: nothing
		share(30000, "pod2", deckA),                                                           // B/2 11 000–30 000 shown; A/3 from 30 000 to the end
	}}
	got, err := slides(fsys, nil, ev, 0, onePeriod, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Deck refs by first share: A is d-1, B is d-2.
	want := []meeting.Slide{
		{TMS: 1000, Deck: "d-1", Page: 1, Text: "A one"},
		{TMS: 6000, Deck: "d-1", Page: 2, Text: "A two"},
		{TMS: 11000, Deck: "d-2", Page: 2, Text: "B two"},
		{TMS: 30000, Deck: "d-1", Page: 3, Text: "A three"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slides\n got %+v\nwant %+v", got, want)
	}
}

// Periods clip showings; short pieces in two periods are not added up; a
// showing that began before a period counts from the period start; the
// earliest qualifying showing gives t_ms.
func TestSlidesPeriods(t *testing.T) {
	fsys := slideFS(map[string]string{deckA + "/1": "A one", deckA + "/2": "A two"})
	ps := []meeting.Period{{StartMS: 0, EndMS: 10000}, {StartMS: 20000, EndMS: 40000, RecordedOffsetMS: 10000}}
	ev := &events.Events{Slides: []events.Slide{
		share(-5000, "pod", deckA),          // A/1 from before the first period: 0–7 000 in period 1
		gotoSlide(7000, "pod", deckA, "1"),  // A/2 7 000–23 000: 3 000 + 3 000 ms, not shown
		gotoSlide(23000, "pod", deckA, "0"), // A/1 again, 23 000–40 000; earliest stays 0
	}}
	got, err := slides(fsys, nil, ev, 0, ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []meeting.Slide{{TMS: 0, Deck: "d-1", Page: 1, Text: "A one"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slides\n got %+v\nwant %+v", got, want)
	}
}

// The default deck, an unsafe deck ID, a missing file, the placeholder and
// an empty file give no slide; deck refs count only decks with slides.
func TestSlidesSkipped(t *testing.T) {
	const unsafe = "../etc"
	fsys := slideFS(map[string]string{
		deckA + "/1": "Default welcome",
		deckB + "/1": "No text could be retrieved for the slide\n",
		deckB + "/2": " \n\t\n",
	})
	deckC := "cccccccccccccccccccccccccccccccccccccccc-1761732000300"
	p, f := textFile(deckC, "1", "C one")
	fsys[p] = f
	ev := &events.Events{Slides: []events.Slide{
		{TS: 0, Kind: events.EventConversionDone, PodID: "pod", Presentation: deckA, OriginalFilename: "default.pdf"},
		share(0, "pod", deckA),
		share(10000, "pod", unsafe),
		share(20000, "pod", deckB),
		gotoSlide(30000, "pod", deckB, "1"),
		gotoSlide(40000, "pod", deckB, "2"), // page 3: no file
		share(50000, "pod", deckC),
	}}
	got, err := slides(fsys, nil, ev, 0, onePeriod, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []meeting.Slide{{TMS: 50000, Deck: "d-1", Page: 1, Text: "C one"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slides\n got %+v\nwant %+v", got, want)
	}
}

// A read error other than a missing file is returned, so the step is retried.
func TestSlidesReadError(t *testing.T) {
	ev := &events.Events{Slides: []events.Slide{share(0, "pod", deckA)}}
	_, err := slides(errFS{}, nil, ev, 0, onePeriod, nil)
	if !errors.Is(err, errBroken) {
		t.Errorf("err = %v, want errBroken", err)
	}
}

var errBroken = errors.New("broken store")

type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, errBroken }

// Only the first 64 KiB are read; the text is cut to 1 000 code points.
func TestSlideTextLimits(t *testing.T) {
	long := strings.Repeat("ä", 600) + "\n" + strings.Repeat("ö", 600)
	p, f := textFile(deckA, "1", long)
	got, err := slideText(fstest.MapFS{p: f}, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := string([]rune(strings.Repeat("ä", 600) + "\n" + strings.Repeat("ö", 600))[:1000]); got != want {
		t.Errorf("text has %d code points, want the first 1 000", len([]rune(got)))
	}
	// The cut never leaves trailing line feeds or spaces.
	p, f = textFile(deckA, "3", strings.Repeat("a", 999)+"\nmore text")
	if got, err := slideText(fstest.MapFS{p: f}, p); err != nil || got != strings.Repeat("a", 999) {
		t.Errorf("text ends %q, want the 999 letters without the line feed", got[max(len(got)-5, 0):])
	}
	// One leading byte order mark is dropped, so a BOM before the placeholder gives no slide.
	p, f = textFile(deckA, "4", "\ufeffNo text could be retrieved for the slide")
	if got, err := slideText(fstest.MapFS{p: f}, p); err != nil || got != "" {
		t.Errorf("BOM + placeholder: text = %q, %v; want \"\"", got, err)
	}
	p, f = textFile(deckA, "5", "\ufeffAgenda")
	if got, _ := slideText(fstest.MapFS{p: f}, p); got != "Agenda" {
		t.Errorf("BOM + text: text = %q, want \"Agenda\"", got)
	}
	// A placeholder after 64 KiB of spaces is never read.
	p, f = textFile(deckA, "2", strings.Repeat(" ", 64<<10)+"hidden")
	if got, err := slideText(fstest.MapFS{p: f}, p); err != nil || got != "" {
		t.Errorf("text = %q, %v; want \"\" (only 64 KiB read)", got, err)
	}
}
