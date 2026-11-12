package main

import (
	"net/http"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/registration"
)

// mountRegistration mounts the registration API (metadata-interface.md §2) on mux.
func mountRegistration(mux *http.ServeMux, d *deps) error {
	registration.New(registration.Config{
		Pool:            d.pool,
		RegistrationTTL: d.cfg.RegistrationTTL,
		Sinks:           registrationSinks(d.cfg.Sinks),
		Results:         d.results,
		Log:             d.log,
	}).Mount(mux, d.creds)
	return nil
}

// registrationSinks returns what the registration API shows of each sink
// profile (metadata-interface.md §2.7).
func registrationSinks(profiles []config.SinkProfile) map[string]registration.Sink {
	sinks := map[string]registration.Sink{}
	for _, p := range profiles {
		sinks[p.Name] = registration.Sink{
			Frontend:            p.Type == config.SinkTypeFrontend,
			DocumentURLTemplate: p.DocumentURLTemplate,
		}
	}
	return sinks
}
