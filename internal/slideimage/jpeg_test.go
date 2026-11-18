package slideimage

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"
)

// progressiveJPEG builds a synthetic progressive (SOF2) four-component
// JPEG of w×h pixels with one DC scan: every block is flat. It is the
// heaviest upload to decode: Go's decoder keeps the coefficients of every
// block of a progressive image (256 bytes per 8×8 block and component)
// next to the decoded image. Go has no progressive encoder, so the bytes
// are written here by hand: one quantization table, one DC Huffman table
// with a single code ("0" = difference 0), one interleaved DC scan.
func progressiveJPEG(w, h int) []byte {
	var b bytes.Buffer
	seg := func(marker byte, payload []byte) {
		b.Write([]byte{0xFF, marker})
		_ = binary.Write(&b, binary.BigEndian, uint16(len(payload)+2))
		b.Write(payload)
	}
	b.Write([]byte{0xFF, 0xD8})                               // SOI
	seg(0xEE, append([]byte("Adobe"), 0, 100, 0, 0, 0, 0, 0)) // APP14, transform 0: CMYK
	dqt := []byte{0x00}
	for range 64 {
		dqt = append(dqt, 1)
	}
	seg(0xDB, dqt)
	sof := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 4}
	for c := byte(1); c <= 4; c++ {
		sof = append(sof, c, 0x11, 0)
	}
	seg(0xC2, sof) // SOF2: progressive
	dht := []byte{0x00, 1}
	dht = append(dht, make([]byte, 15)...)
	dht = append(dht, 0) // one DC code of length 1, category 0
	seg(0xC4, dht)
	sos := []byte{4}
	for c := byte(1); c <= 4; c++ {
		sos = append(sos, c, 0x00)
	}
	sos = append(sos, 0, 0, 0) // Ss 0, Se 0, Ah/Al 0: the DC scan
	seg(0xDA, sos)
	blocks := ((w + 7) / 8) * ((h + 7) / 8) * 4 // one bit "0" per block
	data := make([]byte, (blocks+7)/8)
	if r := blocks % 8; r != 0 {
		data[len(data)-1] = byte(0xFF >> r) // pad the last byte with 1 bits
	}
	b.Write(data)
	b.Write([]byte{0xFF, 0xD9}) // EOI
	return b.Bytes()
}

// The generator gives a progressive CMYK JPEG that Go decodes.
func TestProgressiveJPEG(t *testing.T) {
	img, err := jpeg.Decode(bytes.NewReader(progressiveJPEG(64, 48)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.CMYK); !ok || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Errorf("decoded %T %v, want a 64x48 CMYK image", img, img.Bounds())
	}
}
