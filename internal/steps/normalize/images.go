package normalize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"io"
	"io/fs"

	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/slideimage"
)

// Constants of templates.md §3.2 (provisional until the staging
// measurement, ADR-017 D11).
const (
	// maxRenderAttempts is the most render attempts per job: one page
	// render of a PDF, one decode of an image upload, or one source file
	// that does not open.
	maxRenderAttempts = 50
	// maxSourceBytes is BBB's upload limit; a larger source is not read
	// (upstream-facts.md "BBB presentations").
	maxSourceBytes = 30_000_000
)

// uploadExts are the image uploads that give an image (page 1 only).
var uploadExts = []string{"png", "jpg", "jpeg", "webp"}

// Fallback reasons of a shown page without an image (architecture.md §12,
// recap_slide_image_fallbacks_total{step="normalize"}).
const (
	FallbackNoSource     = "no_source"
	FallbackTooLarge     = "too_large"
	FallbackRenderFailed = "render_failed"
	FallbackCap          = "cap"
)

// Images switches slide images on for one Normalize run (templates.md
// §3.2). After the run it holds the counts of the run.
type Images struct {
	// Store keeps one PNG under the lowercase hex of its SHA-256 (a job
	// blob, or a file for recap tool normalize). It is called once per
	// distinct image.
	Store func(key string, png []byte) error
	// Fallback, if set, is called once per shown page without an image,
	// with its reason.
	Fallback func(reason string)

	// Counts of the run, set by Normalize.
	Attempts int // render attempts (§3.2 "Dedupe and cap" item 2)
	Stored   int // distinct images
}

// renderKey identifies a render: the SHA-256 of the source file and the
// page (§3.2 "Dedupe and cap" item 1).
type renderKey struct {
	sha  string
	page int
}

// rendered is the result of one render: the image ref, or the reason why
// there is none.
type rendered struct {
	ref, reason string
}

// imager finds, renders, dedupes and stores the images of one run.
type imager struct {
	fsys    fs.FS
	files   map[string]manifest.File
	opt     *Images
	r       *slideimage.Renderer
	renders map[renderKey]rendered
	failed  map[string]bool // source files not to be opened again
	stored  map[string]bool
	openSHA string // the PDF open in the renderer
}

func newImager(fsys fs.FS, files []manifest.File, opt *Images) *imager {
	im := &imager{fsys: fsys, files: map[string]manifest.File{}, opt: opt, r: slideimage.New(),
		renders: map[renderKey]rendered{}, failed: map[string]bool{}, stored: map[string]bool{}}
	for _, f := range files {
		if f.Role == manifest.RolePresentation {
			im.files[f.Path] = f
		}
	}
	return im
}

func (im *imager) close() { im.r.Close() }

// source returns the source file of a deck's page images (§3.2 "Source of
// a page image"): the deck's PDF, else its only image upload. pdf tells
// which; ok is false when there is none.
func (im *imager) source(deck string) (f manifest.File, pdf, ok bool) {
	dir := "presentation/" + deck + "/" + deck + "."
	if f, ok := im.files[dir+"pdf"]; ok {
		return f, true, true
	}
	var found []manifest.File
	for _, ext := range uploadExts {
		if f, ok := im.files[dir+ext]; ok {
			found = append(found, f)
		}
	}
	if len(found) == 1 {
		return found[0], false, true
	}
	return manifest.File{}, false, false
}

// image returns the image ref ("sha256:<hex>") of a page of a deck whose ID
// passed the path check, or "" without one. An error is a failed read of a
// package file (a step error, §3.2).
func (im *imager) image(deck string, page int) (string, error) {
	res, err := im.render(deck, page)
	if err != nil {
		return "", err
	}
	if res.ref == "" && im.opt.Fallback != nil {
		im.opt.Fallback(res.reason)
	}
	return res.ref, nil
}

func (im *imager) render(deck string, page int) (rendered, error) {
	src, pdf, ok := im.source(deck)
	if !ok || (!pdf && page != 1) {
		return rendered{reason: FallbackNoSource}, nil
	}
	if src.Size > maxSourceBytes {
		return rendered{reason: FallbackTooLarge}, nil
	}
	key := renderKey{src.SHA256, page}
	if res, ok := im.renders[key]; ok {
		return res, nil
	}
	if im.failed[src.SHA256] {
		return rendered{reason: FallbackRenderFailed}, nil
	}
	if im.opt.Attempts >= maxRenderAttempts {
		return rendered{reason: FallbackCap}, nil
	}
	var res rendered
	var err error
	if pdf {
		res, err = im.renderPDF(src, page)
	} else {
		res, err = im.decodeUpload(src)
	}
	if err != nil {
		return rendered{}, err
	}
	im.renders[key] = res
	return res, nil
}

// renderPDF renders one page of a PDF source. A file that does not open
// counts as one attempt and is not opened again; so is a file in which a
// limit was hit (that hit is the attempt of the page).
func (im *imager) renderPDF(src manifest.File, page int) (rendered, error) {
	if im.openSHA != src.SHA256 {
		im.openSHA = ""
		data, err := im.read(src.Path)
		if err != nil {
			return rendered{}, err
		}
		if _, err := im.r.OpenPDF(data); err != nil {
			im.opt.Attempts++
			im.failed[src.SHA256] = true
			return rendered{reason: FallbackRenderFailed}, nil
		}
		im.openSHA = src.SHA256
	}
	img, err := im.r.RenderPage(page)
	switch {
	case errors.Is(err, slideimage.ErrNoPage):
		return rendered{reason: FallbackRenderFailed}, nil
	case err != nil:
		im.opt.Attempts++
		im.failed[src.SHA256] = true
		im.openSHA = ""
		return rendered{reason: FallbackRenderFailed}, nil
	}
	im.opt.Attempts++
	return im.store(img)
}

// decodeUpload decodes an image upload (page 1). A source over the pixel
// limit is no attempt; one that does not decode is.
func (im *imager) decodeUpload(src manifest.File) (rendered, error) {
	data, err := im.read(src.Path)
	if err != nil {
		return rendered{}, err
	}
	im.openSHA = "" // DecodeUpload closes the renderer instance first
	img, err := im.r.DecodeUpload(data)
	switch {
	case errors.Is(err, slideimage.ErrTooLarge):
		return rendered{reason: FallbackTooLarge}, nil
	case err != nil:
		im.opt.Attempts++
		return rendered{reason: FallbackRenderFailed}, nil
	}
	im.opt.Attempts++
	return im.store(img)
}

// store encodes the image and writes the PNG once per distinct content
// (§3.2 "Dedupe and cap" item 3).
func (im *imager) store(img *image.RGBA) (rendered, error) {
	b, err := slideimage.EncodePNG(img)
	if err != nil {
		return rendered{}, err
	}
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if !im.stored[key] {
		if err := im.opt.Store(key, b); err != nil {
			return rendered{}, err
		}
		im.stored[key] = true
		im.opt.Stored++
	}
	return rendered{ref: meeting.ImagePrefix + key}, nil
}

// read reads a whole package file. Every error is a step error: the file is
// in the package, so a failed read is a blob store failure (§3.2).
func (im *imager) read(p string) ([]byte, error) {
	f, err := im.fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
}
