package slideimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// f14 is the raw recording of fixture F-14 (docs/reference/fixtures.md §3.3).
const f14 = "../../testdata/meetings/F-14/raw/e676a9dd4251878e23432dc7db4ad53b510dadc0-1761732000000/presentation"

func f14File(t *testing.T, deck, ext string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f14, deck, deck+"."+ext))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	chartDeck  = "2769ac3225e7f1a516f640472ef2839a6689325d-1761732002000"
	fourThree  = "3fd0a17706e870d5cce7a92731d04afaf29ce2e0-1761732003000"
	brokenDeck = "5f22c85787a24f8fbbbd52af5a483fb8b7e42400-1761732004000"
	uploadDeck = "e2ead0d62e6dd234021338bda943df6a16a47260-1761732003500"
)

func rgbAt(img image.Image, x, y int) [3]uint8 {
	r, g, b, _ := img.At(x, y).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

func near(a, b [3]uint8) bool {
	for i := range a {
		if d := int(a[i]) - int(b[i]); d > 40 || d < -40 {
			return false
		}
	}
	return true
}

func TestRenderPDF(t *testing.T) {
	r := New()
	defer r.Close()
	n, err := r.OpenPDF(f14File(t, chartDeck, "pdf"))
	if err != nil || n != 3 {
		t.Fatalf("OpenPDF = %d, %v; want 3 pages", n, err)
	}
	p1, err := r.RenderPage(1)
	if err != nil {
		t.Fatal(err)
	}
	if s := p1.Bounds().Size(); s.X != 1280 || s.Y != 720 {
		t.Errorf("page 1 is %v, want 1280x720", s)
	}
	// Page 2: a chart image with a soft mask, transparent elsewhere: white
	// where transparent (templates.md §3.2 item 3), blue bars.
	p2, err := r.RenderPage(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		x, y int
		want [3]uint8
	}{{280, 560, [3]uint8{0, 0, 255}}, {680, 440, [3]uint8{0, 0, 255}}, {1100, 100, [3]uint8{255, 255, 255}}} {
		if got := rgbAt(p2, c.x, c.y); !near(got, c.want) {
			t.Errorf("page 2 pixel (%d, %d) = %v, want about %v", c.x, c.y, got, c.want)
		}
	}
	p3, err := r.RenderPage(3)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := EncodePNG(p1)
	b, _ := EncodePNG(p3)
	if !bytes.Equal(a, b) {
		t.Errorf("pages 1 and 3 have the same content but differ")
	}
	if _, err := r.RenderPage(4); !errors.Is(err, ErrNoPage) {
		t.Errorf("page 4 of 3: %v, want ErrNoPage", err)
	}

	// 4:3 page, after another document.
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil {
		t.Fatal(err)
	}
	p, err := r.RenderPage(1)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Bounds().Size(); s.X != 1280 || s.Y != 960 {
		t.Errorf("4:3 page is %v, want 1280x960", s)
	}
	if _, err := r.OpenPDF(f14File(t, brokenDeck, "pdf")); !errors.Is(err, ErrOpen) {
		t.Errorf("broken PDF: %v, want ErrOpen", err)
	}
}

func TestRenderDeterministic(t *testing.T) {
	render := func() []byte {
		r := New()
		defer r.Close()
		if _, err := r.OpenPDF(f14File(t, chartDeck, "pdf")); err != nil {
			t.Fatal(err)
		}
		img, err := r.RenderPage(2)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodePNG(img)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := render(), render()
	if !bytes.Equal(a, b) {
		t.Error("two renders of the same page differ")
	}
	// PNG colour type 2, 8 bits, IHDR first and only IHDR, IDAT, IEND.
	if a[24] != 8 || a[25] != 2 {
		t.Errorf("bit depth %d colour type %d, want 8 and 2", a[24], a[25])
	}
	for i := 8; i < len(a); {
		n := int(a[i])<<24 | int(a[i+1])<<16 | int(a[i+2])<<8 | int(a[i+3])
		typ := string(a[i+4 : i+8])
		if typ != "IHDR" && typ != "IDAT" && typ != "IEND" {
			t.Errorf("chunk %s, want no ancillary chunks", typ)
		}
		i += 12 + n
	}
}

func TestDecodeUpload(t *testing.T) {
	r := New()
	defer r.Close()
	img, err := r.DecodeUpload(f14File(t, uploadDeck, "png"))
	if err != nil {
		t.Fatal(err)
	}
	if s := img.Bounds().Size(); s.X != 400 || s.Y != 200 {
		t.Errorf("upload is %v, want 400x200 (never scaled up)", s)
	}
	if got := rgbAt(img, 100, 100); got != [3]uint8{255, 255, 255} {
		t.Errorf("transparent half = %v, want white", got)
	}
	if got := rgbAt(img, 300, 100); !near(got, [3]uint8{0, 160, 0}) {
		t.Errorf("opaque half = %v, want about green", got)
	}
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeUploadLimits(t *testing.T) {
	r := New()
	defer r.Close()
	// Scaled down by the long-edge rule: 3000x1001 → 1280x427 (427.09).
	big := image.NewNRGBA(image.Rect(0, 0, 3000, 1001))
	for i := range big.Pix {
		big.Pix[i] = 0xFF
	}
	img, err := r.DecodeUpload(encode(t, big))
	if err != nil {
		t.Fatal(err)
	}
	if s := img.Bounds().Size(); s.X != 1280 || s.Y != 427 {
		t.Errorf("scaled upload is %v, want 1280x427", s)
	}
	// Over 25 megapixels by its header: the IHDR says 5001x5000. Only the
	// header is read, so a PNG whose data does not match is fine here.
	hdr := encode(t, image.NewGray(image.Rect(0, 0, 1, 1)))
	hdr[16], hdr[17], hdr[18], hdr[19] = 0, 0, 0x13, 0x89 // width 5001
	hdr[20], hdr[21], hdr[22], hdr[23] = 0, 0, 0x13, 0x88 // height 5000
	binary.BigEndian.PutUint32(hdr[29:33], crc32.ChecksumIEEE(hdr[12:29]))
	if _, err := r.DecodeUpload(hdr); !errors.Is(err, ErrTooLarge) {
		t.Errorf("5001x5000 upload: %v, want ErrTooLarge", err)
	}
	if _, err := r.DecodeUpload([]byte("not an image")); !errors.Is(err, ErrDecode) {
		t.Errorf("garbage: %v, want ErrDecode", err)
	}
}

func TestFitLongEdge(t *testing.T) {
	for _, c := range []struct {
		w, h       float64
		wantW, wan int
	}{
		{960, 540, 1280, 720},
		{720, 540, 1280, 960},
		{540, 960, 720, 1280},
		{1000, 1, 1280, 1},
		{100000, 1, 1280, 1}, // at least 1 px
		{3000, 1001, 1280, 427},
		{2560, 1, 1280, 1}, // 0.5 rounds half up
		{4, 3, 1280, 960},
	} {
		w, h, ok := FitLongEdge(c.w, c.h)
		if !ok || w != c.wantW || h != c.wan {
			t.Errorf("FitLongEdge(%v, %v) = %d, %d, %v; want %d, %d", c.w, c.h, w, h, ok, c.wantW, c.wan)
		}
	}
	if _, _, ok := FitLongEdge(0, 10); ok {
		t.Error("FitLongEdge(0, 10) is ok")
	}
}

// A render loop in the guest that runs over its time limit is stopped:
// ErrLimit at about the limit, the instance discarded, the next document
// gets a new one (templates.md §3.2 item 4).
func TestTimeLimit(t *testing.T) {
	r := New()
	defer r.Close()
	if _, err := r.OpenPDF(f14File(t, chartDeck, "pdf")); err != nil {
		t.Fatal(err)
	}
	inst, doc := r.inst, *r.doc
	start := time.Now()
	_, err := within(r, 50*time.Millisecond, func() (int, error) {
		for i := 0; ; i++ { // real page renders until the kill stops the guest
			if _, err := renderPage(inst, doc, i%3); err != nil {
				return i, err
			}
		}
	})
	took := time.Since(start)
	if !errors.Is(err, ErrLimit) || r.inst != nil || r.doc != nil {
		t.Fatalf("err = %v, instance %v; want ErrLimit and no instance", err, r.inst)
	}
	if took > killGrace {
		t.Errorf("the kill took %v, want the guest stopped at once", took)
	}
	if _, err := r.RenderPage(1); !errors.Is(err, ErrLimit) {
		t.Errorf("render after a limit hit: %v, want ErrLimit (no open document)", err)
	}
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil {
		t.Fatalf("next document: %v", err)
	}
	if _, err := r.RenderPage(1); err != nil {
		t.Errorf("render with a new instance: %v", err)
	}
}

// The pixel limit is checked from the header for JPEG and WebP as well
// (templates.md §3.2 item 2): only the header is read, so the data behind
// it does not matter.
func TestDecodeUploadLimitOtherFormats(t *testing.T) {
	r := New()
	defer r.Close()
	// JPEG: the SOF0 segment of a 1x1 JPEG says 5000x5001.
	var j bytes.Buffer
	if err := jpeg.Encode(&j, image.NewGray(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	jb := j.Bytes()
	sof := bytes.Index(jb, []byte{0xFF, 0xC0})
	if sof < 0 {
		t.Fatal("no SOF0 marker")
	}
	binary.BigEndian.PutUint16(jb[sof+5:], 5001) // height
	binary.BigEndian.PutUint16(jb[sof+7:], 5000) // width
	if _, err := r.DecodeUpload(jb); !errors.Is(err, ErrTooLarge) {
		t.Errorf("JPEG 5000x5001: %v, want ErrTooLarge", err)
	}
	// WebP: an extended header (VP8X) with a 5000x5001 canvas.
	vp8x := []byte("VP8X\x0a\x00\x00\x00\x00\x00\x00\x00")
	vp8x = append(vp8x, 0x87, 0x13, 0x00, 0x88, 0x13, 0x00) // width-1 = 4999, height-1 = 5000
	w := append([]byte("RIFF\x00\x00\x00\x00WEBP"), vp8x...)
	binary.LittleEndian.PutUint32(w[4:], uint32(len(w)-8))
	if _, err := r.DecodeUpload(w); !errors.Is(err, ErrTooLarge) {
		t.Errorf("WebP 5000x5001: %v, want ErrTooLarge", err)
	}
}
