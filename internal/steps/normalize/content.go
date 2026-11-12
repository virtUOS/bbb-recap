package normalize

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/events"
	"github.com/virtUOS/bbb-recap/internal/htmltext"
	"github.com/virtUOS/bbb-recap/internal/meeting"
)

// message is a chat message while the chat events are replayed.
type message struct {
	ts      int64
	sender  string // BBB userId
	id      string
	replyTo string
	html    string
	removed bool
}

// chat replays the public chat events in document order, as BBB's playback
// does (generators/events.rb:969-1023 @ 7d27e7e): an edit replaces the
// message, a deletion drops the message and the replies to it, a clear
// drops every message sent before it. Only messages sent inside a recording
// period are kept; edits, deletions and clears count wherever they happen.
// Messages whose text is empty after HTML → text are dropped.
func chat(ev *events.Events, origin int64, ps []meeting.Period, people *participants) []meeting.Chat {
	var msgs []*message
	for _, c := range ev.Chat {
		switch c.Kind {
		case events.EventPublicChat:
			msgs = append(msgs, &message{ts: c.TS, sender: c.SenderID, id: c.MessageID, replyTo: c.ReplyToID,
				html: c.Message})
		case events.EventChatEdit:
			if m := find(msgs, c.MessageID); m != nil {
				m.html = c.Message
			}
		case events.EventChatDelete:
			for _, m := range msgs {
				if c.MessageID != "" && (m.id == c.MessageID || m.replyTo == c.MessageID) {
					m.removed = true
				}
			}
		case events.EventChatClear:
			for _, m := range msgs {
				m.removed = true
			}
		}
	}
	out := []meeting.Chat{}
	for _, m := range msgs {
		t := m.ts - origin
		if m.removed || !inPeriods(ps, t) {
			continue
		}
		text := htmltext.Text(m.html)
		if text == "" {
			continue
		}
		out = append(out, meeting.Chat{TMS: t, Participant: people.ref(events.UserKey(m.sender)), Text: text})
	}
	return out
}

// find returns the first live message with id.
func find(msgs []*message, id string) *message {
	for _, m := range msgs {
		if id != "" && m.id == id && !m.removed {
			return m
		}
	}
	return nil
}

// pollState collects the events of one poll.
type pollState struct {
	question  string
	options   map[int]string
	answers   map[string][]int // userId → answer ids of the user's last response
	lastTS    map[string]int64 // userId → timestamp of that response
	secret    []int            // answers without a userId (secret polls)
	published bool
	tMS       int64
}

// polls returns the published polls whose publish event lies inside a
// recording period, by publish time (templates.md §3). Options come from
// the answers of the start or publish event, else from the responses, in
// answer id order. Votes are counted from the responses: a user's last
// response counts once; its events share one timestamp (a multi-response
// answer is one event per chosen option, RespondToPollReqMsgHdlr.scala:37-43
// @ 7d27e7e). Responses without a userId (secret polls,
// RedisRecorderActor.scala:758-762 @ 7d27e7e) count one each. User IDs are
// used only while counting.
func polls(ev *events.Events, origin int64, ps []meeting.Period) []meeting.Poll {
	byID := map[string]*pollState{}
	var order []string
	get := func(id string) *pollState {
		p, ok := byID[id]
		if !ok {
			p = &pollState{options: map[int]string{}, answers: map[string][]int{}, lastTS: map[string]int64{}}
			byID[id] = p
			order = append(order, id)
		}
		return p
	}
	for _, e := range ev.Polls {
		if e.PollID == "" {
			continue
		}
		p := get(e.PollID)
		switch e.Kind {
		case events.EventPollStarted:
			p.question = strings.TrimSpace(e.Question)
			addOptions(p.options, e.Answers)
		case events.EventPollResponded:
			id, err := strconv.Atoi(e.AnswerID)
			if err != nil {
				continue
			}
			if _, ok := p.options[id]; !ok && strings.TrimSpace(e.Answer) != "" {
				p.options[id] = strings.TrimSpace(e.Answer)
			}
			switch {
			case e.UserID == "":
				p.secret = append(p.secret, id)
			case !seenTS(p.lastTS, e.UserID, e.TS):
				p.answers[e.UserID] = []int{id}
				p.lastTS[e.UserID] = e.TS
			case !slices.Contains(p.answers[e.UserID], id):
				p.answers[e.UserID] = append(p.answers[e.UserID], id)
			}
		case events.EventPollPublished:
			if q := strings.TrimSpace(e.Question); q != "" {
				p.question = q
			}
			addOptions(p.options, e.Answers)
			if t := e.TS - origin; !p.published && inPeriods(ps, t) {
				p.published, p.tMS = true, t
			}
		}
	}

	out := []meeting.Poll{}
	for _, id := range order {
		p := byID[id]
		if !p.published {
			continue
		}
		votes := map[int]int{}
		for _, ids := range p.answers {
			for _, a := range ids {
				votes[a]++
			}
		}
		for _, a := range p.secret {
			votes[a]++
		}
		ids := make([]int, 0, len(p.options))
		for a := range p.options {
			ids = append(ids, a)
		}
		sort.Ints(ids)
		poll := meeting.Poll{TMS: p.tMS, Question: p.question, Options: []meeting.PollOption{}}
		for _, a := range ids {
			poll.Options = append(poll.Options, meeting.PollOption{Text: p.options[a], Votes: votes[a]})
		}
		out = append(out, poll)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TMS < out[j].TMS })
	return out
}

// seenTS reports whether user's last response has timestamp ts.
func seenTS(last map[string]int64, user string, ts int64) bool {
	t, ok := last[user]
	return ok && t == ts
}

// addOptions adds the options of an answers field (JSON array of {id, key})
// that are not known yet. A field that does not parse adds nothing.
func addOptions(options map[int]string, answers string) {
	var list []struct {
		ID  int    `json:"id"`
		Key string `json:"key"`
	}
	if answers == "" || json.Unmarshal([]byte(answers), &list) != nil {
		return
	}
	for _, a := range list {
		if _, ok := options[a.ID]; !ok && strings.TrimSpace(a.Key) != "" {
			options[a.ID] = strings.TrimSpace(a.Key)
		}
	}
}
