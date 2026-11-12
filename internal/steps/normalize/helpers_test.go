package normalize

import "github.com/virtUOS/bbb-recap/internal/events"

func eventsWith(ts []int64, start []bool, last int64) *events.Events {
	ev := &events.Events{LastTS: last, HasEvents: true}
	for i := range ts {
		ev.Marks = append(ev.Marks, events.Mark{TS: ts[i], Start: start[i]})
	}
	return ev
}

func eventsMark(utc int64) events.Mark { return events.Mark{UTC: utc} }
