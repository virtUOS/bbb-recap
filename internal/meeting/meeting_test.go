package meeting_test

import (
	"errors"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/meeting"
)

const imageRef = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func TestDecode(t *testing.T) {
	m, err := meeting.Decode([]byte(`{"version":5,"meeting_name":"Fixture Widget Meeting","tracks":[],` +
		`"slides":[{"t_ms":9000,"deck":"d-1","page":1,"text":"Fixture Widget Agenda","image":"` + imageRef + `"},` +
		`{"t_ms":9500,"deck":"d-1","page":2,"text":"Fixture Widget Plan"}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.Version != meeting.Version || m.MeetingName != "Fixture Widget Meeting" {
		t.Errorf("got %+v", m)
	}
	want := []meeting.Slide{
		{TMS: 9000, Deck: "d-1", Page: 1, Text: "Fixture Widget Agenda", Image: imageRef},
		{TMS: 9500, Deck: "d-1", Page: 2, Text: "Fixture Widget Plan"},
	}
	if len(m.Slides) != 2 || m.Slides[0] != want[0] || m.Slides[1] != want[1] {
		t.Errorf("slides = %+v, want %+v", m.Slides, want)
	}
}

// A version 4 blob has slides without images (templates.md §3.2).
func TestDecodeVersion4(t *testing.T) {
	m, err := meeting.Decode([]byte(`{"version":4,"meeting_name":"Fixture Widget Meeting","tracks":[],` +
		`"slides":[{"t_ms":9000,"deck":"d-1","page":1,"text":"Fixture Widget Agenda","image":"` + imageRef + `"}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := meeting.Slide{TMS: 9000, Deck: "d-1", Page: 1, Text: "Fixture Widget Agenda"}
	if len(m.Slides) != 1 || m.Slides[0] != want {
		t.Errorf("slides = %+v, want [%+v]", m.Slides, want)
	}
}

// A version 3 blob has no slides (templates.md §3.1): read as slides [].
func TestDecodeVersion3(t *testing.T) {
	m, err := meeting.Decode([]byte(`{"version":3,"meeting_name":"Fixture Widget Meeting","tracks":[]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.MeetingName != "Fixture Widget Meeting" || m.Slides == nil || len(m.Slides) != 0 {
		t.Errorf("got %+v, want the meeting with slides []", m)
	}
}

func TestDecodeRejectsOtherVersions(t *testing.T) {
	for _, blob := range []string{
		`{"version":2,"meeting_name":"Fixture Widget Meeting"}`,
		`{"version":6,"meeting_name":"Fixture Widget Meeting"}`,
		`{"meeting_name":"Fixture Widget Meeting"}`,
	} {
		_, err := meeting.Decode([]byte(blob))
		if !errors.Is(err, meeting.ErrVersion) {
			t.Errorf("%s: err = %v, want ErrVersion", blob, err)
		}
	}
}

func TestDecodeRejectsInvalidJSON(t *testing.T) {
	_, err := meeting.Decode([]byte(`{"version":`))
	if err == nil || errors.Is(err, meeting.ErrVersion) {
		t.Errorf("err = %v, want a JSON error", err)
	}
}
