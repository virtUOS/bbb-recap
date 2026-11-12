// Package summarize implements the summarize step (docs/reference/templates.md
// §3, §4, §7; providers.md §3.5): it turns the transcript and the declared
// meeting inputs into the summary.v1 JSON of the job's format.
//
// # Call plan
//
// If the whole input fits the budget of the LLM profile, one summarize call
// returns summary.v1. Otherwise the transcript lines are cut into chunks
// (internal/llmplan), one extract call per chunk returns chunk-notes.v1 (map
// calls run concurrently up to max_inflight), and one consolidate call over
// the chunk notes, the chat, the shared notes, the polls and the gap list
// returns summary.v1. Chat, notes and polls never go to extract calls.
//
// Reduce levels: the consolidate call is reduce level 1 when the chunk notes
// fit its budget. If they do not, the notes are packed into groups that fit
// a merge call (chunk notes in, chunk-notes.v1 out; a group of one note is
// passed on as is) and the consolidate is tried again one level higher. At
// most 3 levels (providers.md §1.5): if the consolidate does not fit at
// level 3, the step fails rejected_by_provider (docs/reference/README.md "HTTP").
//
// Re-split: a map call that fails with output_truncated or input_too_large
// is split in half once (llmplan.Halve) and both halves are sent; a second
// such failure fails the step rejected_by_provider. A single summarize call
// that fails that way falls back to the map plan over the two halves of the
// transcript, which count as already re-split. A consolidate or merge call
// that fails that way fails the step rejected_by_provider.
//
// No segments: no LLM call; the result says "no speech" and render writes
// the notice (templates.md §4).
//
// # What reaches the LLM
//
// Only the declared inputs of templates.md §3, taken from the normalized
// meeting and the transcript: meeting name, transcript lines
// "[hh:mm:ss] Name: text" in recorded time, the gap list (speaker and span),
// public chat inside recording periods, shared notes as text and poll
// aggregates. Input has no field for source user IDs, owner identity or
// per-user poll answers, so they cannot be sent. Content sits in delimited
// sections of the user message; the system message (prompts/<call>.md of
// the format) says they are data and names the output language.
//
// Nothing here logs content: only job IDs, call names and counts.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/llmplan"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
)

// Step is the step name.
const Step = "summarize"

// LLM call names (templates.md §4; merge is the group reduce of §3.5 step 3).
const (
	CallSummarize   = "summarize"
	CallExtract     = "extract"
	CallMerge       = "merge"
	CallConsolidate = "consolidate"
)

// Reply schemas of the format (templates.md §4).
const (
	SchemaSummary    = "summary.v1"
	SchemaChunkNotes = "chunk-notes.v1"
)

// Input is everything the LLM may see (templates.md §3).
type Input struct {
	MeetingName string
	Transcript  *assemble.Transcript
	Chat        []ChatMessage  // public chat; messages outside recording periods are dropped here
	Notes       string         // shared notes as plain text; "" when absent
	Polls       []meeting.Poll // aggregates only
}

// ChatMessage is one public chat message: time on the meeting axis, the
// sender's display name ("" if unknown) and the plain text.
type ChatMessage struct {
	AtMS int64
	Name string
	Text string
}

// Result is the outcome of a run.
type Result struct {
	Summary  []byte // summary.v1 JSON after post-processing; nil when NoSpeech
	NoSpeech bool
	Model    string // model reported by the last LLM call
	Calls    int    // LLM calls made (repair calls not counted)
}

// Rejected is the failure rejected_by_provider: the provider refused the
// input after re-split, or the reduce needed more than 3 levels.
type Rejected struct {
	Class providers.ErrorClass // the adapter class that caused it; "" for the level limit
	Code  string
}

func (e *Rejected) Error() string { return "rejected by provider: " + e.Code }

// Summarizer runs the call plan against one LLM profile and one format.
type Summarizer struct {
	LLM     providers.LLM
	Profile *providers.Profile
	Format  *format.Format
	// Sem limits concurrent LLM calls (max_inflight per worker process,
	// architecture.md §7). Nil: a new one of the profile's max_inflight.
	Sem chan struct{}
	Log *slog.Logger
}

// run is the state of one Run.
type run struct {
	*Summarizer
	jobID   string
	lang    string
	in      Input
	mu      sync.Mutex
	calls   int
	model   string
	prompts map[string]string
}

// Run summarizes in for job jobID in the output language lang (templates.md
// §7). Errors are *providers.Error (retryable, invalid_response,
// permanent_config) or *Rejected; Outcome maps them to a step result.
func (s *Summarizer) Run(ctx context.Context, jobID string, in Input, lang string) (*Result, error) {
	if in.Transcript == nil || len(in.Transcript.Segments) == 0 {
		s.Log.Debug("summarize: no speech, no LLM call", "job_id", jobID)
		return &Result{NoSpeech: true}, nil
	}
	if s.Sem == nil {
		s.Sem = make(chan struct{}, max(s.Profile.MaxInflight, 1))
	}
	r := &run{Summarizer: s, jobID: jobID, lang: lang, in: in, prompts: map[string]string{}}
	if err := r.loadPrompts(); err != nil {
		return nil, err
	}
	summary, err := r.plan(ctx)
	if err != nil {
		return nil, err
	}
	out, err := postProcess(summary, recordedEnd(in.Transcript.RecordingPeriods))
	if err != nil {
		return nil, &providers.Error{Class: providers.InvalidResponse, Code: "summary_shape"}
	}
	return &Result{Summary: out, Model: r.model, Calls: r.calls}, nil
}

// loadPrompts reads the system prompts and checks the schemas of the format.
func (r *run) loadPrompts() error {
	name := r.lang
	if t := r.Format.Strings(r.lang); t["language_name"] != "" {
		name = r.lang + " (" + t["language_name"] + ")"
	}
	for _, call := range []string{CallSummarize, CallExtract, CallMerge, CallConsolidate} {
		p, ok := r.Format.Prompt(call)
		if !ok {
			return &providers.Error{Class: providers.PermanentConfig, Code: "format_prompt_missing"}
		}
		r.prompts[call] = strings.ReplaceAll(p, "{language}", name)
	}
	for _, sch := range []string{SchemaSummary, SchemaChunkNotes} {
		if _, ok := r.Format.Schema(sch); !ok {
			return &providers.Error{Class: providers.PermanentConfig, Code: "format_schema_missing"}
		}
	}
	return nil
}

func (r *run) schema(name string) json.RawMessage {
	s, _ := r.Format.Schema(name)
	return s
}

// budget is the §3.5 budget for the data of a call whose system prompt and
// schema are fixed. The schema counts in every mode (the adapter puts it
// into the system message in two of them).
func (r *run) budget(call, schema string) int {
	return llmplan.Budget(r.Profile.ContextTokens, r.Profile.MaxOutputTokens,
		utf8.RuneCountInString(r.prompts[call])+utf8.RuneCount(r.schema(schema)))
}

// plan runs the call plan and returns the summary.v1 reply.
func (r *run) plan(ctx context.Context) (json.RawMessage, error) {
	lines := r.transcriptLines()
	user := joinSections(section("MEETING NAME", r.in.MeetingName), section("TRANSCRIPT", strings.Join(lines, "\n")),
		r.contextSections())
	if llmplan.TextTokens(user) <= r.budget(CallSummarize, SchemaSummary) {
		r.Log.Debug("summarize: single call", "job_id", r.jobID, "lines", len(lines))
		reply, err := r.complete(ctx, CallSummarize, "0", user, SchemaSummary)
		if !resplittable(err) {
			return reply, err
		}
		a, b, ok := llmplan.Halve(lines)
		if !ok {
			return nil, rejected(err)
		}
		r.Log.Debug("summarize: single call refused, map over two halves", "job_id", r.jobID)
		return r.mapReduce(ctx, [][]string{a, b}, false)
	}
	chunks := llmplan.Chunk(lines, r.extractBudget())
	r.Log.Debug("summarize: map-reduce", "job_id", r.jobID, "lines", len(lines), "chunks", len(chunks))
	return r.mapReduce(ctx, chunks, true)
}

// extractUser is the user message of an extract call.
func extractUser(part string, lines []string) string {
	return joinSections(section("PART", part), section("TRANSCRIPT", strings.Join(lines, "\n")))
}

// extractBudget is the budget for the transcript lines of one extract call:
// the call's budget less everything around the lines in the user message,
// measured on a placeholder (section returns "" for empty data). "000 of
// 000" is the widest PART label in practice.
func (r *run) extractBudget() int {
	around := utf8.RuneCountInString(extractUser("000 of 000", []string{"x"})) - 1
	return r.budget(CallExtract, SchemaChunkNotes) - llmplan.Tokens(around)
}

// mapReduce runs one extract call per chunk, then the reduce.
func (r *run) mapReduce(ctx context.Context, chunks [][]string, mayResplit bool) (json.RawMessage, error) {
	results := make([][]json.RawMessage, len(chunks))
	errs := make([]error, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Go(func() { results[i], errs[i] = r.extract(ctx, c, i, len(chunks), mayResplit) })
	}
	wg.Wait()
	var notes []json.RawMessage
	for i := range chunks {
		if errs[i] != nil {
			return nil, errs[i]
		}
		notes = append(notes, results[i]...)
	}
	return r.reduce(ctx, notes)
}

// extract sends one chunk; on output_truncated or input_too_large it sends
// its two halves instead, once.
func (r *run) extract(ctx context.Context, chunk []string, i, n int, mayResplit bool) ([]json.RawMessage, error) {
	part := strconv.Itoa(i+1) + " of " + strconv.Itoa(n)
	user := func(lines []string) string { return extractUser(part, lines) }
	ref := strconv.Itoa(i)
	notes, err := r.complete(ctx, CallExtract, ref, user(chunk), SchemaChunkNotes)
	if !resplittable(err) {
		return []json.RawMessage{notes}, err
	}
	a, b, ok := llmplan.Halve(chunk)
	if !mayResplit || !ok {
		return nil, rejected(err)
	}
	r.Log.Debug("summarize: re-split", "job_id", r.jobID, "chunk", i)
	var out []json.RawMessage
	for k, half := range [][]string{a, b} {
		notes, err := r.complete(ctx, CallExtract, ref+"."+strconv.Itoa(k+1), user(half), SchemaChunkNotes)
		if resplittable(err) {
			return nil, rejected(err)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, notes)
	}
	return out, nil
}

// reduce merges chunk notes in groups until they fit one consolidate call,
// at most llmplan.MaxReduceLevels levels (§3.5 step 3).
func (r *run) reduce(ctx context.Context, notes []json.RawMessage) (json.RawMessage, error) {
	for level := 1; ; level++ {
		wrapped := wrapNotes(notes)
		user := r.consolidateUser(wrapped)
		if llmplan.TextTokens(user) <= r.budget(CallConsolidate, SchemaSummary) {
			r.Log.Debug("summarize: consolidate", "job_id", r.jobID, "level", level, "notes", len(notes))
			reply, err := r.complete(ctx, CallConsolidate, "0", user, SchemaSummary)
			if resplittable(err) {
				return nil, rejected(err)
			}
			return reply, err
		}
		if level == llmplan.MaxReduceLevels {
			return nil, &Rejected{Code: "reduce_levels"}
		}
		groups := llmplan.Groups(wrapped, r.budget(CallMerge, SchemaChunkNotes))
		if len(groups) == len(wrapped) {
			return nil, &Rejected{Code: "reduce_no_progress"} // every note alone fills a merge call
		}
		r.Log.Debug("summarize: merge", "job_id", r.jobID, "level", level, "notes", len(notes), "groups", len(groups))
		merged := make([]json.RawMessage, len(groups))
		errs := make([]error, len(groups))
		var wg sync.WaitGroup
		first := 0
		for g, group := range groups {
			if len(group) == 1 {
				merged[g] = notes[first]
			} else {
				ref := strconv.Itoa(level) + "." + strconv.Itoa(g)
				wg.Go(func() {
					merged[g], errs[g] = r.complete(ctx, CallMerge, ref, strings.Join(group, "\n"), SchemaChunkNotes)
				})
			}
			first += len(group)
		}
		wg.Wait()
		for _, err := range errs {
			if resplittable(err) {
				return nil, rejected(err)
			}
			if err != nil {
				return nil, err
			}
		}
		notes = merged
	}
}

// wrapNotes puts each chunk note into its own NOTES n section.
func wrapNotes(notes []json.RawMessage) []string {
	wrapped := make([]string, len(notes))
	for i, n := range notes {
		wrapped[i] = section("NOTES "+strconv.Itoa(i+1), string(n))
	}
	return wrapped
}

// consolidateUser is the user message of the consolidate call.
func (r *run) consolidateUser(wrapped []string) string {
	return joinSections(section("MEETING NAME", r.in.MeetingName), r.contextSections(), strings.Join(wrapped, "\n"))
}

// complete makes one LLM call with the system prompt of call, waiting for a
// free slot of the profile.
func (r *run) complete(ctx context.Context, call, chunk, user, schema string) (json.RawMessage, error) {
	select {
	case r.Sem <- struct{}{}:
		defer func() { <-r.Sem }()
	case <-ctx.Done():
		return nil, &providers.Error{Class: providers.Retryable, Code: "cancelled"}
	}
	resp, err := r.LLM.Complete(ctx, providers.LlmRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: r.prompts[call]},
			{Role: providers.RoleUser, Content: user},
		},
		Output:          providers.Output{Type: providers.OutputJSON, Schema: r.schema(schema)},
		MaxOutputTokens: r.Profile.MaxOutputTokens,
		CallRef:         r.jobID + ":" + Step + ":" + call + ":" + chunk,
	})
	r.mu.Lock()
	r.calls++
	if resp != nil {
		r.model = resp.Model
	}
	r.mu.Unlock()
	if err != nil {
		var pe *providers.Error
		if !errors.As(err, &pe) {
			return nil, &providers.Error{Class: providers.Retryable, Code: "llm_internal"}
		}
		return nil, pe
	}
	return resp.JSON, nil
}

// resplittable reports the classes that §3.5 step 4 handles by re-splitting.
func resplittable(err error) bool {
	var pe *providers.Error
	return errors.As(err, &pe) && (pe.Class == providers.OutputTruncated || pe.Class == providers.InputTooLarge)
}

func rejected(err error) *Rejected {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return &Rejected{Class: pe.Class, Code: pe.Code}
	}
	return &Rejected{Code: "resplit"}
}

// --- input rendering (templates.md §3) ---------------------------------------

// transcriptLines renders the segments as "[hh:mm:ss] Name: text" in
// recorded time.
func (r *run) transcriptLines() []string {
	tr := r.in.Transcript
	names := map[string]string{}
	for _, s := range tr.Speakers {
		names[s.Ref] = r.speakerName(s)
	}
	lines := make([]string, 0, len(tr.Segments))
	for _, seg := range tr.Segments {
		lines = append(lines, line(recorded(seg.StartMS, tr.RecordingPeriods), names[seg.SpeakerRef], seg.Text))
	}
	return lines
}

// speakerName is the display name, else "Speaker n" from the string table
// with n from spk-<n> (templates.md §5.4).
func (r *run) speakerName(s assemble.Speaker) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	label := r.Format.Strings(r.lang)["speaker"]
	if label == "" {
		label = "Speaker {n}"
	}
	return strings.ReplaceAll(label, "{n}", strings.TrimPrefix(s.Ref, "spk-"))
}

// contextSections renders gaps, chat, shared notes and polls; each is
// omitted when empty.
func (r *run) contextSections() string {
	tr := r.in.Transcript
	periods := tr.RecordingPeriods
	names := map[string]string{}
	for _, s := range tr.Speakers {
		names[s.Ref] = r.speakerName(s)
	}
	var gaps []string
	for _, g := range tr.Gaps {
		gaps = append(gaps, "- "+oneLine(names[g.SpeakerRef])+": no transcript from ["+
			hms(recorded(g.StartMS, periods))+"] to ["+hms(recorded(g.EndMS, periods))+"]")
	}
	var chat []string
	for _, c := range r.in.Chat {
		if inPeriods(c.AtMS, periods) {
			chat = append(chat, line(recorded(c.AtMS, periods), c.Name, c.Text))
		}
	}
	var polls []string
	for i, p := range r.in.Polls {
		var b strings.Builder
		b.WriteString("Poll " + strconv.Itoa(i+1) + ": " + oneLine(p.Question))
		for _, o := range p.Options {
			b.WriteString("\n- " + oneLine(o.Text) + ": " + strconv.Itoa(o.Votes) + " votes")
		}
		polls = append(polls, b.String())
	}
	return joinSections(
		section("GAPS", strings.Join(gaps, "\n")),
		section("CHAT", strings.Join(chat, "\n")),
		section("SHARED NOTES", strings.TrimSpace(r.in.Notes)),
		section("POLLS", strings.Join(polls, "\n\n")),
	)
}

// line is "[hh:mm:ss] Name: text" ("[hh:mm:ss] text" without a name).
func line(ms int64, name, text string) string {
	if name == "" {
		return "[" + hms(ms) + "] " + oneLine(text)
	}
	return "[" + hms(ms) + "] " + oneLine(name) + ": " + oneLine(text)
}

// oneLine turns CR, LF and tabs into spaces, so one item stays one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)), " ")
}

// section fences data: "<<<BEGIN NAME>>>", the data, "<<<END NAME>>>". The
// data cannot contain a fence: runs of three angle brackets are broken up.
// Empty data gives "".
func section(name, data string) string {
	if data == "" {
		return ""
	}
	for strings.Contains(data, "<<<") || strings.Contains(data, ">>>") {
		data = strings.ReplaceAll(strings.ReplaceAll(data, "<<<", "<< <"), ">>>", "> >>")
	}
	return "<<<BEGIN " + name + ">>>\n" + data + "\n<<<END " + name + ">>>"
}

// joinSections joins the non-empty parts with a blank line.
func joinSections(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// --- time (providers.md §4.3, templates.md §5.3) ------------------------------

// recorded maps a time on the meeting axis to recorded time: inside a
// period t − start + recorded_offset; in a pause or before the first period
// the recorded time where the next period starts; after the last period the
// recorded end.
func recorded(t int64, periods []meeting.Period) int64 {
	for _, p := range periods {
		if t < p.StartMS {
			return p.RecordedOffsetMS
		}
		if t <= p.EndMS {
			return t - p.StartMS + p.RecordedOffsetMS
		}
	}
	return recordedEnd(periods)
}

// recordedEnd is the recorded length of the meeting.
func recordedEnd(periods []meeting.Period) int64 {
	if len(periods) == 0 {
		return 0
	}
	p := periods[len(periods)-1]
	return p.EndMS - p.StartMS + p.RecordedOffsetMS
}

func inPeriods(t int64, periods []meeting.Period) bool {
	for _, p := range periods {
		if t >= p.StartMS && t <= p.EndMS {
			return true
		}
	}
	return false
}

// hms formats ms as hh:mm:ss, seconds cut.
func hms(ms int64) string {
	s := max(ms, 0) / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

// --- after the LLM (templates.md §4) -----------------------------------------

// Summary is summary.v1 (templates.md §4).
type Summary struct {
	Overview      string       `json:"overview"`
	Topics        []Topic      `json:"topics"`
	Decisions     []Decision   `json:"decisions"`
	ActionItems   []ActionItem `json:"action_items"`
	OpenQuestions []Question   `json:"open_questions"`
}

// Topic is one topic of summary.v1.
type Topic struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Start   string `json:"start,omitempty"`
}

// Decision is one decision of summary.v1.
type Decision struct {
	Text string `json:"text"`
	At   string `json:"at,omitempty"`
}

// ActionItem is one action item of summary.v1.
type ActionItem struct {
	Task  string `json:"task"`
	Owner string `json:"owner,omitempty"`
	Due   string `json:"due,omitempty"`
	At    string `json:"at,omitempty"`
}

// Question is one open question of summary.v1.
type Question struct {
	Text string `json:"text"`
}

// postProcess removes start/at values after the recorded end and returns
// the summary as deterministic JSON.
func postProcess(reply json.RawMessage, endMS int64) ([]byte, error) {
	var s Summary
	if err := json.Unmarshal(reply, &s); err != nil {
		return nil, err
	}
	late := func(v string) string {
		if len(v) != 8 {
			return ""
		}
		h, err1 := strconv.Atoi(v[0:2])
		m, err2 := strconv.Atoi(v[3:5])
		sec, err3 := strconv.Atoi(v[6:8])
		if err1 != nil || err2 != nil || err3 != nil || int64((h*60+m)*60+sec) > endMS/1000 {
			return ""
		}
		return v
	}
	for i := range s.Topics {
		s.Topics[i].Start = late(s.Topics[i].Start)
	}
	for i := range s.Decisions {
		s.Decisions[i].At = late(s.Decisions[i].At)
	}
	for i := range s.ActionItems {
		s.ActionItems[i].At = late(s.ActionItems[i].At)
	}
	if s.Topics == nil {
		s.Topics = []Topic{}
	}
	if s.Decisions == nil {
		s.Decisions = []Decision{}
	}
	if s.ActionItems == nil {
		s.ActionItems = []ActionItem{}
	}
	if s.OpenQuestions == nil {
		s.OpenQuestions = []Question{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Language is the output language (templates.md §7): the transcript's
// language if the format has a string table for it, else def
// (RECAP_DEFAULT_LANGUAGE).
func Language(f *format.Format, transcriptLang *string, def string) string {
	if transcriptLang != nil && f.Strings(*transcriptLang) != nil {
		return *transcriptLang
	}
	return def
}
