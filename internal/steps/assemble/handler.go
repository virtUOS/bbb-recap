package assemble

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
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/transcribe"
)

// Step is the step name.
const Step = "assemble"

// Deps are what the assemble handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	Log   *slog.Logger
}

// Output is the step output stored in steps.output: the SHA-256 of the
// transcript blob in the job's prefix.
type Output struct {
	TranscriptSHA256 string `json:"transcript_sha256"`
}

// errIncomplete means transcribe left a track without an outcome.
var errIncomplete = errors.New("transcribe outcome missing for a track")

// Handler returns the assemble step handler. It reads the normalized meeting
// (normalize's steps.output) and the transcribe step_items, builds the
// transcript and stores it as a blob under its own hash, so a repeat gives
// the same key (architecture.md §6.4).
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		out, err := d.run(ctx, run.JobID)
		if err != nil {
			code := errorCode(err)
			d.Log.Warn("assemble: step error", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: code}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "assemble"}
		}
		return queue.Result{Outcome: queue.OK, Output: b}
	}
}

func (d Deps) run(ctx context.Context, jobID string) (Output, error) {
	in, err := d.load(ctx, jobID)
	if err != nil {
		return Output{}, err
	}
	data, err := Assemble(in)
	if err != nil {
		return Output{}, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	if err := d.Blobs.Put(ctx, jobID, key, int64(len(data)), bytes.NewReader(data)); err != nil {
		return Output{}, err
	}
	gaps := 0
	for _, a := range in.ASR {
		if a.Gap != "" {
			gaps++
		}
	}
	d.Log.Debug("assemble: done", "job_id", jobID, "tracks", len(in.Meeting.Tracks), "asr_gaps", gaps,
		"bytes", len(data))
	return Output{TranscriptSHA256: key}, nil
}

// load reads the job's created_at, the normalized meeting and one ASR
// outcome per track that normalize could read.
func (d Deps) load(ctx context.Context, jobID string) (Input, error) {
	var createdAt time.Time
	var nout meeting.Output
	if err := d.Pool.QueryRow(ctx, `
		SELECT j.created_at, s.output FROM jobs j JOIN steps s ON s.job_id = j.job_id AND s.name = 'normalize'
		WHERE j.job_id = $1`, jobID).Scan(&createdAt, &nout); err != nil {
		return Input{}, fmt.Errorf("normalize output: %w", err)
	}
	data, err := d.read(ctx, jobID, nout.NormalizedSHA256)
	if err != nil {
		return Input{}, err
	}
	m, err := meeting.Decode(data)
	if err != nil {
		return Input{}, err
	}

	type row struct {
		state  string
		reason *string
		out    transcribe.ItemOutput
	}
	items := map[string]row{}
	rows, err := d.Pool.Query(ctx, `SELECT item, state, gap_reason, output FROM step_items WHERE job_id = $1`, jobID)
	if err != nil {
		return Input{}, err
	}
	var name string
	var r row
	if _, err := pgx.ForEachRow(rows, []any{&name, &r.state, &r.reason, &r.out}, func() error {
		items[name] = r
		r = row{}
		return nil
	}); err != nil {
		return Input{}, err
	}

	in := Input{JobID: jobID, CreatedAt: createdAt, Meeting: m}
	for i, t := range m.Tracks {
		if t.Gap != "" {
			continue // normalize gap: no step_items row
		}
		it, ok := items[transcribe.TrackRef(i)]
		switch {
		case ok && it.state == "gap" && it.reason != nil:
			in.ASR = append(in.ASR, TrackASR{Path: t.Path, Gap: *it.reason})
		case ok && it.state == "succeeded":
			var res providers.AsrResult
			if err := d.readJSON(ctx, jobID, it.out.ResultSHA256, &res); err != nil {
				return Input{}, err
			}
			in.ASR = append(in.ASR, TrackASR{Path: t.Path, Profile: it.out.Profile, Dialect: it.out.Dialect,
				Result: &res})
		default:
			return Input{}, errIncomplete
		}
	}
	return in, nil
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

// readJSON reads a JSON blob of the job into v.
func (d Deps) readJSON(ctx context.Context, jobID, key string, v any) error {
	data, err := d.read(ctx, jobID, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// errorCode reduces an error to a code for the log and the step row (no
// paths or content).
func errorCode(err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no_normalize_output"
	case errors.Is(err, errIncomplete):
		return "transcribe_incomplete"
	case errors.Is(err, meeting.ErrVersion):
		return "meeting_version"
	case errors.Is(err, blob.ErrNotFound):
		return "blob_not_found"
	case errors.Is(err, blob.ErrHashMismatch):
		return "blob_hash_mismatch"
	case errors.Is(err, blob.ErrTransient):
		return "blob_transient"
	}
	return "assemble"
}
