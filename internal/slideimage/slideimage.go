// Package slideimage turns slide sources into the PNG images of
// docs/reference/templates.md §3.2 (ADR-017): it renders PDF pages with
// PDFium compiled to WebAssembly (github.com/klippa-app/go-pdfium in
// WebAssembly mode, run by the pure-Go runtime wazero, no CGO) and decodes
// image uploads (PNG, JPEG, WebP) in Go.
//
// Constants of §3.2 "Rendering" (provisional until the staging measurement
// of ADR-017, D11):
//
//   - a PDF page is rendered as displayed (crop box, page rotation) with its
//     long edge at LongEdge pixels; the short edge is short × LongEdge / long,
//     rounded half up, at least 1;
//   - an upload above MaxPixels (checked from its header only) gives no image
//     (ErrTooLarge); below it is decoded and scaled down by the same rule if
//     its long edge is over LongEdge, never up;
//   - the result is composited on opaque white and written as PNG, 8 bits
//     per channel, colour type 2, without ancillary chunks (EncodePNG);
//   - one renderer at a time per process: a Renderer holds a process-wide
//     lock from its first use to Close. The PDF is passed as bytes and the
//     WebAssembly module sees no filesystem. Its linear memory is capped at
//     MemoryLimit, opening a document at OpenTimeout and rendering a page at
//     PageTimeout. A limit hit (or any other failure inside the module)
//     discards the instance (ErrLimit); the next document gets a new one.
//
// Nothing here logs; callers log codes and sizes only.
package slideimage

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // upload decoder
	"image/png"
	"io"
	"runtime/debug"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // upload decoder (upstream-facts.md "Providers")
)

// Constants of templates.md §3.2; constants, not settings.
const (
	LongEdge    = 1280
	MaxPixels   = 25_000_000
	MemoryLimit = 512 << 20
	OpenTimeout = 10 * time.Second
	PageTimeout = 10 * time.Second
	// startTimeout bounds starting a new WebAssembly instance (not a limit of
	// §3.2; starting is not document work). A start that fails counts as a
	// limit hit.
	startTimeout = 60 * time.Second
	// killGrace is how long a call may take to return after its module was
	// closed; after it the call is abandoned.
	killGrace = 5 * time.Second
)

// wasmPage is the WebAssembly page size.
const wasmPage = 64 << 10

// Errors. ErrOpen: the PDF does not open (parse error). ErrLimit: a time or
// memory limit was hit, or the module failed; the instance was discarded
// and the document must not be opened again. ErrNoPage: the page number is
// beyond the page count. ErrTooLarge: an upload over MaxPixels. ErrDecode:
// an upload that does not decode.
var (
	ErrOpen     = errors.New("slideimage: document does not open")
	ErrLimit    = errors.New("slideimage: renderer limit hit")
	ErrNoPage   = errors.New("slideimage: page beyond the page count")
	ErrTooLarge = errors.New("slideimage: image over the pixel limit")
	ErrDecode   = errors.New("slideimage: image does not decode")
)

// processLock makes one Renderer at a time per process (§3.2 item 4).
var processLock sync.Mutex

// Renderer renders PDF pages and decodes uploads for one normalize run. It
// is not safe for concurrent use. The zero value is not usable; use New.
// Close must be called.
type Renderer struct {
	locked bool
	pool   pdfium.Pool
	inst   pdfium.Pdfium
	doc    *references.FPDF_DOCUMENT
	pages  int
}

// New returns a renderer. It takes the process-wide lock at its first use,
// so a renderer that is never used costs nothing.
func New() *Renderer { return &Renderer{} }

func (r *Renderer) lock() {
	if !r.locked {
		processLock.Lock()
		r.locked = true
	}
}

// Close discards the instance and the WebAssembly runtime and releases the
// process-wide lock.
func (r *Renderer) Close() {
	r.closeInstance()
	if r.pool != nil {
		_ = r.pool.Close()
		r.pool = nil
	}
	if r.locked {
		processLock.Unlock()
		r.locked = false
	}
}

// closeInstance closes the document and the instance, so the memory of the
// instance is returned. The runtime with the compiled module stays for the
// next instance.
func (r *Renderer) closeInstance() {
	if r.inst != nil {
		if r.doc != nil {
			_, _ = r.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: *r.doc})
		}
		_ = r.inst.Close()
	}
	r.inst, r.doc, r.pages = nil, nil, 0
}

// kill discards an instance whose call ran over its limit: closing the
// module's context stops the guest code (wazero WithCloseOnContextDone).
func (r *Renderer) kill() {
	if r.inst != nil {
		_ = r.inst.Kill()
	}
	r.inst, r.doc, r.pages = nil, nil, 0
}

// zeroReader is the module's random source: rendering does not need
// randomness, and a fixed source keeps runs identical.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// start makes a new instance with no filesystem and the memory cap. The
// runtime and the compiled module are made once per Renderer.
func (r *Renderer) start() error {
	if r.pool == nil {
		cfg := wazero.NewRuntimeConfig().
			// The bundled PDFium module needs exception handling (go-pdfium
			// webassembly.Config.RuntimeConfig).
			WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
			WithMemoryLimitPages(MemoryLimit / wasmPage).
			WithCloseOnContextDone(true)
		pool, err := webassembly.Init(webassembly.Config{
			MinIdle:       0,
			MaxIdle:       1,
			MaxTotal:      1,
			FSConfig:      wazero.NewFSConfig(), // nothing mounted (go-pdfium mounts / without it)
			RuntimeConfig: cfg,
			Stdout:        io.Discard,
			Stderr:        io.Discard,
			RandomSource:  zeroReader{},
		})
		if err != nil {
			return fmt.Errorf("%w: start: %v", ErrLimit, err)
		}
		r.pool = pool
		// Compiling the module leaves a few hundred MB of garbage on the Go
		// heap; returning it now keeps it from adding to the peak of the
		// render run (measured on F-14: about 330 MB instead of up to 535 MB).
		debug.FreeOSMemory()
	}
	inst, err := r.pool.GetInstance(startTimeout)
	if err != nil {
		return fmt.Errorf("%w: instance: %v", ErrLimit, err)
	}
	r.inst = inst
	return nil
}

// within runs f with a time limit. On a timeout or a panic the instance is
// discarded and ErrLimit returned.
func (r *Renderer) within(limit time.Duration, f func() error) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("%w: panic in the renderer", ErrLimit)
			}
		}()
		done <- f()
	}()
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case err := <-done:
		if errors.Is(err, ErrLimit) {
			r.kill()
		}
		return err
	case <-t.C:
		r.kill()
		select {
		case err := <-done:
			_ = err // the call returned after its module was closed
		case <-time.After(killGrace):
		}
		return fmt.Errorf("%w: time limit", ErrLimit)
	}
}

// OpenPDF opens a PDF from its bytes and returns its page count; an open
// document is closed first. ErrOpen when it does not parse, ErrLimit when a
// limit was hit.
func (r *Renderer) OpenPDF(data []byte) (int, error) {
	r.lock()
	if r.inst != nil && r.doc != nil {
		_, _ = r.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: *r.doc})
		r.doc, r.pages = nil, 0
	}
	if r.inst == nil {
		if err := r.start(); err != nil {
			return 0, err
		}
	}
	inst := r.inst
	var doc references.FPDF_DOCUMENT
	var pages int
	err := r.within(OpenTimeout, func() error {
		res, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
		if err != nil {
			if isPDFError(err) {
				return fmt.Errorf("%w: %v", ErrOpen, err)
			}
			return fmt.Errorf("%w: open: %v", ErrLimit, err)
		}
		doc = res.Document
		n, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc})
		if err != nil {
			return fmt.Errorf("%w: page count: %v", ErrLimit, err)
		}
		pages = n.PageCount
		return nil
	})
	if err != nil {
		return 0, err
	}
	r.doc, r.pages = &doc, pages
	return pages, nil
}

// isPDFError reports the PDFium load errors: the document itself is bad.
func isPDFError(err error) bool {
	for _, e := range []error{pdfiumerrors.ErrUnknown, pdfiumerrors.ErrFile, pdfiumerrors.ErrFormat,
		pdfiumerrors.ErrPassword, pdfiumerrors.ErrSecurity} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// RenderPage renders page (1-based) of the open document as displayed, its
// long edge at LongEdge pixels, on opaque white. ErrNoPage beyond the page
// count; ErrLimit on a limit hit or any failure inside the module (the
// instance is then discarded and the document closed).
func (r *Renderer) RenderPage(page int) (*image.RGBA, error) {
	if r.inst == nil || r.doc == nil {
		return nil, fmt.Errorf("%w: no open document", ErrLimit)
	}
	if page < 1 || page > r.pages {
		return nil, ErrNoPage
	}
	inst, doc := r.inst, *r.doc
	var img *image.RGBA
	err := r.within(PageTimeout, func() error {
		var err error
		img, err = renderPage(inst, doc, page-1)
		if err != nil {
			return fmt.Errorf("%w: render: %v", ErrLimit, err)
		}
		return nil
	})
	return img, err
}

func renderPage(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, index int) (*image.RGBA, error) {
	p, err := inst.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc, Index: index})
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = inst.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: p.Page}) }()
	ref := requests.Page{ByReference: &p.Page}
	// The page size as displayed: PDFium applies the crop box and the
	// page rotation.
	w, err := inst.FPDF_GetPageWidthF(&requests.FPDF_GetPageWidthF{Page: ref})
	if err != nil {
		return nil, err
	}
	h, err := inst.FPDF_GetPageHeightF(&requests.FPDF_GetPageHeightF{Page: ref})
	if err != nil {
		return nil, err
	}
	width, height, ok := FitLongEdge(float64(w.PageWidth), float64(h.PageHeight))
	if !ok {
		return nil, errors.New("page without size")
	}
	bm, err := inst.FPDFBitmap_Create(&requests.FPDFBitmap_Create{Width: width, Height: height, Alpha: 0})
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = inst.FPDFBitmap_Destroy(&requests.FPDFBitmap_Destroy{Bitmap: bm.Bitmap}) }()
	// Opaque white first: PDFium composites the page on it.
	if _, err := inst.FPDFBitmap_FillRect(&requests.FPDFBitmap_FillRect{Bitmap: bm.Bitmap, Width: width,
		Height: height, Color: 0xFFFFFFFF}); err != nil {
		return nil, err
	}
	if _, err := inst.FPDF_RenderPageBitmap(&requests.FPDF_RenderPageBitmap{Bitmap: bm.Bitmap, Page: ref,
		SizeX: width, SizeY: height, Flags: enums.FPDF_RENDER_FLAG_ANNOT}); err != nil {
		return nil, err
	}
	stride, err := inst.FPDFBitmap_GetStride(&requests.FPDFBitmap_GetStride{Bitmap: bm.Bitmap})
	if err != nil {
		return nil, err
	}
	buf, err := inst.FPDFBitmap_GetBuffer(&requests.FPDFBitmap_GetBuffer{Bitmap: bm.Bitmap})
	if err != nil {
		return nil, err
	}
	if stride.Stride < width*4 || len(buf.Buffer) < stride.Stride*height {
		return nil, errors.New("short bitmap buffer")
	}
	// The bitmap is BGRx (no alpha); copy it out before it is destroyed.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		src := buf.Buffer[y*stride.Stride : y*stride.Stride+width*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for i := 0; i < len(src); i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 0xFF
		}
	}
	return img, nil
}

// FitLongEdge returns the size of an image whose long edge is LongEdge and
// whose short edge is short × LongEdge / long, rounded half up, at least 1
// (§3.2 item 1). ok is false for a size that is not positive and finite.
func FitLongEdge(w, h float64) (width, height int, ok bool) {
	if !(w > 0 && h > 0) || w > 1e9 || h > 1e9 {
		return 0, 0, false
	}
	short := func(s, l float64) int { return max(int(s*LongEdge/l+0.5), 1) }
	if w >= h {
		return LongEdge, short(h, w), true
	}
	return short(w, h), LongEdge, true
}

// DecodeUpload decodes an image upload (PNG, JPEG, WebP). Its header is
// checked first: above MaxPixels it gives ErrTooLarge without decoding.
// An open renderer instance is closed before the decode, so the two never
// hold memory at the same time. A long edge over LongEdge is scaled down
// by the rule of FitLongEdge; the result is composited on opaque white.
func (r *Renderer) DecodeUpload(data []byte) (*image.RGBA, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return nil, ErrDecode
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrTooLarge
	}
	r.lock()
	r.closeInstance()
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	b := src.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Over)
	if max(b.Dx(), b.Dy()) <= LongEdge {
		return flat, nil
	}
	w, h, _ := FitLongEdge(float64(b.Dx()), float64(b.Dy()))
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(out, out.Bounds(), flat, flat.Bounds(), xdraw.Src, nil)
	return out, nil
}

// EncodePNG writes an opaque image as PNG: 8 bits per channel, colour type 2
// (RGB, no alpha), no ancillary chunks. Every pixel's alpha is set to 255
// first, so Go's encoder picks colour type 2.
func EncodePNG(img *image.RGBA) ([]byte, error) {
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xFF
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
