// Package transcribe implements the transcribe step (docs/reference/architecture.md
// §5.2, §6.4; providers.md §1.6): one ASR call per track of the
// normalized meeting, with per-track progress in step_items.
//
// # Tracks and refs
//
// Track refs are trk-<n>, numbered from 1 in the order of the normalized
// meeting's tracks array (TrackRef). A track that normalize marked as a gap
// (track_unreadable) is not sent and gets no step_items row.
//
// # step_items (one row per sent track, item = track ref)
//
//	state       pending (not done yet), succeeded or gap
//	attempt     how many requests were sent for the track
//	gap_reason  asr_failed, asr_rejected_input or asr_invalid_response
//	output      ItemOutput: for succeeded the SHA-256 of the AsrResult blob
//	            (JSON of providers.AsrResult in the job's prefix) and the
//	            provenance; else the last error class and code
//
// A retried step sends only tracks whose row is not succeeded or gap. On
// the step's last attempt a track that still fails becomes a gap, so the
// step never ends in retries_exhausted because of one track.
package transcribe

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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/meeting"
	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/providers"
	"github.com/virtUOS/bbb-recap/internal/queue"
)

// Step is the step name.
const Step = "transcribe"

// Gap reasons (docs/reference/README.md "Adapter error classes → step outcome").
const (
	GapASRFailed          = "asr_failed"
	GapASRRejectedInput   = "asr_rejected_input"
	GapASRInvalidResponse = "asr_invalid_response"
)

// Failure reasons of the step.
const (
	ReasonConfigError     = "config_error"
	ReasonAllTracksFailed = "all_tracks_failed"
)

// step_items states.
const (
	statePending   = "pending"
	stateSucceeded = "succeeded"
	stateGap       = "gap"
)

// TrackRef returns the ref of the i-th track (0-based) of the normalized
// meeting: trk-<i+1>.
func TrackRef(i int) string { return fmt.Sprintf("trk-%d", i+1) }

// Deps are what the transcribe handler needs.
type Deps struct {
	Pool     *pgxpool.Pool
	Blobs    blob.Store
	Log      *slog.Logger
	ASR      providers.ASR
	Profile  *providers.Profile // the ASR profile the adapter was built from
	Language string             // RECAP_ASR_LANGUAGE; empty: auto-detect
}

// Output is the step output stored in steps.output: counts only.
type Output struct {
	Tracks      int `json:"tracks"`
	Transcribed int `json:"transcribed"`
	Gaps        int `json:"gaps"`
}

// ItemOutput is step_items.output.
type ItemOutput struct {
	ResultSHA256 string `json:"result_sha256,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Dialect      string `json:"dialect,omitempty"`
	Model        string `json:"model,omitempty"`
	ErrorClass   string `json:"error_class,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
}

// item is one track of this run.
type item struct {
	ref       string
	track     meeting.Track
	attempt   int    // requests sent so far, this run's included once sent
	state     string // statePending, stateSucceeded or stateGap
	gapReason string
	out       ItemOutput
	err       *providers.Error // last error, nil on success
	sent      bool             // a row must be written
}

// Handler returns the transcribe step handler. Requests to the profile are
// limited to its max_inflight by a semaphore shared by all jobs of this
// process (architecture.md §7).
func Handler(d Deps) queue.Handler {
	sem := make(chan struct{}, max(d.Profile.MaxInflight, 1))
	metrics.InitExternal(d.Profile.Name, metrics.KindASR, requestOutcomes...)
	return func(ctx context.Context, run queue.Run) queue.Result {
		items, gaps, err := d.load(ctx, run.JobID)
		if err != nil {
			d.Log.Warn("transcribe: cannot load the job", "job_id", run.JobID, "err", errorCode(err))
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "transcribe_load"}
		}
		last := run.Attempt >= queue.MaxAttempts(Step)
		var wg sync.WaitGroup
		for _, it := range items {
			if it.state != statePending {
				continue // done in an earlier attempt
			}
			wg.Go(func() { d.transcribe(ctx, run, it, sem, last) })
		}
		wg.Wait()
		if ctx.Err() != nil {
			// Shutdown or lost lease: the engine drops this result, and a
			// clean shutdown gives the attempt back (architecture.md §9).
			// Never turn interrupted tracks into gaps.
			return queue.Result{Outcome: queue.Transient, ErrorClass: "internal", ErrorCode: "cancelled"}
		}
		return d.decide(run, items, gaps)
	}
}

// load reads the normalized meeting and the job's step_items. It returns
// one item per track normalize could read, and the number of tracks
// normalize already made gaps.
func (d Deps) load(ctx context.Context, jobID string) ([]*item, int, error) {
	var out meeting.Output
	if err := d.Pool.QueryRow(ctx, `SELECT output FROM steps WHERE job_id = $1 AND name = 'normalize'`,
		jobID).Scan(&out); err != nil {
		return nil, 0, fmt.Errorf("normalize output: %w", err)
	}
	rc, err := d.Blobs.Get(ctx, jobID, out.NormalizedSHA256)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, 0, err
	}
	var m meeting.Meeting
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, 0, fmt.Errorf("normalized meeting: %w", err)
	}

	type row struct {
		state   string
		attempt int
	}
	done := map[string]row{}
	rows, err := d.Pool.Query(ctx, `SELECT item, state, attempt FROM step_items WHERE job_id = $1`, jobID)
	if err != nil {
		return nil, 0, err
	}
	var name string
	var r row
	if _, err := pgx.ForEachRow(rows, []any{&name, &r.state, &r.attempt}, func() error {
		done[name] = r
		return nil
	}); err != nil {
		return nil, 0, err
	}

	var items []*item
	gaps := 0
	for i, t := range m.Tracks {
		if t.Gap != "" {
			gaps++
			continue
		}
		it := &item{ref: TrackRef(i), track: t, state: statePending}
		if r, ok := done[it.ref]; ok {
			it.state, it.attempt = r.state, r.attempt
		}
		items = append(items, it)
	}
	return items, gaps, nil
}

// transcribe sends one track and records the outcome in it.
func (d Deps) transcribe(ctx context.Context, run queue.Run, it *item, sem chan struct{}, last bool) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
	}
	if ctx.Err() != nil { // select picks at random when both are ready
		it.err = &providers.Error{Class: providers.Retryable, Code: "cancelled"}
		return
	}
	it.sent = true
	it.attempt++
	duration := it.track.EndMS - it.track.StartMS
	if it.track.DurationMS != nil {
		duration = *it.track.DurationMS
	}
	req := providers.AsrRequest{
		JobID:          run.JobID,
		TrackRef:       it.ref,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", run.JobID, it.ref, run.Attempt),
		Audio: providers.Audio{
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				return d.Blobs.Get(ctx, run.JobID, it.track.SHA256)
			},
			Size:      it.track.Size,
			SHA256:    it.track.SHA256,
			MediaType: it.track.MediaType,
		},
		DurationMS:   duration,
		LanguageHint: d.Language,
	}
	h, err := d.ASR.Submit(ctx, req)
	var res *providers.AsrResult
	if err == nil {
		res, err = d.ASR.Result(ctx, h)
	}
	metrics.ExternalRequest(d.Profile.Name, metrics.KindASR, requestOutcome(err))
	cerr := d.ASR.Cleanup(ctx, h)
	if d.ASR.Capabilities().RemoteRetention == retentionUntilCleanup {
		// Only such a dialect sends a cleanup request (none for
		// whisperx-family, providers.md §2.4).
		metrics.ExternalRequest(d.Profile.Name, metrics.KindASRCleanup, requestOutcome(cerr))
	}
	if cerr != nil {
		d.Log.Warn("transcribe: asr cleanup failed", "job_id", run.JobID, "track_ref", it.ref,
			"profile", d.Profile.Name)
	}
	if err == nil {
		err = d.store(ctx, run.JobID, it, res)
	}
	if err != nil {
		d.fail(it, err, last && ctx.Err() == nil) // an interrupted request is no failed attempt
	}
	d.Log.Debug("transcribe: track", "job_id", run.JobID, "track_ref", it.ref, "attempt", it.attempt,
		"state", it.state, "gap_reason", it.gapReason, "error_class", it.out.ErrorClass,
		"error_code", it.out.ErrorCode)
}

// retentionUntilCleanup is the Capabilities.RemoteRetention of a provider
// that keeps the request until the adapter's cleanup call (providers.md §1).
const retentionUntilCleanup = "until_cleanup"

// requestOutcomes are the outcome values of an ASR request: ok or the
// adapter error class.
var requestOutcomes = []string{metrics.OK, string(providers.Retryable), string(providers.PermanentInput),
	string(providers.PermanentConfig), string(providers.InvalidResponse)}

// requestOutcome is the metrics outcome of an adapter call: ok or the error
// class (an error that is no *providers.Error counts as retryable, as in
// fail).
func requestOutcome(err error) string {
	if err == nil {
		return metrics.OK
	}
	var e *providers.Error
	if errors.As(err, &e) {
		return string(e.Class)
	}
	return string(providers.Retryable)
}

// store saves the result as a blob under its own hash.
func (d Deps) store(ctx context.Context, jobID string, it *item, res *providers.AsrResult) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	if err := d.Blobs.Put(ctx, jobID, key, int64(len(data)), bytes.NewReader(data)); err != nil {
		return &providers.Error{Class: providers.Retryable, Code: "blob_put"}
	}
	it.state = stateSucceeded
	it.out = ItemOutput{ResultSHA256: key, Profile: d.Profile.Name, Dialect: d.Profile.Dialect, Model: res.Model}
	return nil
}

// fail records a failed attempt (README adapter table).
func (d Deps) fail(it *item, err error, last bool) {
	var e *providers.Error
	if !errors.As(err, &e) {
		e = &providers.Error{Class: providers.Retryable, Code: "internal"}
	}
	it.err = e
	it.out = ItemOutput{ErrorClass: string(e.Class), ErrorCode: e.Code}
	switch e.Class {
	case providers.PermanentInput:
		it.state, it.gapReason = stateGap, GapASRRejectedInput
	case providers.Retryable:
		if last {
			it.state, it.gapReason = stateGap, GapASRFailed
		}
	case providers.InvalidResponse:
		if last {
			it.state, it.gapReason = stateGap, GapASRInvalidResponse
		}
	}
}

// decide turns the items into the step result (README adapter table).
func (d Deps) decide(run queue.Run, items []*item, unreadable int) queue.Result {
	commit := func(ctx context.Context, tx pgx.Tx) error { return writeItems(ctx, tx, run.JobID, items) }
	out := Output{Tracks: len(items) + unreadable, Gaps: unreadable}
	var pending *providers.Error
	var retryAfter time.Duration
	for _, it := range items {
		switch {
		case it.err != nil && it.err.Class == providers.PermanentConfig:
			return queue.Result{Outcome: queue.Permanent, Reason: ReasonConfigError,
				ErrorClass: string(it.err.Class), ErrorCode: it.err.Code, Commit: commit}
		case it.state == stateSucceeded:
			out.Transcribed++
		case it.state == stateGap:
			out.Gaps++
		default:
			if pending == nil {
				pending = it.err
			}
			if it.err != nil {
				retryAfter = max(retryAfter, it.err.RetryAfter)
			}
		}
	}
	if pending != nil {
		return queue.Result{Outcome: queue.Transient, ErrorClass: string(pending.Class), ErrorCode: pending.Code,
			RetryAfter: retryAfter, Commit: commit}
	}
	if out.Transcribed == 0 {
		return queue.Result{Outcome: queue.Permanent, Reason: ReasonAllTracksFailed, Commit: commit}
	}
	b, _ := json.Marshal(out)
	return queue.Result{Outcome: queue.OK, Output: b, Commit: commit}
}

// writeItems upserts the rows of the tracks sent in this run.
func writeItems(ctx context.Context, tx pgx.Tx, jobID string, items []*item) error {
	for _, it := range items {
		if !it.sent {
			continue
		}
		var reason *string
		if it.gapReason != "" {
			reason = &it.gapReason
		}
		out, err := json.Marshal(it.out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO step_items (job_id, item, state, attempt, gap_reason, output)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (job_id, item) DO UPDATE SET state = EXCLUDED.state, attempt = EXCLUDED.attempt,
				gap_reason = EXCLUDED.gap_reason, output = EXCLUDED.output`,
			jobID, it.ref, it.state, it.attempt, reason, out); err != nil {
			return fmt.Errorf("step_items %s: %w", it.ref, err)
		}
	}
	return nil
}

// errorCode reduces an internal error to a code for the log (no paths or
// content).
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
	}
	return "internal"
}
