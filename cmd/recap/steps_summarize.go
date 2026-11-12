package main

import (
	"fmt"
	"log/slog"

	"github.com/virtUOS/bbb-recap/internal/adapters/openaichat"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
)

// summarizeHandler returns the handler of the summarize step for the
// default LLM profile.
func summarizeHandler(d *deps) (queue.Handler, error) {
	p, err := d.cfg.Providers.Get(providers.KindLLM, "")
	if err != nil {
		return nil, err
	}
	llm, err := newLLM(p, d.log)
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	for _, mt := range d.cfg.MeetingTypes {
		types[mt.Name] = mt.Format
	}
	return summarize.Handler(summarize.Deps{
		Pool:            d.pool,
		Blobs:           d.blobs,
		Log:             d.log,
		LLM:             llm,
		Profile:         p,
		FormatsDir:      d.cfg.FormatsDir,
		MeetingTypes:    types,
		DefaultLanguage: d.cfg.DefaultLanguage,
	}), nil
}

// newLLM returns the adapter of an LLM profile's dialect.
func newLLM(p *providers.Profile, log *slog.Logger) (providers.LLM, error) {
	switch p.Dialect {
	case openaichat.Dialect:
		return openaichat.New(p, log), nil
	}
	return nil, fmt.Errorf("profile %s: no adapter for LLM dialect %q", p.Name, p.Dialect)
}
