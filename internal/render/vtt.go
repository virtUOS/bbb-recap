package render

import (
	"strings"
)

// transcriptVTT renders transcript.vtt (templates.md §6): one cue per
// segment in segment order, a pause NOTE before the first cue of each
// recording period after the first, and a gap NOTE before the first cue
// whose start is at or after the gap's recorded start (after a pause NOTE
// for the same cue), or at the end.
func (r *renderer) transcriptVTT() string {
	s, tr := r.s, r.in.Transcript
	periods := tr.RecordingPeriods

	type note struct {
		start int64
		text  string
	}
	var pending []note
	for _, g := range tr.Gaps {
		start, end := recorded(g.StartMS, periods), recorded(g.EndMS, periods)
		pending = append(pending, note{start, fill(s["vtt_gap"], map[string]string{"start": vttTime(start),
			"end": vttTime(end), "name": noteName(r.speakerName(g.SpeakerRef)), "reason": g.Reason})})
	}

	blocks := []string{"WEBVTT", s["vtt_header"]}
	prev := -1
	for _, seg := range tr.Segments {
		// The period containing the segment midpoint; a midpoint in a pause
		// takes the next period, after the last period the last one.
		mid2 := seg.StartMS + seg.EndMS // twice the midpoint
		idx := len(periods) - 1
		for i, p := range periods {
			if mid2 <= 2*p.EndMS {
				idx = i
				break
			}
		}
		start, end := seg.StartMS, seg.EndMS
		if idx >= 0 {
			p := periods[idx]
			clamp := func(t int64) int64 { return min(max(t, p.StartMS), p.EndMS) - p.StartMS + p.RecordedOffsetMS }
			start, end = clamp(start), clamp(end)
		}
		if end <= start {
			end = start + 1
		}
		if idx > 0 && idx != prev {
			blocks = append(blocks, s["vtt_pause"])
		}
		prev = idx
		var rest []note
		for _, n := range pending {
			if n.start <= start {
				blocks = append(blocks, n.text)
			} else {
				rest = append(rest, n)
			}
		}
		pending = rest
		blocks = append(blocks, seg.ID+"\n"+vttTime(start)+" --> "+vttTime(end)+"\n"+
			"<v "+cueText(r.speakerName(seg.SpeakerRef))+">"+cueText(seg.Text)+"</v>")
	}
	for _, n := range pending {
		blocks = append(blocks, n.text)
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

var (
	cueEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	whitespace = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")
)

// cueText escapes a voice name or cue text (§6): & < > as character
// references, CR, LF and tab as spaces, runs of spaces collapsed, trimmed.
func cueText(s string) string {
	return clean(cueEscaper.Replace(s))
}

// noteName cleans a name for a gap NOTE (§6): whitespace as in cueText, & < >
// kept (comments have no character references), then "-->" replaced by "->"
// until none is left.
func noteName(s string) string {
	s = clean(s)
	for strings.Contains(s, "-->") {
		s = strings.ReplaceAll(s, "-->", "->")
	}
	return s
}

func clean(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(whitespace.Replace(s), " "))
}
