package main

import (
	"fmt"

	"github.com/virtUOS/bbb-recap/internal/adapters/docs"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/sink"
	"github.com/virtUOS/bbb-recap/internal/steps/deliver"
)

// deliverHandler returns the handler of the deliver step with one
// sender per sink profile.
func deliverHandler(d *deps) (queue.Handler, error) {
	config.LogSinkWarnings(d.log, d.cfg.Sinks)
	sinks := map[string]deliver.Sink{}
	for _, p := range d.cfg.Sinks {
		s, err := newSink(p, d)
		if err != nil {
			return nil, err
		}
		sinks[p.Name] = s
	}
	return deliver.Handler(deliver.Deps{Pool: d.pool, Blobs: d.blobs, Log: d.log, Sinks: sinks}), nil
}

// newSink returns the step's view of a sink profile with the adapter of
// its type.
func newSink(p config.SinkProfile, d *deps) (deliver.Sink, error) {
	switch p.Type {
	case docs.Type:
		return deliver.Sink{
			Sender:           docs.New(p.Name, p.DocsBaseURL, p.APIKey, p.Timeout, d.log),
			MaxDocumentBytes: p.MaxDocumentBytes,
			MaxInflight:      p.MaxInflight,
		}, nil
	case config.SinkTypeFrontend:
		if d.results == nil {
			return deliver.Sink{}, fmt.Errorf("sink profile %s: type frontend needs a blob backend that keeps results", p.Name)
		}
		return deliver.Sink{
			Sender:           sink.Frontend{Results: d.results},
			Idempotent:       true,
			MaxDocumentBytes: p.MaxDocumentBytes,
			MaxInflight:      p.MaxInflight,
		}, nil
	}
	return deliver.Sink{}, fmt.Errorf("sink profile %s: no adapter for type %q", p.Name, p.Type)
}
