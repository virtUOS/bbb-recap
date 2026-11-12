package render

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Escape makes one string from meeting content or the LLM literal Markdown
// text (templates.md §5.2): newlines (CRLF, CR, LF) become spaces; a
// backslash goes before \ ` * _ ~ [ ] < > # |; a leading -, + or = gets a
// backslash, and a leading <digits>. becomes <digits>\. ('>' and '#' are
// escaped everywhere anyway).
func Escape(s string) string {
	s = newlines.Replace(s)
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune("\\`*_~[]<>#|", c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if out != "" && strings.ContainsRune("-+=", rune(out[0])) {
		return `\` + out
	}
	if m := orderedItem.FindStringIndex(out); m != nil {
		dot := m[1] - 1
		return out[:dot] + `\` + out[dot:]
	}
	return out
}

var (
	newlines    = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")
	orderedItem = regexp.MustCompile(`^[0-9]+\.`)
)

// summaryMD renders summary.md (templates.md §5.4): blocks separated by one
// blank line, the file ends with one LF.
func (r *renderer) summaryMD() string {
	s, tr := r.s, r.in.Transcript
	periods := tr.RecordingPeriods
	zone := r.in.Zone
	at := func(ms int64) string {
		return r.origin.Add(time.Duration(ms) * time.Millisecond).In(zone).Format("15:04")
	}
	first, last, duration := at(0), at(0), int64(0)
	if len(periods) > 0 {
		first, last = at(periods[0].StartMS), at(periods[len(periods)-1].EndMS)
	}
	for _, p := range periods {
		duration += p.EndMS - p.StartMS
	}

	var blocks []string
	add := func(b ...string) { blocks = append(blocks, b...) }
	// 1 header, 2 notice
	add(fill(s["header"], map[string]string{"meeting_name": Escape(r.in.MeetingName),
		"date": r.origin.In(zone).Format(time.DateOnly), "first_start": first, "last_end": last, "tz": zone.String(),
		"recorded_duration": hms(duration)}))
	add(s["notice"])
	// 3 gaps
	for _, g := range tr.Gaps {
		add(fill(s["gap"], map[string]string{"speaker": Escape(r.speakerName(g.SpeakerRef)),
			"start": hms(recorded(g.StartMS, periods)), "end": hms(recorded(g.EndMS, periods))}))
	}
	// 4–8 the LLM sections, or the no-speech notice
	if sum := r.in.Summary; sum == nil {
		add(s["no_speech"])
	} else {
		add("## "+s["overview"], Escape(sum.Overview))

		add("## " + s["topics"])
		if len(sum.Topics) == 0 {
			add(s["none"])
		}
		for _, t := range sum.Topics {
			add("### "+Escape(t.Title)+timeRef(t.Start), Escape(t.Summary))
		}

		var items []string
		for _, d := range sum.Decisions {
			items = append(items, "- "+Escape(d.Text)+timeRef(d.At))
		}
		add("## "+s["decisions"], list(items, s["none"]))

		items = nil
		for _, a := range sum.ActionItems {
			line := "- [ ] " + Escape(a.Task)
			if a.Owner != "" {
				line += " — " + Escape(a.Owner)
			}
			if a.Due != "" {
				line += " — " + s["due"] + ": " + Escape(a.Due)
			}
			items = append(items, line+timeRef(a.At))
		}
		add("## "+s["action_items"], list(items, s["none"]))

		items = nil
		for _, q := range sum.OpenQuestions {
			items = append(items, "- "+Escape(q.Text))
		}
		add("## "+s["open_questions"], list(items, s["none"]))
	}
	// 9 polls, aggregate counts only
	if len(r.in.Polls) > 0 {
		add("## " + s["polls"])
		for _, p := range r.in.Polls {
			rows := []string{"| " + s["option"] + " | " + s["votes"] + " |", "| --- | --- |"}
			for _, o := range p.Options {
				rows = append(rows, "| "+Escape(o.Text)+" | "+strconv.Itoa(o.Votes)+" |")
			}
			add("**"+Escape(p.Question)+"**", strings.Join(rows, "\n"))
		}
	}
	// 10 footer
	var models []string
	for _, t := range tr.Tracks {
		if t.Provider != nil && t.Provider.Model != "" && !slices.Contains(models, t.Provider.Model) {
			models = append(models, t.Provider.Model)
		}
	}
	llm := Escape(r.in.LLMModel)
	if llm == "" {
		llm = s["no_llm"]
	}
	add("---", fill(s["footer"], map[string]string{"format_id": r.in.Format.ID,
		"format_version": r.in.Format.Version, "asr_model": Escape(strings.Join(models, ", ")), "llm_model": llm,
		"generated_at": r.in.GeneratedAt.UTC().Format(time.RFC3339)}))
	return strings.Join(blocks, "\n\n") + "\n"
}

// timeRef is " [hh:mm:ss]", or "" without a time.
func timeRef(t string) string {
	if t == "" {
		return ""
	}
	return " [" + t + "]"
}

// list joins list items, or returns none for an empty list.
func list(items []string, none string) string {
	if len(items) == 0 {
		return none
	}
	return strings.Join(items, "\n")
}
