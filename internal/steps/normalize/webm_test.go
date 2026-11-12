package normalize

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

const f01Raw = "../../../testdata/meetings/F-01/raw/536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"

func TestWebMDuration(t *testing.T) {
	b, err := os.ReadFile(f01Raw + "/audio/microphone-w_fixada0001-TR_fixA1-1761732005200.webm")
	if err != nil {
		t.Fatal(err)
	}
	ms, ok, err := webmDuration(bytes.NewReader(b))
	// The file covers 5 200 … 60 000 ms (fixtures.md §3.1); the encoder adds a little.
	if err != nil || !ok || ms < 54000 || ms > 56000 {
		t.Fatalf("duration %d ok=%v err=%v", ms, ok, err)
	}

	for name, data := range map[string][]byte{
		"empty":       nil,
		"text":        []byte("SYNTHETIC fixture: not a WebM file"),
		"truncated":   b[:20],
		"no segment":  b[:0x24],
		"zero header": {0x00, 0x00, 0x00},
	} {
		if _, _, err := webmDuration(bytes.NewReader(data)); !errors.Is(err, errNotWebM) {
			t.Errorf("%s: err = %v, want errNotWebM", name, err)
		}
	}
	// A top-level element larger than any package file is rejected, not
	// skipped at a wrong offset.
	huge := append(append([]byte{}, b[:0x24]...),
		0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // Segment, unknown size
		0xEC, 0x01, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF) // Void of 4 GiB
	if _, _, err := webmDuration(bytes.NewReader(huge)); !errors.Is(err, errNotWebM) {
		t.Errorf("huge element: err = %v", err)
	}
	// Matroska (DocType "matroska") is not WebM.
	mkv := bytes.Replace(b[:64], []byte("webm"), []byte("mkvx"), 1)
	if _, _, err := webmDuration(bytes.NewReader(mkv)); !errors.Is(err, errNotWebM) {
		t.Errorf("other doctype: err = %v", err)
	}
}

func TestReadVint(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		keep bool
		v    int64
		n    int
	}{
		{[]byte{0x81}, false, 1, 1},
		{[]byte{0x40, 0x02}, false, 2, 2},
		{[]byte{0xFF}, false, unknownSize, 1},
		{[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, false, unknownSize, 8},
		{[]byte{0x1A, 0x45, 0xDF, 0xA3}, true, idEBML, 4},
		{[]byte{0x00}, false, 0, 0},
		{[]byte{0x40}, false, 0, 0},
	} {
		v, n := readVint(c.in, c.keep)
		if v != c.v || n != c.n {
			t.Errorf("readVint(%x, %v) = %d, %d; want %d, %d", c.in, c.keep, v, n, c.v, c.n)
		}
	}
}
