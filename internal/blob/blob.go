// Package blob defines the job-scoped blob store (docs/reference/architecture.md §8,
// ADR-011). Every blob lives under jobs/<job_id>/sha256/<hex> and is verified
// against its SHA-256 on write and on read. A blob's role or file name is never
// part of its key.
//
// Backends: the filesystem backend in this package (development and tests) and
// the S3 backend in internal/adapters/s3blob. The conformance suite in
// internal/blob/blobtest runs against any backend.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Errors a Store returns. Backends wrap them, so callers test with errors.Is.
// An error that is none of these is permanent (configuration or programming
// error): the queue does not retry it.
var (
	// ErrNotFound: no blob under that key.
	ErrNotFound = errors.New("blob: not found")
	// ErrHashMismatch: the bytes do not match the expected SHA-256 or size.
	ErrHashMismatch = errors.New("blob: hash mismatch")
	// ErrTransient: a temporary backend failure; the caller may retry later.
	ErrTransient = errors.New("blob: transient error")
	// ErrInvalidKey: the job ID or hash is not well-formed.
	ErrInvalidKey = errors.New("blob: invalid key")
)

// Store is the blob store interface. All methods are safe to repeat.
type Store interface {
	// Put stores the size bytes of r under jobs/<jobID>/sha256/<sha256>,
	// hashing them while streaming. If the bytes do not match sha256 and size,
	// nothing is left under the key and Put returns ErrHashMismatch. If the
	// key already holds a blob that verifies, Put succeeds and may return
	// without reading r. Size -1 means unknown: then only the hash is
	// checked.
	Put(ctx context.Context, jobID, sha256 string, size int64, r io.Reader) error

	// Get opens a blob. The reader verifies the hash: a mismatch surfaces as
	// ErrHashMismatch from Read at the latest instead of io.EOF. A missing
	// blob is ErrNotFound.
	Get(ctx context.Context, jobID, sha256 string) (io.ReadCloser, error)

	// Copy copies one blob from one job to another (used by rerun). A missing
	// source is ErrNotFound.
	Copy(ctx context.Context, fromJob, toJob, sha256 string) error

	// DeletePrefix deletes every blob of a job. Nothing there is success.
	DeletePrefix(ctx context.Context, jobID string) error

	// ListJobs lists the jobs that have at least one object, sorted by ID.
	// It is meant for the orphan search of the cleanup loop.
	ListJobs(ctx context.Context) ([]Job, error)

	// Ping checks that the store is reachable and usable, for the
	// readiness check (architecture.md §9). It reads and writes nothing.
	Ping(ctx context.Context) error
}

// Job is one job prefix in the store.
type Job struct {
	ID string
	// Modified is the newest modification time of the job's objects. A
	// cleanup that keeps prefixes younger than some age uses it.
	Modified time.Time
}

var (
	jobIDRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidJobID reports whether id is a lowercase UUID in canonical text form
// (job_id is a UUIDv7, docs/reference/README.md "Identifiers").
func ValidJobID(id string) bool { return jobIDRE.MatchString(id) }

// Key returns the object key of a blob: jobs/<jobID>/sha256/<sha256>. It
// checks both parts, so a key can never leave its job's prefix.
func Key(jobID, sha256 string) (string, error) {
	if err := CheckJobID(jobID); err != nil {
		return "", err
	}
	if !sha256RE.MatchString(sha256) {
		return "", fmt.Errorf("%w: sha256 must be 64 lowercase hex digits", ErrInvalidKey)
	}
	return JobPrefix(jobID) + "sha256/" + sha256, nil
}

// CheckJobID returns ErrInvalidKey unless jobID is valid.
func CheckJobID(jobID string) error {
	if !ValidJobID(jobID) {
		return fmt.Errorf("%w: job ID must be a lowercase UUID", ErrInvalidKey)
	}
	return nil
}

// JobPrefix returns the key prefix of a job, with a trailing slash.
func JobPrefix(jobID string) string { return "jobs/" + jobID + "/" }
