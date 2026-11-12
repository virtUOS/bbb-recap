package main

import (
	"fmt"
	"log/slog"

	"github.com/virtUOS/bbb-recap/internal/adapters/transkribierbar"
	"github.com/virtUOS/bbb-recap/internal/adapters/whisperx"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/transcribe"
)

// transcribeHandler returns the handler of the transcribe step for
// the default ASR profile.
func transcribeHandler(d *deps) (queue.Handler, error) {
	p, err := d.cfg.Providers.Get(providers.KindASR, "")
	if err != nil {
		return nil, err
	}
	asr, err := newASR(p, d.log)
	if err != nil {
		return nil, err
	}
	return transcribe.Handler(transcribe.Deps{
		Pool:     d.pool,
		Blobs:    d.blobs,
		Log:      d.log,
		ASR:      asr,
		Profile:  p,
		Language: d.cfg.ASRLanguage,
	}), nil
}

// newASR returns the adapter of an ASR profile's dialect.
func newASR(p *providers.Profile, log *slog.Logger) (providers.ASR, error) {
	switch p.Dialect {
	case whisperx.Dialect:
		return whisperx.New(p, log), nil
	case transkribierbar.Dialect:
		return transkribierbar.New(p, log), nil
	}
	return nil, fmt.Errorf("profile %s: no adapter for ASR dialect %q", p.Name, p.Dialect)
}
