package main

import (
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/normalize"
)

// normalizeHandler returns the handler of the normalize step.
func normalizeHandler(d *deps) (queue.Handler, error) {
	return normalize.Handler(normalize.Deps{Pool: d.pool, Blobs: d.blobs, Log: d.log}), nil
}
