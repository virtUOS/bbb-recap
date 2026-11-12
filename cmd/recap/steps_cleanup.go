package main

import (
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/cleanup"
)

// cleanupHandler returns the handler of the cleanup step
// (privacy.md §5.1).
func cleanupHandler(d *deps) (queue.Handler, error) {
	return cleanup.Handler(cleanup.Deps{
		Pool: d.pool, Blobs: d.blobs, Results: d.results, ResultTTL: d.cfg.ResultTTL, Log: d.log,
	}), nil
}
