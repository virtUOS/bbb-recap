// Package queue is the Postgres job queue of docs/reference/architecture.md §5 and
// §6: jobs with linear steps, claimed with FOR UPDATE SKIP LOCKED, leased on
// the database clock, reaped when the lease expires, retried with backoff and
// completed in one transaction.
//
// Every write that depends on a step being claimed is guarded by
// state = 'claimed' and the owner (or an expired lease for the reaper), so a
// worker that lost its step can never commit a result.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// Steps lists the step names in pipeline order (docs/reference/README.md "Steps").
var Steps = []string{"validate", "normalize", "transcribe", "assemble", "summarize", "render", "deliver", "cleanup"}

// Fixed constants (architecture.md §5.3, §6.2, §9); not configuration.
const (
	LeaseDuration     = 60 * time.Second
	HeartbeatInterval = 20 * time.Second
	PollInterval      = 2 * time.Second
	ReapInterval      = 15 * time.Second

	backoffBase = 30 * time.Second
	backoffMax  = 30 * time.Minute
)

// Worker shutdown (architecture.md §9). After the drain time of Config,
// running steps are cancelled and get cancelGrace to return (a result
// recorded in that window still counts, later ones are aborted); then
// releaseTime for the release statement. So Run returns within
// Config.Drain + StopTime after its context ends. The process-wide budget
// is the caller's (internal/ops).
const (
	cancelGrace = 2 * time.Second
	releaseTime = 3 * time.Second

	// StopTime is what Run needs after the drain time.
	StopTime = cancelGrace + releaseTime

	// DefaultDrain is the drain time when Config.Drain is 0: the 25 s
	// budget minus the 2 s pool close and StopTime (internal/ops pins the
	// sum in a test).
	DefaultDrain = 18 * time.Second
)

// MaxAttempts returns the number of attempts a step gets (architecture.md §5.3).
func MaxAttempts(step string) int {
	switch step {
	case "transcribe", "summarize":
		return 5
	case "deliver":
		return 10
	}
	return 3
}

// Backoff returns the delay before attempt n+1 after attempt n failed:
// min(30s × 2^(n−1), 30m) with ±20 % jitter, but at least retryAfter
// (itself capped at 30 m). r is a random number in [0, 1) that picks the
// jitter: 0 gives −20 %, 0.5 none, values near 1 almost +20 %.
func Backoff(n int, retryAfter time.Duration, r float64) time.Duration {
	d := backoffMax
	if n >= 1 && n <= 16 { // 30 s × 2^15 is far above the cap already
		d = min(backoffBase<<(n-1), backoffMax)
	}
	d = time.Duration(float64(d) * (0.8 + 0.4*r))
	return max(d, min(retryAfter, backoffMax))
}

// Outcome is what a step returns (docs/reference/README.md "Step outcomes").
type Outcome string

// Step outcomes.
const (
	OK        Outcome = "ok"
	Transient Outcome = "transient"
	Permanent Outcome = "permanent"
	Hold      Outcome = "hold"
)

// Run tells a handler which step it runs.
type Run struct {
	JobID   string
	Step    string
	Attempt int // 1 on the first run
}

// Result is a handler's answer. Only codes go into it, never content.
type Result struct {
	Outcome Outcome
	// Reason is the held reason (Hold) or the failure reason (Permanent),
	// one of the README values.
	Reason string
	// Detail is a code stored in jobs.detail with a hold or a failure.
	Detail string
	// ErrorClass and ErrorCode are stored on the step for Transient and
	// Permanent (architecture.md §5.3).
	ErrorClass, ErrorCode string
	// RetryAfter is the provider's Retry-After for Transient.
	RetryAfter time.Duration
	// Output is stored on the step for OK: blob keys and small
	// non-personal facts only.
	Output json.RawMessage
	// Commit, if set, runs inside the completion transaction, after the
	// guard and before the job is updated, so a step can write its own
	// rows (architecture.md §6.3). An error rolls everything back; the
	// step is then released as a transient failure (internal/
	// step_commit): retrying with backoff, the attempt counts.
	Commit func(ctx context.Context, tx pgx.Tx) error
	// Cancel requests cancellation of the job (validate on a tombstone): the
	// job continues with cleanup and ends cleaned/cancelled.
	Cancel bool
}

// Handler runs one step. ctx is cancelled when the lease is lost or the
// worker shuts down; the result is then dropped.
type Handler func(ctx context.Context, run Run) Result

// Actor is who caused a change, for the audit row (architecture.md §6.1).
type Actor struct {
	Kind string // system, admin or client
	ID   string // empty for system
}

// System is the actor of the engine's own changes.
var System = Actor{Kind: "system"}

// Errors of Cancel.
var (
	ErrNotFound       = errors.New("queue: job not found")
	ErrNotCancellable = errors.New("queue: job cannot be cancelled in its state")
)

// NewJobID returns a UUIDv7 (RFC 9562 §5.7) in lowercase canonical form: a
// 48-bit Unix millisecond timestamp, then random bits.
func NewJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go ≥ 1.24)
	ms := uint64(time.Now().UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | b[6]&0x0f // version 7
	b[8] = 0x80 | b[8]&0x3f // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewOwnerID returns a worker instance ID for steps.owner: the host name
// (the pod name under Kubernetes) and a random suffix, so two processes on
// one host differ.
func NewOwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}

// NewJob is the input of CreateJob.
type NewJob struct {
	JobID      string // from NewJobID
	PackageID  string
	ConfigHash string
	RerunOf    string // empty unless created by the admin action rerun
}

// CreateJob inserts a job in state received at step validate and one pending
// row per step. It runs in the caller's transaction (the ingest commit), so
// a rollback leaves nothing behind. It writes no audit row: the caller writes
// ingest_commit or rerun.
func CreateJob(ctx context.Context, tx pgx.Tx, j NewJob) error {
	var rerunOf *string
	if j.RerunOf != "" {
		rerunOf = &j.RerunOf
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (job_id, package_id, rerun_of, state, current_step, config_hash)
		VALUES ($1, $2, $3, 'received', 'validate', $4)`,
		j.JobID, j.PackageID, rerunOf, j.ConfigHash); err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO steps (job_id, name, state)
		SELECT $1, name, 'pending' FROM unnest($2::text[]) AS name`,
		j.JobID, Steps); err != nil {
		return fmt.Errorf("insert steps: %w", err)
	}
	return nil
}

// ReleaseHeld resumes a job that is held for one of reasons: job running,
// held reason and detail cleared, the held step runnable now. It reports
// whether the job was released. A job held for another reason, or not held,
// is left alone. It runs in the caller's transaction (the registration PUT
// that fixes the hold, metadata-interface.md §2).
func ReleaseHeld(ctx context.Context, tx pgx.Tx, by Actor, jobID string, reasons ...string) (bool, error) {
	ok, err := releaseHeld(ctx, tx, jobID, reasons)
	if err != nil || !ok {
		return false, err
	}
	return true, audit(ctx, tx, by, jobID, "running")
}

// releaseHeld is ReleaseHeld without the audit row.
func releaseHeld(ctx context.Context, tx pgx.Tx, jobID string, reasons []string) (bool, error) {
	var step string
	err := tx.QueryRow(ctx, `
		UPDATE jobs SET state = 'running', held_reason = NULL, detail = NULL, updated_at = now()
		WHERE job_id = $1 AND state = 'held' AND held_reason = ANY($2)
		RETURNING current_step`, jobID, reasons).Scan(&step)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE steps SET run_after = now() WHERE job_id = $1 AND name = $2`, jobID, step)
	return err == nil, err
}

// Cancel requests cancellation of a job (registration DELETE,
// metadata-interface.md §2.5; architecture.md §5.1). The job continues with
// cleanup, which ends it cleaned/cancelled. If a step is claimed, the move to
// cleanup happens when that step ends ("the next step boundary").
//
// Allowed from received, running, held and failed, but only while deliver
// has not been claimed: deliver is not claimed now and there is no
// deliveries row (deliver commits its sending row before the request,
// sinks.md §1.4). Otherwise it returns ErrNotCancellable (held
// delivery_uncertain included).
// Cancelling twice is not an error, and neither is cancelling a job that an
// admin purge already stops (it ends cleaned/purged).
func Cancel(ctx context.Context, tx pgx.Tx, by Actor, jobID string) error {
	// Lock the job row first, in a statement of its own. Every step
	// boundary (finish, claim, ReleaseHeld, shutdown release) takes this
	// lock too. The step rows are read afterwards, in a second statement,
	// so they are current: a join under FOR UPDATE would recheck only the
	// job row after waiting and could see stale step rows (EvalPlanQual,
	// https://www.postgresql.org/docs/16/transaction-iso.html#XACT-READ-COMMITTED).
	var state, current string
	var requested bool
	err := tx.QueryRow(ctx, `
		SELECT state, current_step, cancel_requested_at IS NOT NULL OR purge_requested_at IS NOT NULL
		FROM jobs WHERE job_id = $1
		FOR UPDATE`, jobID).Scan(&state, &current, &requested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if requested {
		return nil
	}
	var stepState string
	var deliverStarted bool
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT state FROM steps WHERE job_id = $1 AND name = $2),
			EXISTS (SELECT 1 FROM steps WHERE job_id = $1 AND name = 'deliver' AND state = 'claimed')
				OR EXISTS (SELECT 1 FROM deliveries WHERE job_id = $1)`,
		jobID, current).Scan(&stepState, &deliverStarted); err != nil {
		return err
	}
	if deliverStarted || (state != "received" && state != "running" && state != "held" && state != "failed") {
		return ErrNotCancellable
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET cancel_requested_at = now(), updated_at = now() WHERE job_id = $1`,
		jobID); err != nil {
		return err
	}
	if stepState == "claimed" {
		return nil // finish moves the job to cleanup when the step ends
	}
	if state == "held" || state == "failed" {
		// Only received and running jobs are claimable; cleanup needs it running.
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET state = 'running', held_reason = NULL, failure_reason = NULL, detail = NULL,
				updated_at = now()
			WHERE job_id = $1`, jobID); err != nil {
			return err
		}
		if err := audit(ctx, tx, by, jobID, "running"); err != nil {
			return err
		}
	}
	return toCleanup(ctx, tx, jobID)
}

// toCleanup moves a cancelled job to its cleanup step, runnable now.
func toCleanup(ctx context.Context, tx pgx.Tx, jobID string) error {
	if _, err := tx.Exec(ctx, `UPDATE jobs SET current_step = 'cleanup', updated_at = now() WHERE job_id = $1`,
		jobID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE steps SET run_after = now() WHERE job_id = $1 AND name = 'cleanup'`, jobID)
	return err
}

// audit writes a job_state audit row; detail is "<new state>" or
// "<new state>/<reason>" (architecture.md §6.1).
func audit(ctx context.Context, tx pgx.Tx, by Actor, jobID, detail string) error {
	var id *string
	if by.ID != "" {
		id = &by.ID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit (actor_kind, actor_id, action, job_id, detail)
		VALUES ($1, $2, 'job_state', $3, $4)`, by.Kind, id, jobID, detail)
	return err
}
