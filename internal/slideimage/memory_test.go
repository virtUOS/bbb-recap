package slideimage

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

// nestedFormsPDF builds a synthetic PDF of a few KB whose page draws a form
// XObject nested levels deep, each level drawing the one below fan times:
// PDFium's memory grows with every level until a limit is hit.
func nestedFormsPDF(levels, fan int) []byte {
	stream := func(dict, body string) string {
		return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(body), body)
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 960 540] /Contents 4 0 R "+
			"/Resources << /XObject << /F%d %d 0 R >> >> >>", levels-1, 5+levels-1),
		stream("", fmt.Sprintf("/F%d Do", levels-1)),
		stream("/Type /XObject /Subtype /Form /BBox [0 0 960 540]",
			"0 0 1 rg 10 10 500 300 re f 1 0 0 rg 20 20 100 100 re f"),
	}
	for i := 1; i < levels; i++ {
		objs = append(objs, stream(fmt.Sprintf("/Type /XObject /Subtype /Form /BBox [0 0 960 540] "+
			"/Resources << /XObject << /F%d %d 0 R >> >>", i-1, 5+i-1),
			strings.Repeat(fmt.Sprintf("/F%d Do ", i-1), fan)))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

// memoryOverhead is what the process may hold at a limit hit beyond the
// module's linear memory: the page bitmap, Go heap and stacks (measured
// about 60 MiB; generous so the test is not flaky).
const memoryOverhead = 128 << 20

// brokenPagePDF builds a synthetic two-page PDF whose second page entry is
// not a page object: the document opens, page 1 renders, PDFium cannot load
// page 2.
func brokenPagePDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 960 540] /Contents 4 0 R >>",
		"<< /Length 24 >>\nstream\n0 0 1 rg 10 10 500 300 re f\nendstream",
		"42",
	}
	return rawPDF(objs)
}

// rawPDF writes objects 1..n with an xref table.
func rawPDF(objs []string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

// skipOrFail skips a measurement that cannot run here; with CI set it
// fails instead, so a CI run never passes without measuring.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// peakRSS returns the process's peak resident set size (VmHWM) in bytes,
// after resetPeak the peak since then.
func peakRSS(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		skipOrFail(t, "no /proc/self/status: %v", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return kb << 10
		}
	}
	skipOrFail(t, "no VmHWM in /proc/self/status")
	return 0
}

// resetPeak returns the free Go heap to the OS and resets VmHWM to the
// current RSS.
func resetPeak(t *testing.T) {
	t.Helper()
	debug.FreeOSMemory()
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0); err != nil {
		skipOrFail(t, "cannot reset the peak RSS: %v", err)
	}
}

// uploadThenCompile bounds the peak growth of an upload at the pixel limit
// followed by the first compile of the module: the larger of the two
// (about 570 MiB for the progressive CMYK JPEG), not their sum.
const uploadThenCompile = 600 << 20

// minGrowth is the least growth of the peak RSS that shows the page reached
// the memory cap, not the time limit (a slow runner).
const minGrowth = 400 << 20

// checkLimitHit renders the nested-form page on r and checks ErrLimit and
// the bound on the growth of the peak RSS since base.
func checkLimitHit(t *testing.T, r *Renderer, base int64, what string) {
	t.Helper()
	if _, err := r.OpenPDF(nestedFormsPDF(5, 30)); err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Now()
	_, err := r.RenderPage(1)
	took := time.Since(start)
	peak := peakRSS(t)
	grow := peak - base
	t.Logf("%s: limit hit %v; peak RSS %d MiB, %d MiB above the start", what, errors.Is(err, ErrLimit), peak>>20,
		grow>>20)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("err is not ErrLimit")
	}
	if grow > MemoryLimit+memoryOverhead {
		t.Errorf("peak RSS grew by %d MiB, want at most %d MiB", grow>>20, (MemoryLimit+memoryOverhead)>>20)
	}
	// The cap, not the time limit: the page stops well before PageTimeout
	// (about 2 s here). The growth alone does not show it when uploads
	// before the page already raised the peak.
	if took >= PageTimeout/2 || grow < minGrowth {
		skipOrFail(t, "the page took %v and the peak RSS grew by %d MiB only: it hit the time limit, not the memory cap",
			took, grow>>20)
	}
}

// A page that drives PDFium to the memory cap (templates.md §3.2 item 4)
// gives ErrLimit, the next document gets a new instance, and the process's
// peak RSS stays bounded by the cap plus the renderer's own overhead.
func TestMemoryLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bound holds on Linux only (alloc_linux.go)")
	}
	if raceEnabled {
		t.Skip("the race detector's shadow memory makes the peak RSS meaningless; the run without -race measures it")
	}
	r := New()
	defer r.Close()
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil { // compile the module first
		t.Fatal(err)
	}
	resetPeak(t)
	checkLimitHit(t, r, peakRSS(t), "nested forms")
	if _, err := r.OpenPDF(f14File(t, chartDeck, "pdf")); err != nil {
		t.Fatalf("next document: %v", err)
	}
	if _, err := r.RenderPage(1); err != nil {
		t.Errorf("render with a new instance: %v", err)
	}
}

// png16 is a 5000x5000 16-bit RGBA PNG (25 MP, the limit); seed makes
// distinct files.
func png16(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA64(image.Rect(0, 0, 5000, 5000))
	for i := 0; i < len(img.Pix); i += 8 {
		img.Pix[i+0], img.Pix[i+4], img.Pix[i+6], img.Pix[i+7] = byte(i>>8+seed), byte(i>>16), 0xFF, 0xFF
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Uploads decoded at the pixel limit, one after the other, then a page that
// hits the memory cap: each decode returns its heap before the next and
// before a new instance, so the same bound holds (templates.md §3.2 items 2
// and 4). The uploads are two distinct 16-bit PNGs and a progressive CMYK
// JPEG, the heaviest decode.
func TestMemoryLimitAfterUploads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bound holds on Linux only (alloc_linux.go)")
	}
	if raceEnabled {
		t.Skip("the race detector's shadow memory makes the peak RSS meaningless; the run without -race measures it")
	}
	uploads := []struct {
		name string
		data []byte
	}{{"16-bit PNG", png16(t, 0)}, {"another 16-bit PNG", png16(t, 7)},
		{"progressive CMYK JPEG", progressiveJPEG(5000, 5000)}}
	r := New()
	defer r.Close()
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil { // compile the module first
		t.Fatal(err)
	}
	resetPeak(t)
	base := peakRSS(t)
	for _, u := range uploads {
		if _, err := r.DecodeUpload(u.data); err != nil {
			t.Fatalf("%s: %v", u.name, err)
		}
		grow := peakRSS(t) - base
		t.Logf("after the %s: peak RSS %d MiB above the start", u.name, grow>>20)
		if grow > MemoryLimit+memoryOverhead {
			t.Errorf("%s: peak RSS grew by %d MiB, want at most %d MiB", u.name, grow>>20,
				(MemoryLimit+memoryOverhead)>>20)
		}
	}
	checkLimitHit(t, r, base, "uploads, then nested forms")
}

// An upload as the first slide of a run, before the renderer's module is
// compiled: the heap is returned before the compile too, so the compile
// does not come on top of the decode's garbage (templates.md §3.2 item 2).
func TestMemoryLimitUploadFirst(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bound holds on Linux only (alloc_linux.go)")
	}
	if raceEnabled {
		t.Skip("the race detector's shadow memory makes the peak RSS meaningless; the run without -race measures it")
	}
	upload := progressiveJPEG(5000, 5000)
	r := New()
	defer r.Close()
	resetPeak(t)
	base := peakRSS(t)
	if _, err := r.DecodeUpload(upload); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("after the upload: peak RSS %d MiB above the start", (peakRSS(t)-base)>>20)
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil { // compiles the module
		t.Fatal(err)
	}
	grow := peakRSS(t) - base
	t.Logf("after compiling: peak RSS %d MiB above the start", grow>>20)
	if grow > uploadThenCompile {
		t.Errorf("peak RSS grew by %d MiB after the upload and the compile, want at most %d MiB", grow>>20,
			uploadThenCompile>>20)
	}
	checkLimitHit(t, r, base, "upload first, then nested forms")
}

// An error PDFium reports for one page costs only that page: the instance
// and the document stay (templates.md §3.2 item 4).
func TestPageError(t *testing.T) {
	r := New()
	defer r.Close()
	n, err := r.OpenPDF(brokenPagePDF())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Logf("page count %d", n)
	if _, err := r.RenderPage(2); !errors.Is(err, ErrPageFailed) {
		t.Fatalf("page 2: %v, want ErrPageFailed", err)
	}
	inst := r.inst
	if _, err := r.RenderPage(1); err != nil || r.inst != inst {
		t.Errorf("page 1 after a page error: %v, same instance %v", err, r.inst == inst)
	}
}
