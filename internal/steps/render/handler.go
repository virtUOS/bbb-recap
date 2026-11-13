// Package render implements the render step (docs/reference/templates.md §5,
// §6; architecture.md §5.2): it reads the outputs of normalize, assemble
// and summarize, renders summary.md, transcript.vtt and the title with the
// render core (internal/render) and stores the three as blobs keyed by
// their hash. The step output holds the blob keys and the format
// provenance, never content (the title is personal data). A repeat gives
// the same bytes and keys (architecture.md §6.4): generated_at is the
// job's created_at.
//
// Nothing here logs content: only job IDs, codes and byte lengths.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	core "github.com/virtUOS/bbb-recap/internal/render"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
	"github.com/virtUOS/bbb-recap/internal/steps/summarize"
)

// Step is the step name.
const Step = "render"

// Failure and held reasons of the step (docs/reference/README.md).
const (
	ReasonConfigError    = "config_error"
	ReasonRejectedBySink = "rejected_by_sink"
	ReasonInternalError  = "internal_error"
	ReasonUnknownType    = "unknown_type"
)

// DefaultMaxDocumentBytes is the sink profile's default max_document_bytes
// (sinks.md §2.4: 2 MiB).
const DefaultMaxDocumentBytes = 2 << 20

const errorClassInternal = "internal"

// Deps are what the render handler needs.
type Deps struct {
	Pool       *pgxpool.Pool
	Blobs      blob.Store
	Log        *slog.Logger
	FormatsDir string         // RECAP_FORMATS_DIR
	Zone       *time.Location // RECAP_DISPLAY_TZ
	// MaxDocumentBytes maps a sink profile name to its max_document_bytes;
	// a missing or zero entry means DefaultMaxDocumentBytes.
	MaxDocumentBytes map[string]int64
}

// Output is the step output stored in steps.output: blob keys and the
// format provenance (templates.md §1).
type Output struct {
	SummarySHA256    string `json:"summary_md_sha256"`
	TranscriptSHA256 string `json:"transcript_vtt_sha256"`
	TitleSHA256      string `json:"title_sha256"`
	FormatID         string `json:"format_id"`
	FormatVersion    string `json:"format_version"`
	FormatSHA256     string `json:"format_sha256"`
	Language         string `json:"language"`
}

// job is what the handler reads from the database.
type job struct {
	sinkProfile *string
	createdAt   time.Time
	summarize   summarize.Output
	in          core.Input
}

// Handler returns the render step handler.
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		j, err := d.load(ctx, run.JobID)
		switch {
		case errors.Is(err, format.ErrNotFound):
			d.Log.Warn("render: format not found", "job_id", run.JobID)
			return queue.Result{Outcome: queue.Hold, Reason: ReasonUnknownType, Detail: "format_missing"}
		case errors.Is(err, errFormat):
			d.Log.Error("render: format invalid", "job_id", run.JobID)
			return configError("format_invalid")
		case errors.Is(err, errStored):
			d.Log.Error("render: stored output unreadable", "job_id", run.JobID)
			return internalError("stored_output_unreadable")
		case errors.Is(err, meeting.ErrVersion):
			d.Log.Error("render: unsupported normalized meeting version", "job_id", run.JobID)
			return internalError("meeting_version")
		case err != nil:
			code := errorCode(err)
			d.Log.Warn("render: cannot load the job", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
		}
		doc, err := core.Render(j.in)
		if errors.Is(err, core.ErrStrings) {
			d.Log.Error("render: no complete string table", "job_id", run.JobID, "language", j.in.Language)
			return configError("strings_missing")
		}
		if err != nil {
			d.Log.Error("render: bad transcript time base", "job_id", run.JobID)
			return internalError("time_base")
		}
		limit := int64(DefaultMaxDocumentBytes)
		if j.sinkProfile != nil && d.MaxDocumentBytes[*j.sinkProfile] > 0 {
			limit = d.MaxDocumentBytes[*j.sinkProfile]
		}
		if int64(len(doc.SummaryMD)) > limit {
			d.Log.Info("render: document over the sink's limit", "job_id", run.JobID, "bytes", len(doc.SummaryMD),
				"max_document_bytes", limit)
			return queue.Result{Outcome: queue.Permanent, Reason: ReasonRejectedBySink, Detail: "document_too_large",
				ErrorClass: "rejected", ErrorCode: "document_too_large"}
		}
		f := j.in.Format
		out := Output{FormatID: f.ID, FormatVersion: f.Version, FormatSHA256: f.SHA256, Language: j.in.Language}
		for _, b := range []struct {
			key  *string
			data []byte
		}{
			{&out.SummarySHA256, doc.SummaryMD},
			{&out.TranscriptSHA256, doc.TranscriptVTT},
			{&out.TitleSHA256, []byte(doc.Title)},
		} {
			sum := sha256.Sum256(b.data)
			*b.key = hex.EncodeToString(sum[:])
			if err := d.Blobs.Put(ctx, run.JobID, *b.key, int64(len(b.data)), bytes.NewReader(b.data)); err != nil {
				return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: errorCode(err)}
			}
		}
		d.Log.Info("render: done", "job_id", run.JobID, "language", out.Language, "format_id", out.FormatID,
			"summary_bytes", len(doc.SummaryMD), "transcript_bytes", len(doc.TranscriptVTT))
		b, err := json.Marshal(out)
		if err != nil {
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: "render"}
		}
		return queue.Result{Outcome: queue.OK, Output: b}
	}
}

func configError(code string) queue.Result {
	return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError, ErrorClass: string(providers.PermanentConfig),
		ErrorCode: code}
}

// internalError is the failure for an earlier step's stored output that
// cannot be used (docs/reference/README.md "Job failure reasons").
func internalError(code string) queue.Result {
	return queue.Result{Outcome: queue.Permanent, Reason: ReasonInternalError, ErrorClass: errorClassInternal,
		ErrorCode: code}
}

// errStored means a stored step output or blob of an earlier step does
// not decode: a bug or corruption, not worth a retry.
var errStored = errors.New("render: stored output unreadable")

// errFormat means the format exists but cannot be loaded.
var errFormat = errors.New("render: invalid format")

// load reads the job, the step outputs it builds on and their blobs. The
// format and language are the ones summarize recorded, so both steps use
// the same format.
func (d Deps) load(ctx context.Context, jobID string) (*job, error) {
	j := &job{}
	var nout meeting.Output
	var aout assemble.Output
	if err := d.Pool.QueryRow(ctx, `
		SELECT j.sink_profile, j.created_at, n.output, a.output, s.output FROM jobs j
		JOIN steps n ON n.job_id = j.job_id AND n.name = 'normalize'
		JOIN steps a ON a.job_id = j.job_id AND a.name = 'assemble'
		JOIN steps s ON s.job_id = j.job_id AND s.name = 'summarize'
		WHERE j.job_id = $1`, jobID).Scan(&j.sinkProfile, &j.createdAt, &nout, &aout, &j.summarize); err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	f, err := format.Load(d.FormatsDir, j.summarize.FormatID)
	if errors.Is(err, format.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, errFormat
	}
	data, err := d.read(ctx, jobID, nout.NormalizedSHA256)
	if err != nil {
		return nil, err
	}
	m, err := meeting.Decode(data)
	if errors.Is(err, meeting.ErrVersion) {
		return nil, err
	}
	if err != nil {
		return nil, errStored
	}
	var tr assemble.Transcript
	if err := d.readJSON(ctx, jobID, aout.TranscriptSHA256, &tr); err != nil {
		return nil, err
	}
	j.in = core.Input{MeetingName: m.MeetingName, Transcript: &tr, Polls: m.Polls, LLMModel: j.summarize.Model,
		GeneratedAt: j.createdAt, Format: f, Language: j.summarize.Language, Zone: d.Zone}
	if !j.summarize.NoSpeech {
		var s summarize.Summary
		if err := d.readJSON(ctx, jobID, j.summarize.SummarySHA256, &s); err != nil {
			return nil, err
		}
		j.in.Summary = &s
	}
	return j, nil
}

// read reads a blob of the job.
func (d Deps) read(ctx context.Context, jobID, key string) ([]byte, error) {
	rc, err := d.Blobs.Get(ctx, jobID, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (d Deps) readJSON(ctx context.Context, jobID, key string, v any) error {
	b, err := d.read(ctx, jobID, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errStored
	}
	return nil
}

// errorCode reduces an internal error to a code for the log and the step
// row (no paths or content).
func errorCode(err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no_summarize_output"
	case errors.Is(err, blob.ErrNotFound):
		return "blob_not_found"
	case errors.Is(err, blob.ErrHashMismatch):
		return "blob_hash_mismatch"
	case errors.Is(err, blob.ErrTransient):
		return "blob_transient"
	}
	return "render"
}
