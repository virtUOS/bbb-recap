// Package render is the render core (docs/reference/templates.md §5, §6, §7):
// it turns a transcript, a summary.v1 document (or "no speech"), the meeting
// name and the poll aggregates into summary.md, transcript.vtt and the
// document title. Rendering is deterministic: the same input gives the same
// bytes.
//
// Every fixed user-facing string comes from the string table of the output
// language (strings/<lang>.json of the format, keys in Keys). Templates in
// the table use {name} placeholders; the values put into them are escaped
// first.
//
// The package does not log: everything it handles is content.
package render

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
)

// Keys are the entries every string table of a format must have.
var Keys = []string{
	"speaker", "header", "notice", "gap", "overview", "topics", "decisions", "action_items", "open_questions",
	"polls", "none", "due", "option", "votes", "no_speech", "no_llm", "footer",
	"vtt_header", "vtt_pause", "vtt_gap",
}

// ErrStrings means the format has no complete string table for the output
// language: a configuration error.
var ErrStrings = errors.New("render: string table missing or incomplete")

// MaxTitle is the length of the title in characters (templates.md §5.1).
const MaxTitle = 200

// Input is everything render uses.
type Input struct {
	MeetingName string
	Transcript  *assemble.Transcript
	Summary     *summarize.Summary // nil: no speech (templates.md §4)
	Polls       []meeting.Poll     // aggregates only
	LLMModel    string             // "" when no LLM call was made
	GeneratedAt time.Time          // the job's created_at (templates.md §5.4)
	Format      *format.Format
	Language    string         // output language (templates.md §7)
	Zone        *time.Location // RECAP_DISPLAY_TZ
}

// Output is the rendered document, transcript and title.
type Output struct {
	SummaryMD     []byte
	TranscriptVTT []byte
	Title         string
}

// Render renders in. Errors are ErrStrings or a bad transcript time base.
func Render(in Input) (*Output, error) {
	s, err := Strings(in.Format, in.Language)
	if err != nil {
		return nil, err
	}
	origin, err := time.Parse(time.RFC3339Nano, in.Transcript.TimeBase.OriginUTC)
	if err != nil {
		return nil, fmt.Errorf("render: time_base.origin_utc: %w", err)
	}
	r := &renderer{in: in, s: s, origin: origin}
	return &Output{
		SummaryMD:     []byte(r.summaryMD()),
		TranscriptVTT: []byte(r.transcriptVTT()),
		Title:         Title(in.MeetingName, origin, in.Zone),
	}, nil
}

// Strings returns the string table of lang and checks that it has every key.
func Strings(f *format.Format, lang string) (map[string]string, error) {
	t := f.Strings(lang)
	for _, k := range Keys {
		if t[k] == "" {
			return nil, ErrStrings
		}
	}
	return t, nil
}

// Title is "{meeting_name} {date}" (templates.md §5.1): control characters
// become spaces, runs of spaces collapse, and a title over MaxTitle
// characters has its meeting name cut so it is exactly MaxTitle long,
// ending with "…".
func Title(meetingName string, origin time.Time, zone *time.Location) string {
	date := origin.In(zone).Format(time.DateOnly)
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, meetingName)
	name = spaces.ReplaceAllString(name, " ")
	if utf8.RuneCountInString(name)+1+len(date) > MaxTitle {
		name = string([]rune(name)[:MaxTitle-1-len(date)-1]) + "…"
	}
	return name + " " + date
}

var spaces = regexp.MustCompile(` +`)

// renderer holds one Render call.
type renderer struct {
	in     Input
	s      map[string]string
	origin time.Time
}

// fill replaces the {name} placeholders of a string-table template with
// values[name] in one pass (a value is not scanned again). The braces keep
// the placeholders apart, so the map order does not matter.
func fill(tmpl string, values map[string]string) string {
	var args []string
	for name, v := range values {
		args = append(args, "{"+name+"}", v)
	}
	return strings.NewReplacer(args...).Replace(tmpl)
}

// speakerName is the display name of ref, else "Speaker n" from the string
// table with n from spk-<n> (templates.md §5.4, §6).
func (r *renderer) speakerName(ref string) string {
	for _, sp := range r.in.Transcript.Speakers {
		if sp.Ref == ref && sp.DisplayName != "" {
			return sp.DisplayName
		}
	}
	return fill(r.s["speaker"], map[string]string{"n": strings.TrimPrefix(ref, "spk-")})
}

// --- time (providers.md §4.3, templates.md §5.3) ------------------------------

// recorded maps a time on the meeting axis to recorded time. Inside a
// period it is t − start + recorded_offset (the first period containing t
// wins); in a pause the recorded time where the next period starts; after
// the last period the recorded end.
func recorded(t int64, periods []meeting.Period) int64 {
	for _, p := range periods {
		if t < p.StartMS {
			return p.RecordedOffsetMS
		}
		if t <= p.EndMS {
			return t - p.StartMS + p.RecordedOffsetMS
		}
	}
	if len(periods) == 0 {
		return 0
	}
	p := periods[len(periods)-1]
	return p.EndMS - p.StartMS + p.RecordedOffsetMS
}

// hms formats ms as hh:mm:ss, seconds cut.
func hms(ms int64) string {
	s := max(ms, 0) / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

// vttTime formats ms as hh:mm:ss.ttt, hours at least two digits (§6).
func vttTime(ms int64) string {
	ms = max(ms, 0)
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", s/3600, s/60%60, s%60, ms%1000)
}
