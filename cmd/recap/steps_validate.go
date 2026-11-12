package main

import (
	"os"
	"path/filepath"

	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/validate"
)

// validateHandler returns the handler of the validate step.
func validateHandler(d *deps) (queue.Handler, error) {
	c := validate.Configured{
		MeetingTypes:  configuredMeetingTypes(d.cfg),
		FrontendTypes: d.cfg.FrontendTypes,
		FrontendSinks: d.cfg.FrontendSinks,
	}
	for _, s := range d.cfg.Sinks {
		c.Sinks = append(c.Sinks, s.Name)
	}
	return validate.Handler(validate.Deps{
		Pool:            d.pool,
		Blobs:           d.blobs,
		Log:             d.log,
		MetaPrefix:      d.cfg.MetaPrefix,
		Configured:      c,
		FrontendServers: d.cfg.FrontendServers,
	}), nil
}

// configuredMeetingTypes lists the meeting types of the meeting-types file
// whose format directory exists under RECAP_FORMATS_DIR. A type whose
// format is missing is left out, so validate holds its jobs unknown_type
// before any provider call (templates.md §1). A present but invalid format
// is caught later by summarize/render (config_error).
func configuredMeetingTypes(cfg *config.Config) []string {
	var types []string
	for _, mt := range cfg.MeetingTypes {
		if st, err := os.Stat(filepath.Join(cfg.FormatsDir, mt.Format)); err == nil && st.IsDir() {
			types = append(types, mt.Name)
		}
	}
	return types
}
