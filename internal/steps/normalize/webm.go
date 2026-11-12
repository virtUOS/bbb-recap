package normalize

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// errNotWebM means the bytes are not a WebM file this reader understands.
var errNotWebM = errors.New("not a WebM file")

// EBML element IDs (Matroska / WebM, RFC 8794 and RFC 9559), with their
// length marker bits, as they appear in the file.
const (
	idEBML          = 0x1A45DFA3
	idDocType       = 0x4282
	idSegment       = 0x18538067
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idCluster       = 0x1F43B675
)

// Bounds for the elements this reader loads into memory.
const (
	maxHeaderSize = 4 << 10
	maxInfoSize   = 64 << 10
)

// unknownSize marks an EBML element of unknown size (all value bits set).
const unknownSize = -1

// webmDuration reads the EBML header and the Segment's Info element of a
// WebM file and returns its duration in milliseconds. ok is false when the
// file is WebM but has no Duration before its first Cluster. A file that is
// not WebM, or is cut short in the header, gives errNotWebM; other errors are
// errors of r. It does not decode any audio.
func webmDuration(r io.Reader) (ms int64, ok bool, err error) {
	br := bufio.NewReader(r)
	id, size, err := readElementHead(br)
	if err != nil || id != idEBML || size == unknownSize || size > maxHeaderSize {
		return 0, false, notWebM(err)
	}
	header := make([]byte, size)
	if _, err := io.ReadFull(br, header); err != nil {
		return 0, false, notWebM(err)
	}
	docType, found := childBytes(header, idDocType)
	if !found || string(docType) != "webm" {
		return 0, false, errNotWebM
	}

	id, _, err = readElementHead(br)
	if err != nil || id != idSegment {
		return 0, false, notWebM(err)
	}
	for {
		id, size, err := readElementHead(br)
		if err == io.EOF {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, notWebM(err)
		}
		if id == idCluster || size == unknownSize {
			return 0, false, nil // media data starts: no Info before it
		}
		if id != idInfo {
			if size > math.MaxInt32 {
				return 0, false, errNotWebM // larger than a package file may be
			}
			if _, err := br.Discard(int(size)); err != nil {
				return 0, false, notWebM(err)
			}
			continue
		}
		if size > maxInfoSize {
			return 0, false, errNotWebM
		}
		info := make([]byte, size)
		if _, err := io.ReadFull(br, info); err != nil {
			return 0, false, notWebM(err)
		}
		return infoDuration(info)
	}
}

// notWebM keeps errors of the underlying reader and turns format problems
// (including a file that ends too early) into errNotWebM.
func notWebM(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errNotWebM) {
		return errNotWebM
	}
	return err
}

// infoDuration reads TimecodeScale (default 1 000 000 ns) and Duration (a
// float in TimecodeScale units) from the body of an Info element.
func infoDuration(info []byte) (int64, bool, error) {
	scale := uint64(1_000_000)
	if b, found := childBytes(info, idTimecodeScale); found {
		if len(b) == 0 || len(b) > 8 {
			return 0, false, errNotWebM
		}
		scale = 0
		for _, c := range b {
			scale = scale<<8 | uint64(c)
		}
	}
	b, found := childBytes(info, idDuration)
	if !found {
		return 0, false, nil
	}
	var d float64
	switch len(b) {
	case 4:
		d = float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		d = math.Float64frombits(binary.BigEndian.Uint64(b))
	default:
		return 0, false, errNotWebM
	}
	ms := d * float64(scale) / 1e6
	if math.IsNaN(ms) || ms < 0 || ms > math.MaxInt32 {
		return 0, false, errNotWebM
	}
	return int64(math.Round(ms)), true, nil
}

// childBytes finds the first child element id in an element body and
// returns its data.
func childBytes(body []byte, want uint64) ([]byte, bool) {
	for len(body) > 0 {
		id, n := readVint(body, true)
		if n == 0 {
			return nil, false
		}
		body = body[n:]
		size, m := readVint(body, false)
		if m == 0 || size < 0 || size > int64(len(body)-m) {
			return nil, false
		}
		body = body[m:]
		if uint64(id) == want {
			return body[:size], true
		}
		body = body[size:]
	}
	return nil, false
}

// readVint decodes an EBML variable-length integer at the start of b and
// returns it and its length (0 if invalid). keepMarker keeps the length
// marker bit, as element IDs are written with it. A size with all value bits
// set is returned as unknownSize.
func readVint(b []byte, keepMarker bool) (int64, int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	n := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if len(b) < n || (keepMarker && n > 4) {
		return 0, 0
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= uint64(0xFF >> n)
	}
	allOnes := v == uint64(0xFF>>n)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
		allOnes = allOnes && c == 0xFF
	}
	if !keepMarker && allOnes {
		return unknownSize, n
	}
	return int64(v), n
}

// readElementHead reads an element ID and size from r.
func readElementHead(r *bufio.Reader) (id uint64, size int64, err error) {
	v, err := readVintFrom(r, true)
	if err != nil {
		return 0, 0, err
	}
	s, err := readVintFrom(r, false)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return uint64(v), s, err
}

func readVintFrom(r *bufio.Reader, keepMarker bool) (int64, error) {
	first, err := r.Peek(1)
	if err != nil {
		return 0, err
	}
	if first[0] == 0 {
		return 0, errNotWebM
	}
	n := 1
	for mask := byte(0x80); first[0]&mask == 0; mask >>= 1 {
		n++
	}
	b, err := r.Peek(n)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	v, m := readVint(b, keepMarker)
	if m == 0 {
		return 0, errNotWebM
	}
	_, _ = r.Discard(m)
	return v, nil
}
