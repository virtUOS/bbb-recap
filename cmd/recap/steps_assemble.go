package main

import (
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
)

// assembleHandler returns the handler of the assemble step.
func assembleHandler(d *deps) (queue.Handler, error) {
	return assemble.Handler(assemble.Deps{Pool: d.pool, Blobs: d.blobs, Log: d.log}), nil
}
