package main

import (
	"context"

	"github.com/virtUOS/bbb-recap/internal/retention"
)

// cleanupLoop returns the worker's cleanup loop (architecture.md §8,
// privacy.md §5.3), run in a goroutine until ctx ends.
func cleanupLoop(d *deps) func(context.Context) {
	l := &retention.Loop{
		Pool:              d.pool,
		Blobs:             d.blobs,
		Log:               d.log,
		StagingTTL:        d.cfg.IngestStagingTTL,
		MetadataRetention: d.cfg.JobMetadataRetention,
		ResultTTL:         d.cfg.ResultTTL,
		Results:           d.results,
	}
	return l.Run
}
