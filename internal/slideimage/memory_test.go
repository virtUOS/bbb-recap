package slideimage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
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

// peakRSS returns the process's peak resident set size (VmHWM) in bytes,
// after resetPeak the peak since then.
func peakRSS(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return kb << 10
		}
	}
	t.Skip("no VmHWM")
	return 0
}

func resetPeak(t *testing.T) {
	t.Helper()
	runtime.GC()
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0); err != nil {
		t.Skipf("cannot reset the peak RSS: %v", err)
	}
}

// A page that drives PDFium to the memory cap (templates.md §3.2 item 4)
// gives ErrLimit, the next document gets a new instance, and the process's
// peak RSS stays bounded by the cap plus the renderer's own overhead.
func TestMemoryLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peak RSS is measured on Linux")
	}
	r := New()
	defer r.Close()
	if _, err := r.OpenPDF(f14File(t, fourThree, "pdf")); err != nil { // compile the module first
		t.Fatal(err)
	}
	resetPeak(t)
	base := peakRSS(t)
	if _, err := r.OpenPDF(nestedFormsPDF(5, 30)); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := r.RenderPage(1)
	peak := peakRSS(t)
	t.Logf("limit hit %v; peak RSS %d MiB, %d MiB above the RSS after compiling", errors.Is(err, ErrLimit), peak>>20,
		(peak-base)>>20)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("err is not ErrLimit")
	}
	if grow := peak - base; grow > MemoryLimit+memoryOverhead {
		t.Errorf("peak RSS grew by %d MiB, want at most %d MiB", grow>>20, (MemoryLimit+memoryOverhead)>>20)
	}
	if _, err := r.OpenPDF(f14File(t, chartDeck, "pdf")); err != nil {
		t.Fatalf("next document: %v", err)
	}
	if _, err := r.RenderPage(1); err != nil {
		t.Errorf("render with a new instance: %v", err)
	}
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
