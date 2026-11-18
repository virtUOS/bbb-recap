package normalize

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// f14Dir is the raw recording of fixture F-14 (fixtures.md §3.3).
var f14Dir = filepath.Join("..", "..", "..", "testdata", "meetings", "F-14", "raw",
	"e676a9dd4251878e23432dc7db4ad53b510dadc0-1761732000000")

// templates.md §3.2 "Dedupe and cap": F-14 makes 50 render attempts and 48
// distinct images; the broken PDF and the pages after the cap fall back.
// Without images nothing is rendered and no slide has an image.
func TestImagesF14(t *testing.T) {
	sel, err := manifest.Select(f14Dir)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]int{}
	fallbacks := map[string]int{}
	img := &Images{
		Store:    func(key string, png []byte) error { stored[key] = len(png); return nil },
		Fallback: func(reason string) { fallbacks[reason]++ },
	}
	m, err := Normalize(os.DirFS(f14Dir), sel.Files, img)
	if err != nil {
		t.Fatal(err)
	}
	if img.Attempts != 50 || img.Stored != 48 || len(stored) != 48 {
		t.Errorf("attempts %d, images %d (%d stored), want 50 and 48", img.Attempts, img.Stored, len(stored))
	}
	if fallbacks[FallbackRenderFailed] != 1 || fallbacks[FallbackCap] != 8 || len(fallbacks) != 2 {
		t.Errorf("fallbacks %v, want render_failed 1 and cap 8", fallbacks)
	}
	withImage := 0
	for _, s := range m.Slides {
		if s.Image != "" {
			withImage++
		}
	}
	if len(m.Slides) != 59 || withImage != 50 {
		t.Errorf("%d slides, %d with an image; want 59 and 50", len(m.Slides), withImage)
	}

	off, err := Normalize(os.DirFS(f14Dir), sel.Files, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range off.Slides {
		if s.Image != "" || s.Text == "" {
			t.Errorf("images off: slide %+v", s)
		}
	}
}

// countFS counts the opens of each file.
type countFS struct {
	fs.FS
	opens map[string]int
}

func (c countFS) Open(name string) (fs.File, error) {
	c.opens[name]++
	return c.FS.Open(name)
}

// A page beyond the page count is no attempt and does not open the file
// again once its page count is known, also when pages of two decks
// alternate (templates.md §3.2 "Dedupe and cap").
func TestImagesPageBeyondCount(t *testing.T) {
	sel, err := manifest.Select(f14Dir)
	if err != nil {
		t.Fatal(err)
	}
	const chart = "2769ac3225e7f1a516f640472ef2839a6689325d-1761732002000" // 3 pages
	const four = "3fd0a17706e870d5cce7a92731d04afaf29ce2e0-1761732003000"  // 2 pages
	cfs := countFS{FS: os.DirFS(f14Dir), opens: map[string]int{}}
	img := &Images{Store: func(string, []byte) error { return nil }}
	im := newImager(cfs, sel.Files, img)
	defer im.close()
	for _, p := range []struct {
		deck string
		page int
	}{{chart, 1}, {four, 1}, {chart, 7}, {four, 9}, {chart, 8}, {four, 10}, {chart, 9}} {
		if _, err := im.image(p.deck, p.page); err != nil {
			t.Fatal(err)
		}
	}
	if img.Attempts != 2 {
		t.Errorf("%d render attempts, want 2 (pages beyond the count are none)", img.Attempts)
	}
	for _, d := range []string{chart, four} {
		if n := cfs.opens["presentation/"+d+"/"+d+".pdf"]; n > 3 {
			t.Errorf("deck %s opened %d times, want each open only while it is needed", d[:8], n)
		}
	}
	// Once both counts are known, no page beyond them opens a file.
	opens := cfs.opens["presentation/"+chart+"/"+chart+".pdf"] + cfs.opens["presentation/"+four+"/"+four+".pdf"]
	for i := 20; i < 30; i++ {
		_, _ = im.image(chart, i)
		_, _ = im.image(four, i)
	}
	if after := cfs.opens["presentation/"+chart+"/"+chart+".pdf"] + cfs.opens["presentation/"+four+"/"+four+".pdf"]; after != opens {
		t.Errorf("%d opens for pages beyond the known counts, want none", after-opens)
	}
}
