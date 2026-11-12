package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/config"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/sink"
	"github.com/virtUOS/bbb-recap/internal/steps/deliver"
)

// sinkToolInput is the stdin of `recap tool sink` (contracts/sink/README.md).
type sinkToolInput struct {
	Request          *sink.Request `json:"request"`
	Delivery         *sink.Record  `json:"delivery"`
	RequestTimeoutMS int64         `json:"request_timeout_ms"`
}

// sinkToolError is the error field of the output: class and code only.
type sinkToolError struct {
	Class string `json:"class"`
	Code  string `json:"code"`
}

// sinkToolOutput is the stdout of `recap tool sink`. It never holds the
// title, the content, the sub or the email.
type sinkToolOutput struct {
	Outcome       string         `json:"outcome"`
	HeldReason    *string        `json:"held_reason"`
	FailureReason *string        `json:"failure_reason"`
	Error         *sinkToolError `json:"error"`
	Delivery      *sink.Record   `json:"delivery"`
}

// toolSink is `recap tool sink`: one deliver call (sinks.md §1.5) with the
// profile of request.sink_profile from RECAP_SINKS_FILE, at most one HTTP
// request. A frontend profile (§4a) stores the summary in the filesystem
// blob store of RECAP_FS_ROOT instead and sends nothing. The input record stands in for the deliveries row: one in
// sending is the re-entry of §1.4. Logs go to stderr.
func toolSink(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool sink"
	if len(args) > 0 {
		fmt.Fprintf(stderr, "Usage: %s < input.json\n", prog)
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

	path := os.Getenv("RECAP_SINKS_FILE")
	if path == "" {
		fmt.Fprintf(stderr, "%s: RECAP_SINKS_FILE: required\n", prog)
		return exitUsage
	}
	profiles, err := config.LoadSinks(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid sinks file (RECAP_SINKS_FILE):\n%v\n", prog, err)
		return exitUsage
	}
	config.LogSinkWarnings(log, profiles)

	var in sinkToolInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object\n", prog) // the decoder error may quote content
		return exitUsage
	}
	if in.Request == nil || in.Request.JobID == "" || in.Request.SinkProfile == "" {
		fmt.Fprintf(stderr, "%s: request with job_id and sink_profile: required\n", prog)
		return exitUsage
	}
	if in.RequestTimeoutMS < 0 {
		fmt.Fprintf(stderr, "%s: request_timeout_ms: want a positive number\n", prog)
		return exitUsage
	}
	var profile *config.SinkProfile
	for i := range profiles {
		if profiles[i].Name == in.Request.SinkProfile {
			profile = &profiles[i]
		}
	}
	if profile == nil {
		fmt.Fprintf(stderr, "%s: request.sink_profile: no profile %q in RECAP_SINKS_FILE\n", prog, in.Request.SinkProfile)
		return exitUsage
	}
	if in.RequestTimeoutMS > 0 {
		profile.Timeout = time.Duration(in.RequestTimeoutMS) * time.Millisecond
	}
	d := &deps{log: log}
	if profile.Type == config.SinkTypeFrontend {
		// The frontend sink stores in the blob store (sinks.md §4a); the
		// tool takes the filesystem backend only (contracts/sink/README.md).
		if os.Getenv("RECAP_BLOB_BACKEND") != "fs" || os.Getenv("RECAP_FS_ROOT") == "" {
			fmt.Fprintf(stderr, "%s: a frontend profile needs RECAP_BLOB_BACKEND=fs and RECAP_FS_ROOT\n", prog)
			return exitUsage
		}
		store, err := blob.NewFS(os.Getenv("RECAP_FS_ROOT"))
		if err != nil {
			fmt.Fprintf(stderr, "%s: RECAP_FS_ROOT: %v\n", prog, err)
			return exitUsage
		}
		d.blobs, d.results = store, store
	}
	s, err := newSink(*profile, d)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitUsage
	}

	var rec *sink.Record
	if s.Idempotent {
		rec, err = sink.Keep(context.Background(), s.Sender, *in.Request, in.Delivery, s.MaxDocumentBytes)
	} else {
		rec, err = sink.Deliver(context.Background(), s.Sender, *in.Request, in.Delivery, s.MaxDocumentBytes, nil)
	}
	var serr *sink.Error
	if err != nil && !errors.As(err, &serr) {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitFailure
	}
	res := deliver.Result(rec, err)
	out := sinkToolOutput{Outcome: string(res.Outcome)}
	if err == nil || serr.Class == sink.Uncertain {
		out.Delivery = rec
	}
	if serr != nil {
		out.Error = &sinkToolError{Class: string(serr.Class), Code: serr.Code}
		reason := res.Reason
		switch res.Outcome {
		case queue.Hold:
			out.HeldReason = &reason
		case queue.Permanent:
			out.FailureReason = &reason
		}
	}
	log.Info("sink tool: done", "job_id", in.Request.JobID, "profile", profile.Name, "outcome", out.Outcome)
	return writeJSON(stdout, out)
}
