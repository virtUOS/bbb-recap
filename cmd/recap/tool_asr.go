package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

// asrLanguageRE is the grammar of RECAP_ASR_LANGUAGE (ISO 639-1).
var asrLanguageRE = regexp.MustCompile(`^[a-z]{2}$`)

// asrToolInput is the stdin of `recap tool asr` (contracts/providers/README.md).
type asrToolInput struct {
	Profile string `json:"profile"`
	Request *struct {
		JobID          string `json:"job_id"`
		TrackRef       string `json:"track_ref"`
		IdempotencyKey string `json:"idempotency_key"`
		Audio          struct {
			Path      string `json:"path"`
			MediaType string `json:"media_type"`
			Size      int64  `json:"size"`
			SHA256    string `json:"sha256"`
		} `json:"audio"`
		DurationMS int64 `json:"duration_ms"`
	} `json:"request"`
	Operations       []string `json:"operations"`
	AttemptTimeoutMS int64    `json:"attempt_timeout_ms"`
}

// toolASR is `recap tool asr`: it loads the providers file with the
// service's start rules, then runs the listed adapter operations on one
// handle and prints one entry per operation. It never reads the service
// configuration. Logs go to stderr.
func toolASR(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool asr"
	if len(args) > 0 {
		fmt.Fprintf(stderr, "Usage: %s < request.json\n", prog)
		return exitUsage
	}
	level := slog.LevelInfo
	if v := os.Getenv("RECAP_LOG_LEVEL"); v != "" {
		var err error
		if level, err = logging.ParseLevel(v); err != nil {
			fmt.Fprintf(stderr, "%s: RECAP_LOG_LEVEL: %v\n", prog, err)
			return exitUsage
		}
	}
	log := logging.New(stderr, level)

	path := os.Getenv("RECAP_PROVIDERS_FILE")
	if path == "" {
		fmt.Fprintf(stderr, "%s: RECAP_PROVIDERS_FILE: required\n", prog)
		return exitUsage
	}
	set, err := providers.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid providers file (RECAP_PROVIDERS_FILE):\n%v\n", prog, err)
		return exitUsage
	}
	set.LogWarnings(log)
	lang := os.Getenv("RECAP_ASR_LANGUAGE")
	if lang != "" && !asrLanguageRE.MatchString(lang) {
		fmt.Fprintf(stderr, "%s: RECAP_ASR_LANGUAGE: want an ISO 639-1 code or empty\n", prog)
		return exitUsage
	}

	var in asrToolInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object: %v\n", prog, err)
		return exitUsage
	}
	if len(in.Operations) == 0 {
		return writeJSON(stdout, map[string]any{"operations": []any{}})
	}
	if in.Operations[0] != "submit" {
		fmt.Fprintf(stderr, "%s: operations: the first operation must be submit\n", prog)
		return exitUsage
	}
	for _, op := range in.Operations {
		if !slices.Contains([]string{"submit", "status", "result", "cleanup"}, op) {
			fmt.Fprintf(stderr, "%s: operations: unknown operation %q\n", prog, op)
			return exitUsage
		}
	}
	if in.Request == nil {
		fmt.Fprintf(stderr, "%s: request: required with operations\n", prog)
		return exitUsage
	}
	profile, err := set.Get(providers.KindASR, in.Profile)
	if err != nil {
		fmt.Fprintf(stderr, "%s: profile: %v\n", prog, err)
		return exitUsage
	}
	adapter, err := newASR(profile, log)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitUsage
	}

	f, err := os.Open(in.Request.Audio.Path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: request.audio.path: cannot open the audio file\n", prog)
		return exitFailure
	}
	defer func() { _ = f.Close() }()
	opened := false
	req := providers.AsrRequest{
		JobID:          in.Request.JobID,
		TrackRef:       in.Request.TrackRef,
		IdempotencyKey: in.Request.IdempotencyKey,
		Audio: providers.Audio{
			// The file is streamed once, by the one submit.
			Open: func(context.Context) (io.ReadCloser, error) {
				if opened {
					return nil, errors.New("audio already sent")
				}
				opened = true
				return f, nil
			},
			Size:      in.Request.Audio.Size,
			SHA256:    in.Request.Audio.SHA256,
			MediaType: in.Request.Audio.MediaType,
		},
		DurationMS:     in.Request.DurationMS,
		LanguageHint:   lang,
		AttemptTimeout: time.Duration(in.AttemptTimeoutMS) * time.Millisecond,
	}

	ctx := context.Background()
	var h providers.AsrHandle
	out := make([]map[string]any, 0, len(in.Operations))
	for _, op := range in.Operations {
		entry := map[string]any{"op": op}
		var err error
		switch op {
		case "submit":
			h, err = adapter.Submit(ctx, req)
		case "status":
			var st providers.AsrStatus
			st, err = adapter.Status(ctx, h)
			entry["status"] = st
		case "result":
			var r *providers.AsrResult
			r, err = adapter.Result(ctx, h)
			entry["result"] = r
		case "cleanup":
			err = adapter.Cleanup(ctx, h)
		}
		entry["error"] = errorJSON(err)
		out = append(out, entry)
	}
	return writeJSON(stdout, map[string]any{"operations": out})
}

// errorJSON is an adapter error in tool output: class, code, retry_after_ms.
func errorJSON(err error) any {
	if err == nil {
		return nil
	}
	var e *providers.Error
	if !errors.As(err, &e) {
		e = &providers.Error{Class: providers.InvalidResponse, Code: "internal"}
	}
	var retry any
	if e.RetryAfter > 0 {
		retry = e.RetryAfter.Milliseconds()
	}
	return map[string]any{"class": e.Class, "code": e.Code, "retry_after_ms": retry}
}

// writeJSON prints v as one JSON line on stdout.
func writeJSON(stdout io.Writer, v any) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return exitFailure
	}
	_, _ = stdout.Write(b.Bytes())
	return exitOK
}
