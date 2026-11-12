package admin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Detail is what the job page of the admin web UI shows beyond the job
// body (architecture.md §11): the job it was rerun from, the current step
// and every step. Not part of the HTTP API. Like the job body, it holds
// IDs, states, codes and timestamps only (privacy.md P-5): no step output,
// no worker name.
type Detail struct {
	Job
	RerunOf     *string
	CurrentStep *string
	Steps       []Step
}

// Step is one row of the steps table, in pipeline order.
type Step struct {
	Name           string
	State          string
	Attempt        int
	RunAfter       string // RFC 3339 UTC
	LastErrorClass *string
	LastErrorCode  *string
}

// JobDetail returns the job body with its steps; ErrNotFound if the job does
// not exist. The reads share one snapshot (a read-only repeatable-read
// transaction), so a job erased meanwhile is ErrNotFound, never a mix.
func (a *API) JobDetail(ctx context.Context, jobID string) (Detail, error) {
	jobID, ok := normalizeJobID(jobID)
	if !ok {
		return Detail{}, ErrNotFound
	}
	var d Detail
	err := pgx.BeginTxFunc(ctx, a.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			if d.Job, _, err = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id = $1::uuid`, jobID)); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT rerun_of::text, current_step FROM jobs WHERE job_id = $1::uuid`, jobID).
				Scan(&d.RerunOf, &d.CurrentStep); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT name, state, attempt, run_after, last_error_class, last_error_code
				FROM steps WHERE job_id = $1::uuid
				ORDER BY array_position(ARRAY['validate', 'normalize', 'transcribe', 'assemble', 'summarize',
					'render', 'deliver', 'cleanup'], name)`, jobID)
			if err != nil {
				return err
			}
			d.Steps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Step, error) {
				var (
					s        Step
					runAfter time.Time
				)
				err := row.Scan(&s.Name, &s.State, &s.Attempt, &runAfter, &s.LastErrorClass, &s.LastErrorCode)
				s.RunAfter = formatTime(runAfter)
				return s, err
			})
			return err
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	return d, nil
}
