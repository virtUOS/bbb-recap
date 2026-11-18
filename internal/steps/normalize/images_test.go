package normalize

import (
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
