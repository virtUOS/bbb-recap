package summarize

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/htmltext"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/assemble"
)

// Failure and held reasons of the step (docs/reference/README.md).
const (
	ReasonConfigError        = "config_error"
	ReasonRejectedByProvider = "rejected_by_provider"
	ReasonUnknownType        = "unknown_type"
)

const errorClassInternal = "internal"

// Deps are what the summarize handler needs.
type Deps struct {
	Pool    *pgxpool.Pool
	Blobs   blob.Store
	Log     *slog.Logger
	LLM     providers.LLM
	Profile *providers.Profile // the LLM profile the adapter was built from
	// FormatsDir is RECAP_FORMATS_DIR; MeetingTypes maps a meeting type to
	// its format ID (RECAP_MEETING_TYPES_FILE).
	FormatsDir      string
	MeetingTypes    map[string]string
	DefaultLanguage string // RECAP_DEFAULT_LANGUAGE
}

// Output is the step output stored in steps.output: the blob key of the
// summary JSON and provenance, no content. Without speech SummarySHA256 is
// empty and NoSpeech is true.
type Output struct {
	SummarySHA256 string `json:"summary_sha256,omitempty"`
	NoSpeech      bool   `json:"no_speech,omitempty"`
	Profile       string `json:"profile"`
	Dialect       string `json:"dialect"`
	Model         string `json:"model,omitempty"` // reported by the provider
	Language      string `json:"language"`
	FormatID      string `json:"format_id"`
	FormatVersion string `json:"format_version"`
	FormatSHA256  string `json:"format_sha256"`
}

// Handler returns the summarize step handler. LLM calls to the profile are
// limited to its max_inflight by a semaphore shared by all jobs of this
// process (architecture.md §7). The summary is stored under its own hash,
// so a repeat with the same replies gives the same key (§6.4).
func Handler(d Deps) queue.Handler {
	sem := make(chan struct{}, max(d.Profile.MaxInflight, 1))
	return func(ctx context.Context, run queue.Run) queue.Result {
		f, in, err := d.load(ctx, run.JobID)
		switch {
		case errors.Is(err, format.ErrNotFound):
			d.Log.Warn("summarize: format not found", "job_id", run.JobID)
			return queue.Result{Outcome: queue.Hold, Reason: ReasonUnknownType, Detail: "format_missing"}
		case errors.Is(err, errFormat):
			d.Log.Error("summarize: format invalid", "job_id", run.JobID)
			return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
				ErrorClass: string(providers.PermanentConfig), ErrorCode: "format_invalid"}
		case err != nil:
			code := errorCode(err)
			d.Log.Warn("summarize: cannot load the job", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: code}
		}
		lang := Language(f, in.Transcript.Language, d.DefaultLanguage)
		s := &Summarizer{LLM: d.LLM, Profile: d.Profile, Format: f, Sem: sem, Log: d.Log}
		res, err := s.Run(ctx, run.JobID, in, lang)
		if err != nil {
			r := Outcome(err)
			d.Log.Info("summarize: failed", "job_id", run.JobID, "outcome", r.Outcome, "reason", r.Reason,
				"error_class", r.ErrorClass, "error_code", r.ErrorCode)
			return r
		}
		out := Output{NoSpeech: res.NoSpeech, Profile: d.Profile.Name, Dialect: d.Profile.Dialect,
			Model: res.Model, Language: lang, FormatID: f.ID, FormatVersion: f.Version, FormatSHA256: f.SHA256}
		if !res.NoSpeech {
			sum := sha256.Sum256(res.Summary)
			out.SummarySHA256 = hex.EncodeToString(sum[:])
			if err := d.Blobs.Put(ctx, run.JobID, out.SummarySHA256, int64(len(res.Summary)),
				bytes.NewReader(res.Summary)); err != nil {
				return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: errorCode(err)}
			}
		}
		d.Log.Info("summarize: done", "job_id", run.JobID, "calls", res.Calls, "no_speech", res.NoSpeech,
			"language", lang, "bytes", len(res.Summary))
		b, err := json.Marshal(out)
		if err != nil {
			return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: "summarize"}
		}
		return queue.Result{Outcome: queue.OK, Output: b}
	}
}

// Outcome maps a Run error to a step result (README "Adapter error classes
// → step outcome", LLM rows).
func Outcome(err error) queue.Result {
	var rej *Rejected
	if errors.As(err, &rej) {
		class := string(rej.Class)
		if class == "" {
			class = "reduce"
		}
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonRejectedByProvider, ErrorClass: class,
			ErrorCode: rej.Code}
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return queue.Result{Outcome: queue.Transient, ErrorClass: errorClassInternal, ErrorCode: "summarize"}
	}
	switch pe.Class {
	case providers.PermanentConfig:
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError, ErrorClass: string(pe.Class),
			ErrorCode: pe.Code}
	case providers.OutputTruncated, providers.InputTooLarge:
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonRejectedByProvider, ErrorClass: string(pe.Class),
			ErrorCode: pe.Code}
	}
	// retryable and invalid_response: transient, bounded by the step's
	// max attempts (architecture.md §5.3).
	return queue.Result{Outcome: queue.Transient, ErrorClass: string(pe.Class), ErrorCode: pe.Code,
		RetryAfter: pe.RetryAfter}
}

// errFormat means the format exists but cannot be loaded.
var errFormat = errors.New("summarize: invalid format")

// load reads the job's meeting type and format, the normalized meeting, the
// transcript and the shared notes, and builds the Input.
func (d Deps) load(ctx context.Context, jobID string) (*format.Format, Input, error) {
	var meetingType *string
	var nout meeting.Output
	var aout assemble.Output
	if err := d.Pool.QueryRow(ctx, `
		SELECT j.meeting_type, n.output, a.output FROM jobs j
		JOIN steps n ON n.job_id = j.job_id AND n.name = 'normalize'
		JOIN steps a ON a.job_id = j.job_id AND a.name = 'assemble'
		WHERE j.job_id = $1`, jobID).Scan(&meetingType, &nout, &aout); err != nil {
		return nil, Input{}, fmt.Errorf("job: %w", err)
	}
	id, ok := "", false
	if meetingType != nil {
		id, ok = d.MeetingTypes[*meetingType]
	}
	if !ok {
		return nil, Input{}, format.ErrNotFound
	}
	f, err := format.Load(d.FormatsDir, id)
	if errors.Is(err, format.ErrNotFound) {
		return nil, Input{}, err
	}
	if err != nil {
		return nil, Input{}, errFormat
	}

	var m meeting.Meeting
	if err := d.readJSON(ctx, jobID, nout.NormalizedSHA256, &m); err != nil {
		return nil, Input{}, err
	}
	var tr assemble.Transcript
	if err := d.readJSON(ctx, jobID, aout.TranscriptSHA256, &tr); err != nil {
		return nil, Input{}, err
	}
	in := Input{MeetingName: m.MeetingName, Transcript: &tr, Polls: m.Polls}
	names := map[string]string{}
	for _, p := range m.Participants {
		names[p.Ref] = p.DisplayName
	}
	for _, c := range m.Chat {
		in.Chat = append(in.Chat, ChatMessage{AtMS: c.TMS, Name: names[c.Participant], Text: c.Text})
	}
	if m.Notes != nil {
		b, err := d.read(ctx, jobID, m.Notes.SHA256)
		if err != nil {
			return nil, Input{}, err
		}
		in.Notes = plainNotes(m.Notes.MediaType, b)
	}
	return f, in, nil
}

// plainNotes returns the shared notes as text: HTML is converted, any other
// media type (text/markdown) is taken as it is.
func plainNotes(mediaType string, b []byte) string {
	if mediaType == meeting.MediaTypeHTML {
		return htmltext.Text(string(b))
	}
	return string(b)
}

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
	return json.Unmarshal(b, v)
}

// errorCode reduces an internal error to a code for the log and the step
// row (no paths or content).
func errorCode(err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no_assemble_output"
	case errors.Is(err, blob.ErrNotFound):
		return "blob_not_found"
	case errors.Is(err, blob.ErrHashMismatch):
		return "blob_hash_mismatch"
	case errors.Is(err, blob.ErrTransient):
		return "blob_transient"
	}
	return "summarize"
}
