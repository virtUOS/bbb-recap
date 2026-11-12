package main

import (
	"net/http"

	"github.com/virtUOS/bbb-recap/internal/admin"
)

// mountAdmin mounts the admin API (architecture.md §4, §5.4) on mux: job
// list and job, actions, audit.
func mountAdmin(mux *http.ServeMux, d *deps) error {
	admin.New(admin.Config{
		Pool:                   d.pool,
		Blobs:                  d.blobs,
		Results:                d.results,
		ConfigHash:             d.cfg.Hash(),
		RetentionAfterDelivery: d.cfg.RetentionAfterDelivery,
		Log:                    d.log,
	}).Mount(mux, d.creds)
	return nil
}
