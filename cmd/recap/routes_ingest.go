package main

import (
	"net/http"

	"github.com/virtUOS/bbb-recap/internal/ingest"
)

// mountIngest mounts the ingest API (bbb-exporter.md §6) on mux.
func mountIngest(mux *http.ServeMux, d *deps) error {
	ingest.New(ingest.Config{
		Pool:       d.pool,
		Blobs:      d.blobs,
		Creds:      d.creds,
		MetaPrefix: d.cfg.MetaPrefix,
		ConfigHash: d.cfg.Hash(),
		Log:        d.log,
	}).Mount(mux)
	return nil
}
