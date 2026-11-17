package main

import (
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/render"
)

// renderHandler returns the handler of the render step.
func renderHandler(d *deps) (queue.Handler, error) {
	// Only the sinks that carry the summary limit its size (sinks.md
	// §2.4): a webhook with content notice sends title and links.
	limits := map[string]int64{}
	for _, s := range d.cfg.Sinks {
		if s.Type != config.SinkTypeWebhook || s.Content == config.WebhookContentSummary {
			limits[s.Name] = s.MaxDocumentBytes
		}
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
