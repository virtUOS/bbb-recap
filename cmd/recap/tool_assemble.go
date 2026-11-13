package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
)

// assembleToolInput is the stdin of `recap tool assemble`
// (contracts/transcript/README.md "Assemble form").
type assembleToolInput struct {
	JobID      string              `json:"job_id"`
	CreatedAt  string              `json:"created_at"`
	Normalized json.RawMessage     `json:"normalized"`
	ASR        []assemble.TrackASR `json:"asr"`
}

// toolAssemble is `recap tool assemble [--check]`: without --check it reads
// the assemble input on stdin and prints the transcript; with --check it
// reads a transcript and prints the verdict {"valid": bool, "error"?}, exit
// 0 whenever a verdict was produced. Logs go to stderr at RECAP_LOG_LEVEL
// (default info) and never carry transcript content.
func toolAssemble(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool assemble"
	if len(args) > 1 || (len(args) == 1 && args[0] != "--check") {
		fmt.Fprintf(stderr, "Usage: %s [--check] < input.json\n", prog)
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
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read stdin\n", prog)
		return exitFailure
	}

	if len(args) == 1 {
		verdict := map[string]any{"valid": true}
		if err := assemble.Check(data); err != nil {
			msg := err.Error()
			if errors.Is(err, assemble.ErrSchema) {
				// The validator's text names the schema by a build-host file
				// URL; name it by its $id instead.
				msg = "does not match the schema urn:recap:schema:transcript:1"
			}
			verdict = map[string]any{"valid": false, "error": msg}
		}
		log.Debug("checked", "valid", verdict["valid"], "bytes", len(data))
		return writeJSON(stdout, verdict)
	}

	var in assembleToolInput
	if err := json.Unmarshal(data, &in); err != nil || in.Normalized == nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object with job_id, created_at, normalized and asr\n", prog)
		return exitUsage
	}
	m, err := meeting.Decode(in.Normalized)
	if err != nil {
		fmt.Fprintf(stderr, "%s: normalized: %v\n", prog, err)
		return exitUsage
	}
	created, err := time.Parse(time.RFC3339Nano, in.CreatedAt)
	if err != nil {
		fmt.Fprintf(stderr, "%s: created_at: want an RFC 3339 time\n", prog)
		return exitUsage
	}
	out, err := assemble.Assemble(assemble.Input{JobID: in.JobID, CreatedAt: created, Meeting: m,
		ASR: in.ASR})
	if err != nil {
		// Assemble errors name refs and counts, never content; schema
		// errors may quote values, so they are not printed.
		msg := "the transcript does not pass the schema"
		if !errors.Is(err, assemble.ErrSchema) {
			msg = err.Error()
		}
		log.Error("assemble failed", "err", msg)
		fmt.Fprintf(stderr, "%s: %s\n", prog, msg)
		return exitFailure
	}
	log.Debug("assembled", "tracks", len(m.Tracks), "asr_outcomes", len(in.ASR), "bytes", len(out))
	_, _ = stdout.Write(out)
	return exitOK
}
