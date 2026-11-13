package meeting_test

import (
	"errors"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/meeting"
)

func TestDecode(t *testing.T) {
	m, err := meeting.Decode([]byte(`{"version":3,"meeting_name":"Fixture Widget Meeting","tracks":[]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.Version != meeting.Version || m.MeetingName != "Fixture Widget Meeting" {
		t.Errorf("got %+v", m)
	}
}

func TestDecodeRejectsOtherVersions(t *testing.T) {
	for _, blob := range []string{
		`{"version":2,"meeting_name":"Fixture Widget Meeting"}`,
		`{"version":4,"meeting_name":"Fixture Widget Meeting"}`,
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
