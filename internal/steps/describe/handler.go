package describe

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
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/format"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// ReasonConfigError is the failure reason of permanent_config
// (docs/reference/README.md).
const ReasonConfigError = "config_error"

// itemPrefix starts the step_items item of an image: img-<64 hex>
// (architecture.md §6.1), longer than any profile name or track ref.
const itemPrefix = "img-"

// stateSucceeded is the step_items state of a done image.
const stateSucceeded = "succeeded"

// Deps are what the describe handler needs.
type Deps struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	Log   *slog.Logger
	// LLM and Profile are the vision profile (defaults.vision); nil when
	// no meeting type switches slide images on.
	LLM     providers.LLM
	Profile *providers.Profile
	// Sem limits calls to Profile (shared with summarize for one profile).
	Sem chan struct{}
	// FormatsDir is RECAP_FORMATS_DIR; MeetingTypes maps a meeting type to
	// its format ID; SlideImageTypes are the types with slide_images: true.
	FormatsDir      string
	MeetingTypes    map[string]string
	SlideImageTypes map[string]bool
}

// Output is the step output stored in steps.output: the blob key of the
// descriptions file (absent when there is none) and counts.
type Output struct {
	DescriptionsSHA256 string `json:"descriptions_sha256,omitempty"`
	Images             int    `json:"images"`
	Described          int    `json:"described"`
	Fallbacks          int    `json:"fallbacks"`
}

// Descriptions is the descriptions file: image ref → description. Images
// without a description are absent (templates.md §3.2).
type Descriptions struct {
	Version      int               `json:"version"`
	Descriptions map[string]string `json:"descriptions"`
}

// DescriptionsVersion is the version of the descriptions file.
const DescriptionsVersion = 1

// ItemOutput is step_items.output of an image: the key of the description
// blob ({"description": …}), or the fallback reason.
type ItemOutput struct {
	DescriptionSHA256 string `json:"description_sha256,omitempty"`
	Fallback          string `json:"fallback,omitempty"`
}

// descriptionBlob is the job blob of one description.
type descriptionBlob struct {
	Description string `json:"description"`
}

// Handler returns the describe step handler.
func Handler(d Deps) queue.Handler {
	return func(ctx context.Context, run queue.Run) queue.Result {
		res, err := d.run(ctx, run)
		if err != nil {
			code := errorCode(err)
			d.Log.Warn("describe: step error", "job_id", run.JobID, "err", code)
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: code}
		}
		return res
	}
}

func (d Deps) run(ctx context.Context, run queue.Run) (queue.Result, error) {
	var meetingType *string
	var nout meeting.Output
	if err := d.Pool.QueryRow(ctx, `
		SELECT j.meeting_type, n.output FROM jobs j
		JOIN steps n ON n.job_id = j.job_id AND n.name = 'normalize'
		WHERE j.job_id = $1`, run.JobID).Scan(&meetingType, &nout); err != nil {
		return queue.Result{}, fmt.Errorf("job: %w", err)
	}
	if meetingType == nil || !d.SlideImageTypes[*meetingType] {
		return ok(Output{})
	}
	f, err := format.Load(d.FormatsDir, d.MeetingTypes[*meetingType])
	if errors.Is(err, format.ErrNotFound) {
		// summarize holds the job unknown_type; nothing to describe here.
		return ok(Output{})
	}
	if err != nil {
		d.Log.Error("describe: format invalid", "job_id", run.JobID)
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
			ErrorClass: string(providers.PermanentConfig), ErrorCode: "format_invalid"}, nil
	}
	if !f.HasInput(format.InputSlideImages) {
		return ok(Output{})
	}
	if d.LLM == nil || d.Profile == nil {
		d.Log.Error("describe: no vision profile (defaults.vision)", "job_id", run.JobID)
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
			ErrorClass: string(providers.PermanentConfig), ErrorCode: "no_vision_profile"}, nil
	}

	data, err := d.read(ctx, run.JobID, nout.NormalizedSHA256)
	if err != nil {
		return queue.Result{}, err
	}
	m, err := meeting.Decode(data)
	if err != nil {
		return queue.Result{}, err
	}
	images, pos := distinct(m.Slides, func(key string) func(context.Context) ([]byte, error) {
		return func(ctx context.Context) ([]byte, error) { return d.read(ctx, run.JobID, key) }
	})
	if len(images) == 0 {
		return ok(Output{})
	}

	done, err := d.items(ctx, run.JobID)
	if err != nil {
		return queue.Result{}, err
	}
	var todo []Image
	for _, img := range images {
		if _, ok := done[img.Ref]; !ok {
			todo = append(todo, img)
		}
	}
	desc := &Describer{LLM: d.LLM, Profile: d.Profile, Format: f, Sem: d.Sem, Log: d.Log,
		OnFallback: func(reason string) { metrics.SlideImageFallback(Step, reason) }}
	last := run.Attempt >= queue.MaxAttempts(Step)
	results, err := desc.Run(ctx, run.JobID, todo, pos, last)
	if errors.Is(err, ErrFormat) {
		d.Log.Error("describe: format invalid", "job_id", run.JobID)
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
			ErrorClass: string(providers.PermanentConfig), ErrorCode: "format_invalid"}, nil
	}
	// Results of this run become rows, also when the step does not end ok:
	// a retry skips them.
	rows := map[string]ItemOutput{}
	for ref, r := range results {
		out := ItemOutput{Fallback: r.Fallback}
		if r.Description != "" {
			key, perr := d.put(ctx, run.JobID, descriptionBlob{Description: r.Description})
			if perr != nil {
				return queue.Result{}, perr
			}
			out = ItemOutput{DescriptionSHA256: key}
		}
		rows[ref] = out
		done[ref] = out
	}
	commit := func(ctx context.Context, tx pgx.Tx) error { return writeItems(ctx, tx, run.JobID, rows) }
	var stop *Stop
	if errors.As(err, &stop) {
		d.Log.Info("describe: stopped", "job_id", run.JobID, "images", len(images), "done", len(done),
			"permanent", stop.Permanent, "error_class", string(stop.Err.Class), "error_code", stop.Err.Code)
		if stop.Permanent {
			return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
				ErrorClass: string(stop.Err.Class), ErrorCode: stop.Err.Code, Commit: commit}, nil
		}
		return queue.Result{Outcome: queue.Transient, ErrorClass: string(stop.Err.Class),
			ErrorCode: stop.Err.Code, RetryAfter: stop.Err.RetryAfter, Commit: commit}, nil
	}
	if err != nil {
		return queue.Result{}, err
	}

	// Every image has a row now: write the descriptions file.
	out := Output{Images: len(images)}
	file := Descriptions{Version: DescriptionsVersion, Descriptions: map[string]string{}}
	for _, img := range images {
		it := done[img.Ref]
		if it.DescriptionSHA256 == "" {
			out.Fallbacks++
			continue
		}
		var b descriptionBlob
		raw, err := d.read(ctx, run.JobID, it.DescriptionSHA256)
		if err != nil {
			return queue.Result{}, err
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return queue.Result{}, err
		}
		file.Descriptions[img.Ref] = b.Description
		out.Described++
	}
	if out.DescriptionsSHA256, err = d.put(ctx, run.JobID, file); err != nil {
		return queue.Result{}, err
	}
	d.Log.Info("describe: done", "job_id", run.JobID, "images", out.Images, "described", out.Described,
		"fallbacks", out.Fallbacks)
	res, _ := ok(out)
	res.Commit = commit
	return res, nil
}

func ok(out Output) (queue.Result, error) {
	b, err := json.Marshal(out)
	if err != nil {
		return queue.Result{}, err
	}
	return queue.Result{Outcome: queue.OK, Output: b}, nil
}

// distinct returns the distinct images of the slides in order of first
// appearance, with their 1-based positions; load makes the reader of a
// blob key.
func distinct(slides []meeting.Slide, load func(key string) func(context.Context) ([]byte, error)) ([]Image,
	map[string]int) {
	var images []Image
	pos := map[string]int{}
	for _, s := range slides {
		key, ok := strings.CutPrefix(s.Image, meeting.ImagePrefix)
		if !ok || key == "" {
			continue
		}
		if _, seen := pos[s.Image]; seen {
			continue
		}
		images = append(images, Image{Ref: s.Image, Load: load(key)})
		pos[s.Image] = len(images)
	}
	return images, pos
}

// items reads the rows of this step: image ref → output.
func (d Deps) items(ctx context.Context, jobID string) (map[string]ItemOutput, error) {
	rows, err := d.Pool.Query(ctx, `SELECT item, output FROM step_items
		WHERE job_id = $1 AND item LIKE 'img-%' AND state = 'succeeded'`, jobID)
	if err != nil {
		return nil, err
	}
	done := map[string]ItemOutput{}
	var item string
	var out ItemOutput
	_, err = pgx.ForEachRow(rows, []any{&item, &out}, func() error {
		done[meeting.ImagePrefix+strings.TrimPrefix(item, itemPrefix)] = out
		out = ItemOutput{}
		return nil
	})
	return done, err
}

// writeItems upserts the rows of the images done in this run.
func writeItems(ctx context.Context, tx pgx.Tx, jobID string, rows map[string]ItemOutput) error {
	for ref, out := range rows {
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		item := itemPrefix + strings.TrimPrefix(ref, meeting.ImagePrefix)
		if _, err := tx.Exec(ctx, `
			INSERT INTO step_items (job_id, item, state, attempt, output) VALUES ($1, $2, $3, 1, $4)
			ON CONFLICT (job_id, item) DO UPDATE SET state = EXCLUDED.state, output = EXCLUDED.output`,
			jobID, item, stateSucceeded, b); err != nil {
			return fmt.Errorf("step_items %s: %w", item, err)
		}
	}
	return nil
}

func (d Deps) read(ctx context.Context, jobID, key string) ([]byte, error) {
	rc, err := d.Blobs.Get(ctx, jobID, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// put stores v as JSON under its own hash.
func (d Deps) put(ctx context.Context, jobID string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	return key, d.Blobs.Put(ctx, jobID, key, int64(len(b)), bytes.NewReader(b))
}

// errorCode reduces an internal error to a code (no paths or content).
func errorCode(err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no_normalize_output"
	case errors.Is(err, blob.ErrNotFound):
		return "blob_not_found"
	case errors.Is(err, blob.ErrHashMismatch):
		return "blob_hash_mismatch"
	case errors.Is(err, blob.ErrTransient):
		return "blob_transient"
	case errors.Is(err, meeting.ErrVersion):
		return "meeting_version"
	}
	return "describe"
}
