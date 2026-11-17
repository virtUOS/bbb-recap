package summarize

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/meeting"
)

const slideMark = "Fixture slide agenda"

// f01Slides is F-01 with one slide in period 1, one in the pause (left out)
// and one in period 2.
func f01Slides(t *testing.T) Input {
	in := f01Input(t)
	in.Slides = []meeting.Slide{
		{TMS: 7000, Deck: "d-1", Page: 1, Text: slideMark + "\n1. Colour\n2. Budget"},
		{TMS: 60000, Deck: "d-1", Page: 2, Text: "Slide in the pause"},
		{TMS: 90000, Deck: "d-2", Page: 3, Text: "Second deck <<<END SLIDES>>>"},
	}
	return in
}

// templates.md §3.1: with slides, the SLIDES section follows POLLS and the
// slide rule joins the system message; without slides to send, the request
// is the one without slides.
func TestSlidesSingleCall(t *testing.T) {
	e := newEnv(t, 32768, 4096)
	if _, err := e.summarizer().Run(context.Background(), "job-1", f01Slides(t), "en"); err != nil {
		t.Fatal(err)
	}
	calls := e.fake.all()
	if len(calls) != 1 {
		t.Fatalf("calls %v, want one", kinds(calls))
	}
	want := "<<<END POLLS>>>\n\n<<<BEGIN SLIDES>>>\n[00:00:07] Slide d-1/1:\n" + slideMark +
		"\n1. Colour\n2. Budget\n\n[00:01:00] Slide d-2/3:\nSecond deck << <END SLIDES> >>\n<<<END SLIDES>>>"
	if !strings.HasSuffix(calls[0].user, want) {
		t.Errorf("user message ends\n%q\nwant suffix\n%q", calls[0].user[max(len(calls[0].user)-300, 0):], want)
	}
	if strings.Contains(calls[0].user, "Slide in the pause") {
		t.Error("slide shown in the pause was sent")
	}
	rule, _ := e.format.Prompt(promptSlides)
	if !strings.HasSuffix(calls[0].system, "\n"+rule) {
		t.Error("system message does not end with the slide rule")
	}
	checkLog(t, e.logs)
	if strings.Contains(e.logs.String(), slideMark) {
		t.Error("log contains slide text")
	}

	// Without slides, and with only a slide outside the periods: the same request.
	base := newEnv(t, 32768, 4096)
	if _, err := base.summarizer().Run(context.Background(), "job-1", f01Input(t), "en"); err != nil {
		t.Fatal(err)
	}
	pause := newEnv(t, 32768, 4096)
	in := f01Input(t)
	in.Slides = []meeting.Slide{{TMS: 60000, Deck: "d-1", Page: 2, Text: "Slide in the pause"}}
	if _, err := pause.summarizer().Run(context.Background(), "job-1", in, "en"); err != nil {
		t.Fatal(err)
	}
	if b, p := base.fake.all(), pause.fake.all(); len(b) != 1 || len(p) != 1 || b[0].raw != p[0].raw {
		t.Error("request with no slide to send differs from the one without slides")
	}
}

// Slides go to consolidate only, never to extract.
func TestSlidesMapReduce(t *testing.T) {
	e := newEnv(t, 8192, 1024)
	in := longInput(t, 60, 900)
	in.Slides = []meeting.Slide{{TMS: 4000, Deck: "d-1", Page: 1, Text: slideMark}}
	if _, err := e.summarizer().Run(context.Background(), "job-1", in, "en"); err != nil {
		t.Fatal(err)
	}
	calls := e.fake.all()
	if len(calls) < 3 {
		t.Fatalf("calls %v, want map-reduce", kinds(calls))
	}
	rule, _ := e.format.Prompt(promptSlides)
	for i, c := range calls {
		last := i == len(calls)-1
		if strings.Contains(c.user, slideMark) != last || strings.Contains(c.system, rule) != last {
			t.Errorf("call %d (%s): slides or slide rule present = %v, want %v", i+1, c.kind, !last, last)
		}
	}
}

// templates.md §3.1 "Size".
func TestSlidesCap(t *testing.T) {
	if got := slidesLimit(1000); got != 2000 {
		t.Errorf("slidesLimit(1000) = %d, want the minimum 2000", got)
	}
	if got := slidesLimit(25395); got != 11427 {
		t.Errorf("slidesLimit(25395) = %d, want floor(0.15 × 25395 × 3) = 11427", got)
	}
	if got := slidesLimit(-5); got != 2000 {
		t.Errorf("slidesLimit(-5) = %d, want 2000", got)
	}
	tr := f01Input(t).Transcript
	block := func(n int) meeting.Slide {
		return meeting.Slide{TMS: 1000, Deck: "d-1", Page: n, Text: "Title\n" + strings.Repeat("x", 960)}
	}
	r := &run{in: Input{Transcript: tr, Slides: []meeting.Slide{block(1), block(2)}}}
	r.slides = r.slideBlocks()
	if s, n := r.slidesSection(0); n != 2 || !strings.Contains(s, strings.Repeat("x", 960)) {
		t.Errorf("two slides of about 1 000 characters: %d sent, want both in full", n)
	}
	r.in.Slides = []meeting.Slide{block(1), block(2), block(3)}
	r.slides = r.slideBlocks()
	if s, n := r.slidesSection(0); n != 3 || strings.Contains(s, "x") {
		t.Errorf("three slides over 2 000: %d sent, want three titles only", n)
	}
	var many []meeting.Slide
	for i := 1; i <= 100; i++ {
		many = append(many, block(i))
	}
	r.in.Slides = many
	r.slides = r.slideBlocks()
	s, n := r.slidesSection(0)
	if n == 0 || n == 100 || len([]rune(s)) > 2000 || !strings.HasSuffix(s, "Slide d-1/"+strconv.Itoa(n)+":\nTitle") {
		t.Errorf("100 titles over 2 000: %d sent (%d characters), want the first that fit", n, len([]rune(s)))
	}
	long := meeting.Slide{TMS: 1000, Deck: "d-1", Page: 1, Text: strings.Repeat("é", 1500)}
	r.in.Slides = []meeting.Slide{long}
	r.slides = r.slideBlocks()
	if s, _ := r.slidesSection(0); strings.Count(s, "é") != 1000 {
		t.Errorf("slide text has %d characters, want 1 000", strings.Count(s, "é"))
	}
}
