package normalize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/queue"
	"github.com/virtUOS/bbb-recap/internal/steps/validate"
)

// Deps are what the normalize handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	Log   *slog.Logger
}

// Handler returns the normalize step handler. It stores the normalized
// meeting as a blob under its own hash; a repeat writes the same bytes to
// the same key (architecture.md §6.4).
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		out, err := d.run(ctx, run.JobID)
		if errors.Is(err, ErrEvents) {
			// validate rejects such packages (V-S01); seeing one here means
			// the package changed or validate was skipped.
			d.Log.Warn("normalize: unusable events.xml", "job_id", run.JobID)
			return queue.Result{Outcome: queue.Permanent, Reason: validate.ReasonEventsMismatch, Detail: "normalize"}
		}
		if err != nil {
			d.Log.Warn("normalize: step error", "job_id", run.JobID, "err", err)
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "normalize"}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "normalize"}
		}
		return queue.Result{Outcome: queue.OK, Output: b}
	}
}

func (d Deps) run(ctx context.Context, jobID string) (meeting.Output, error) {
	j, err := validate.LoadJob(ctx, d.Pool, jobID)
	if err != nil {
		return meeting.Output{}, err
	}
	m, err := Normalize(j.FS(ctx, d.Blobs), j.Files)
	if err != nil {
		return meeting.Output{}, err
	}
	data, err := meeting.Marshal(m)
	if err != nil {
		return meeting.Output{}, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	if err := d.Blobs.Put(ctx, jobID, key, int64(len(data)), bytes.NewReader(data)); err != nil {
		return meeting.Output{}, err
	}
	gaps := 0
	for _, t := range m.Tracks {
		if t.Gap != "" {
			gaps++
		}
	}
	d.Log.Debug("normalize: done", "job_id", jobID, "tracks", len(m.Tracks), "gaps", gaps,
		"periods", len(m.RecordingPeriods), "bytes", len(data))
	return meeting.Output{NormalizedSHA256: key}, nil
}
