package blob

import (
	"context"
	"io"
)

// Results stores what a job keeps for pickup after its cleanup, outside the
// job prefix that cleanup deletes: the summary.md of the frontend sink under
// results/<job_id>/summary.md (architecture.md §8, sinks.md §4a, ADR-014).
// The object is written whole and replaced on a second write; it is not
// content-addressed. All methods are safe to repeat.
//
// A backend implements it in addition to Store: the filesystem backend here
// and the S3 backend (internal/adapters/s3blob). The suite in
// internal/blob/blobtest (RunResults) checks both.
type Results interface {
	// PutSummary stores md as the job's summary, replacing an earlier one.
	// A reader never sees a partly written object.
	PutSummary(ctx context.Context, jobID string, md []byte) error

	// GetSummary opens the job's summary. None stored is ErrNotFound.
	GetSummary(ctx context.Context, jobID string) (io.ReadCloser, error)

	// DeleteResults deletes everything under the job's results prefix.
	// Nothing there is success.
	DeleteResults(ctx context.Context, jobID string) error

	// ListResults lists the jobs that have a results prefix, sorted by ID,
	// for the cleanup loop.
	ListResults(ctx context.Context) ([]Job, error)
}

// SummaryName is the file name of the frontend sink's summary in the results
// prefix.
const SummaryName = "summary.md"

// ResultsPrefix returns the results key prefix of a job, with a trailing
// slash.
func ResultsPrefix(jobID string) string { return "results/" + jobID + "/" }

// SummaryKey returns the object key of a job's summary:
// results/<jobID>/summary.md. It checks the job ID, so a key can never leave
// the job's prefix.
func SummaryKey(jobID string) (string, error) {
	if err := CheckJobID(jobID); err != nil {
		return "", err
	}
	return ResultsPrefix(jobID) + SummaryName, nil
}
