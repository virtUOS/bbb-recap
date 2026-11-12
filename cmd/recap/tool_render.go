package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/render"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
)

const toolRenderUsage = "Usage: recap tool render --out DIR [--summarize] < input.json\n" +
	"       recap tool render --check-schema FORMAT/SCHEMA < reply.json"

// Defaults of the variables the tool reads (deployment.md §5).
const (
	defaultMeetingTypesFile = "/etc/recap/meeting-types.yaml"
	defaultFormatsDir       = "/etc/recap/formats"
)

var languageRE = regexp.MustCompile(`^[a-z]{2}$`)

// renderToolInput is the stdin of `recap tool render`
// (contracts/render/README.md "Input"). BBB user IDs, the owner and
// per-user poll answers are not decoded: only the counts are used.
type renderToolInput struct {
	MeetingType string               `json:"meeting_type"`
	MeetingName string               `json:"meeting_name"`
	Transcript  *assemble.Transcript `json:"transcript"`
	Chat        []struct {
		AtMS int64  `json:"at_ms"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"chat"`
	Notes *string `json:"notes"`
	Polls []struct {
		Question  string   `json:"question"`
		Options   []string `json:"options"`
		Responses []struct {
			Option int `json:"option"`
		} `json:"responses"`
	} `json:"polls"`
	LLMModel    string             `json:"llm_model"`
	GeneratedAt string             `json:"generated_at"`
	Summary     *summarize.Summary `json:"summary"`
}

// toolRender is `recap tool render` (contracts/render/README.md): format
// selection, optionally the summarize step, and the render step for one
// job, without queue, database or blob store; or --check-schema. Logs go
// to stderr at RECAP_LOG_LEVEL and carry no content.
func toolRender(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool render"
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "")
	summarizeFlag := fs.Bool("summarize", false, "")
	check := fs.String("check-schema", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || (*out == "") == (*check == "") ||
		(*check != "" && *summarizeFlag) {
		fmt.Fprintln(stderr, toolRenderUsage)
		return exitUsage
	}
	level := slog.LevelInfo
	if v := os.Getenv("RECAP_LOG_LEVEL"); v != "" {
		var err error
		if level, err = logging.ParseLevel(v); err != nil {
			fmt.Fprintf(stderr, "%s: RECAP_LOG_LEVEL: %v\n", prog, err)
			return exitUsage
		}
	}
	log := logging.New(stderr, level)
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read stdin\n", prog)
		return exitFailure
	}
	formatsDir := envOr("RECAP_FORMATS_DIR", defaultFormatsDir)
	if *check != "" {
		return checkSchema(prog, formatsDir, *check, data, log, stdout, stderr)
	}
	return renderOnce(prog, formatsDir, *out, *summarizeFlag, data, log, stdout, stderr)
}

// checkSchema validates data against FORMAT/SCHEMA and prints {"valid": bool}.
func checkSchema(prog, formatsDir, arg string, data []byte, log *slog.Logger, stdout, stderr io.Writer) int {
	id, name, ok := strings.Cut(arg, "/")
	if !ok {
		fmt.Fprintln(stderr, toolRenderUsage)
		return exitUsage
	}
	f, err := format.Load(formatsDir, id)
	if err != nil {
		fmt.Fprintf(stderr, "%s: format %s: %v\n", prog, id, err)
		return exitUsage
	}
	if _, ok := f.Schema(name); !ok {
		fmt.Fprintf(stderr, "%s: format %s has no schema %s\n", prog, id, name)
		return exitUsage
	}
	valid := f.Validate(name, data) == nil
	log.Debug("checked", "schema", arg, "valid", valid, "bytes", len(data))
	return writeJSON(stdout, map[string]any{"valid": valid})
}

// renderOnce runs format selection, optionally summarize, and render.
func renderOnce(prog, formatsDir, outDir string, withSummarize bool, data []byte, log *slog.Logger,
	stdout, stderr io.Writer) int {
	zone, err := time.LoadLocation(envOr("RECAP_DISPLAY_TZ", "UTC"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: RECAP_DISPLAY_TZ: unknown zone\n", prog)
		return exitUsage
	}
	defLang := envOr("RECAP_DEFAULT_LANGUAGE", "en")
	if !languageRE.MatchString(defLang) {
		fmt.Fprintf(stderr, "%s: RECAP_DEFAULT_LANGUAGE: want an ISO 639-1 code\n", prog)
		return exitUsage
	}
	types, err := readMeetingTypes(envOr("RECAP_MEETING_TYPES_FILE", defaultMeetingTypesFile))
	if err != nil {
		fmt.Fprintf(stderr, "%s: RECAP_MEETING_TYPES_FILE: %v\n", prog, err)
		return exitUsage
	}

	var in renderToolInput
	if err := json.Unmarshal(data, &in); err != nil || in.Transcript == nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object with at least transcript\n", prog)
		return exitUsage
	}
	generated, err := time.Parse(time.RFC3339Nano, in.GeneratedAt)
	if err != nil {
		fmt.Fprintf(stderr, "%s: generated_at: want an RFC 3339 time\n", prog)
		return exitUsage
	}
	if !withSummarize && in.Summary == nil {
		fmt.Fprintf(stderr, "%s: summary: required without --summarize\n", prog)
		return exitUsage
	}
	polls, err := countPolls(in)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitUsage
	}

	// Format selection before any provider call (templates.md §1).
	id, ok := types[in.MeetingType]
	if !ok {
		log.Info("meeting type has no format", "outcome", "hold")
		return writeJSON(stdout, map[string]string{"outcome": string(queue.Hold), "reason": summarize.ReasonUnknownType})
	}
	f, err := format.Load(formatsDir, id)
	if errors.Is(err, format.ErrNotFound) {
		log.Info("format not found", "format_id", id, "outcome", "hold")
		return writeJSON(stdout, map[string]string{"outcome": string(queue.Hold), "reason": summarize.ReasonUnknownType})
	}
	if err != nil {
		log.Error("format invalid", "format_id", id)
		return writeJSON(stdout, map[string]string{"outcome": string(queue.Permanent), "reason": summarize.ReasonConfigError})
	}
	lang := summarize.Language(f, in.Transcript.Language, defLang)

	sum, model := in.Summary, in.LLMModel
	if withSummarize {
		res, code := runSummarize(prog, f, in, polls, lang, log, stderr)
		if code >= 0 {
			return code
		}
		if res.outcome != "" {
			return writeJSON(stdout, map[string]string{"outcome": res.outcome, "reason": res.reason})
		}
		sum, model = res.summary, res.model
	}

	doc, err := render.Render(render.Input{MeetingName: in.MeetingName, Transcript: in.Transcript, Summary: sum,
		Polls: polls, LLMModel: model, GeneratedAt: generated, Format: f, Language: lang, Zone: zone})
	if errors.Is(err, render.ErrStrings) {
		log.Error("no complete string table", "format_id", f.ID, "language", lang)
		return writeJSON(stdout, map[string]string{"outcome": string(queue.Permanent), "reason": summarize.ReasonConfigError})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: transcript: %v\n", prog, err)
		return exitUsage
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"summary.md", doc.SummaryMD}, {"transcript.vtt", doc.TranscriptVTT}} {
		if err := os.WriteFile(filepath.Join(outDir, file.name), file.data, 0o644); err != nil {
			fmt.Fprintf(stderr, "%s: cannot write %s\n", prog, file.name)
			return exitFailure
		}
	}
	log.Debug("rendered", "format_id", f.ID, "language", lang, "summary_bytes", len(doc.SummaryMD),
		"transcript_bytes", len(doc.TranscriptVTT))
	return writeJSON(stdout, map[string]string{"outcome": string(queue.OK), "format_id": f.ID,
		"format_version": f.Version, "format_sha256": f.SHA256, "language": lang, "title": doc.Title})
}

// summarized is the result of runSummarize: a summary (nil without speech)
// and the model, or a non-ok outcome with its reason.
type summarized struct {
	summary         *summarize.Summary
	model           string
	outcome, reason string
}

// runSummarize runs the summarize call plan against the default LLM
// profile. code is an exit code when the tool must stop, else -1.
func runSummarize(prog string, f *format.Format, in renderToolInput, polls []meeting.Poll, lang string,
	log *slog.Logger, stderr io.Writer) (summarized, int) {
	path := os.Getenv("RECAP_PROVIDERS_FILE")
	if path == "" {
		fmt.Fprintf(stderr, "%s: RECAP_PROVIDERS_FILE: required with --summarize\n", prog)
		return summarized{}, exitUsage
	}
	set, err := providers.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid providers file (RECAP_PROVIDERS_FILE):\n%v\n", prog, err)
		return summarized{}, exitUsage
	}
	set.LogWarnings(log)
	profile, err := set.Get(providers.KindLLM, "")
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return summarized{}, exitUsage
	}
	llm, err := newLLM(profile, log)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return summarized{}, exitUsage
	}
	sin := summarize.Input{MeetingName: in.MeetingName, Transcript: in.Transcript, Polls: polls}
	if in.Notes != nil {
		sin.Notes = *in.Notes
	}
	for _, c := range in.Chat {
		sin.Chat = append(sin.Chat, summarize.ChatMessage{AtMS: c.AtMS, Name: c.Name, Text: c.Text})
	}
	s := &summarize.Summarizer{LLM: llm, Profile: profile, Format: f, Log: log}
	res, err := s.Run(context.Background(), "tool", sin, lang)
	if err != nil {
		r := summarize.Outcome(err)
		reason := r.Reason
		if reason == "" {
			reason = r.ErrorCode
		}
		log.Info("summarize failed", "outcome", r.Outcome, "error_class", r.ErrorClass, "error_code", r.ErrorCode)
		return summarized{outcome: string(r.Outcome), reason: reason}, -1
	}
	if res.NoSpeech {
		return summarized{}, -1
	}
	var sum summarize.Summary
	if err := json.Unmarshal(res.Summary, &sum); err != nil {
		return summarized{}, exitFailure
	}
	return summarized{summary: &sum, model: res.Model}, -1
}

// countPolls turns the per-user responses into aggregate counts; the
// responses are not kept.
func countPolls(in renderToolInput) ([]meeting.Poll, error) {
	var polls []meeting.Poll
	for i, p := range in.Polls {
		poll := meeting.Poll{Question: p.Question}
		for _, o := range p.Options {
			poll.Options = append(poll.Options, meeting.PollOption{Text: o})
		}
		for _, r := range p.Responses {
			if r.Option < 0 || r.Option >= len(poll.Options) {
				return nil, fmt.Errorf("polls[%d]: response option out of range", i)
			}
			poll.Options[r.Option].Votes++
		}
		polls = append(polls, poll)
	}
	return polls, nil
}

// readMeetingTypes reads the meeting-types file (templates.md §1) as a map
// from meeting type to format ID.
func readMeetingTypes(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read the file")
	}
	var file struct {
		SchemaVersion string               `yaml:"schema_version"`
		MeetingTypes  []config.MeetingType `yaml:"meeting_types"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	types := map[string]string{}
	for _, mt := range file.MeetingTypes {
		types[mt.Name] = mt.Format
	}
	return types, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
