package main

import (
	"github.com/virtUOS/bbb-recap/internal/adapters/openaichat"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/describe"
)

// describeHandler returns the handler of the describe step for the vision
// profile (defaults.vision). Without one no meeting type switches slide
// images on (the configuration check refuses that), and the step is a no-op.
func describeHandler(d *deps) (queue.Handler, error) {
	types := map[string]string{}
	for _, mt := range d.cfg.MeetingTypes {
		types[mt.Name] = mt.Format
	}
	deps := describe.Deps{
		Pool:            d.pool,
		Blobs:           d.blobs,
		Log:             d.log,
		FormatsDir:      d.cfg.FormatsDir,
		MeetingTypes:    types,
		SlideImageTypes: config.SlideImageTypes(d.cfg.MeetingTypes),
	}
	if p, ok := d.cfg.Providers.Vision(); ok {
		llm, err := newLLM(p, d.log)
		if err != nil {
			return nil, err
		}
		metrics.InitExternal(p.Name, metrics.KindVision, openaichat.Outcomes...)
		deps.LLM, deps.Profile, deps.Sem = llm, p, d.llmSem(p)
	}
	return describe.Handler(deps), nil
}
