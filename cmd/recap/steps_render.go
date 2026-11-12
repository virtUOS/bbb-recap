package main

import (
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/render"
)

// renderHandler returns the handler of the render step.
func renderHandler(d *deps) (queue.Handler, error) {
	limits := map[string]int64{}
	for _, s := range d.cfg.Sinks {
		limits[s.Name] = s.MaxDocumentBytes
	}
	return render.Handler(render.Deps{
		Pool:             d.pool,
		Blobs:            d.blobs,
		Log:              d.log,
		FormatsDir:       d.cfg.FormatsDir,
		Zone:             d.cfg.DisplayTZ,
		MaxDocumentBytes: limits,
	}), nil
}
