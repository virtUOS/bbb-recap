// Package s3blob is the S3 backend of the blob store (docs/reference/architecture.md
// §8, ADR-011): one bucket, path-style addressing, multipart above 64 MiB,
// hash verified on write and on read. The client's own retries are off: the
// job queue is the only retry layer.
//
// Errors never carry request URLs or S3 error messages, only the operation,
// the S3 error code and the HTTP status.
package s3blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// PartSize is the multipart threshold and part size: objects up to this size
// go in one PUT.
const PartSize = 64 << 20

// Config holds plain values; reading them from the environment and secret
// files is the caller's job.
type Config struct {
	Endpoint        string // http://host:port or https://host:port, no path
	Region          string // default us-east-1
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// Store is a blob.Store on one S3 bucket.
type Store struct {
	client *minio.Client
	bucket string
}

var _ blob.Store = (*Store)(nil)

// New returns a store. It does not contact the server.
func New(cfg Config) (*Store, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("s3blob: endpoint must be http(s)://host[:port] without path, query or user info")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3blob: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1, // 1 = no retries (minio-go api.go Options.MaxRetries)
	})
	if err != nil {
		return nil, fmt.Errorf("s3blob: %w", err)
	}
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// Put uploads r unless the key already holds a blob that verifies.
//
// Writes are conditional, so that a concurrent Put of the same key never
// replaces or removes an object another Put wrote: a new object is written
// with If-None-Match: *, a corrupt one is replaced with If-Match on its
// ETag. If the condition fails, another Put got there first, and the
// object is checked like an existing one. On a hash mismatch the object
// this Put wrote is removed only while it still has this upload's ETag.
func (s *Store) Put(ctx context.Context, jobID, sha256 string, size int64, r io.Reader) error {
	key, err := blob.Key(jobID, sha256)
	if err != nil {
		return err
	}
	opts := minio.PutObjectOptions{PartSize: PartSize, ContentType: "application/octet-stream"}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	switch err = classify("stat", err); {
	case err == nil:
		if s.intact(ctx, key, sha256) {
			return nil
		}
		opts.SetMatchETag(info.ETag)
	case errors.Is(err, blob.ErrNotFound):
		opts.SetMatchETagExcept("*")
	default:
		return err
	}

	vr := blob.NewVerifyingReader(r, sha256, size)
	up, err := s.client.PutObject(ctx, s.bucket, key, vr, size, opts)
	if conditionFailed(err) {
		// Another Put wrote the key since the stat.
		if s.intact(ctx, key, sha256) {
			return nil
		}
		return fmt.Errorf("%w: s3 put: the object changed during the upload", blob.ErrTransient)
	}
	if err == nil {
		// The upload may stop after size bytes; check the end of the stream.
		err = vr.Finish()
	}
	if vr.Mismatch() {
		if up.ETag != "" {
			if rmErr := s.removeOwn(ctx, key, up.ETag); rmErr != nil {
				return fmt.Errorf("%w (removing the object failed: %w)", blob.ErrHashMismatch, rmErr)
			}
		}
		return fmt.Errorf("s3 put: %w", blob.ErrHashMismatch)
	}
	return classify("put", err)
}

// removeOwn removes the object at key if it still has the ETag etag, that
// is if no other Put has replaced it.
func (s *Store) removeOwn(ctx context.Context, key, etag string) error {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	switch err = classify("stat", err); {
	case errors.Is(err, blob.ErrNotFound):
		return nil
	case err != nil:
		return err
	case info.ETag != etag:
		return nil
	}
	return classify("delete", s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}))
}

// conditionFailed reports whether a conditional write failed because the
// object changed: 412 Precondition Failed, or 409 Conflict when a delete
// finished during the write
// (https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
func conditionFailed(err error) bool {
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && (resp.StatusCode == 412 || resp.StatusCode == 409)
}

// intact reports whether the object at key reads back with hash sha256.
func (s *Store) intact(ctx context.Context, key, sha256 string) bool {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return false
	}
	defer func() { _ = obj.Close() }()
	return blob.Verify(obj, sha256, -1) == nil
}

// Get opens the object; the reader verifies its hash.
func (s *Store) Get(ctx context.Context, jobID, sha256 string) (io.ReadCloser, error) {
	key, err := blob.Key(jobID, sha256)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, classify("get", err)
	}
	// GetObject is lazy; Stat makes a missing key show up here.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, classify("get", err)
	}
	return blob.NewVerifyingReadCloser(classifyingReader{obj}, sha256, -1), nil
}

// Copy copies server-side. Objects are at most 2 GiB (meeting-package.md §8),
// below the 5 GiB single-copy limit of S3.
func (s *Store) Copy(ctx context.Context, fromJob, toJob, sha256 string) error {
	src, err := blob.Key(fromJob, sha256)
	if err != nil {
		return err
	}
	dst, err := blob.Key(toJob, sha256)
	if err != nil {
		return err
	}
	_, err = s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: dst},
		minio.CopySrcOptions{Bucket: s.bucket, Object: src})
	return classify("copy", err)
}

// DeletePrefix deletes every object below jobs/<jobID>/.
func (s *Store) DeletePrefix(ctx context.Context, jobID string) error {
	if err := blob.CheckJobID(jobID); err != nil {
		return err
	}
	opts := minio.ListObjectsOptions{Prefix: blob.JobPrefix(jobID), Recursive: true}
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return classify("list", obj.Err)
		}
		err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{})
		if err := classify("delete", err); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ListJobs lists every object below jobs/ and groups them by job.
func (s *Store) ListJobs(ctx context.Context) ([]blob.Job, error) {
	newest := map[string]time.Time{}
	opts := minio.ListObjectsOptions{Prefix: "jobs/", Recursive: true}
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return nil, classify("list", obj.Err)
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(obj.Key, "jobs/"), "/")
		if !ok || !blob.ValidJobID(id) {
			continue
		}
		if obj.LastModified.After(newest[id]) {
			newest[id] = obj.LastModified
		}
	}
	jobs := make([]blob.Job, 0, len(newest))
	for id, t := range newest {
		jobs = append(jobs, blob.Job{ID: id, Modified: t})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}

// Ping checks that the bucket exists and the credentials may see it (a
// HEAD on the bucket, minio-go BucketExists).
func (s *Store) Ping(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err := classify("ping", err); err != nil {
		return err
	}
	if !ok {
		return errors.New("s3 ping: bucket does not exist")
	}
	return nil
}

// classify maps a client error to the blob errors. The result names the
// operation, the S3 error code and the HTTP status, never the request URL or
// the S3 error message.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err // the caller's own deadline or cancellation, not a backend failure
	}
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		// Transport errors: drop the URL, keep the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("%w: s3 %s: %v", blob.ErrTransient, op, err)
	}
	switch {
	case resp.Code == "NoSuchKey":
		return fmt.Errorf("s3 %s: %w", op, blob.ErrNotFound)
	case resp.StatusCode >= 500, resp.StatusCode == 429,
		resp.Code == "SlowDown", resp.Code == "RequestTimeout", resp.Code == "ServiceUnavailable":
		return fmt.Errorf("%w: s3 %s: %s (status %d)", blob.ErrTransient, op, resp.Code, resp.StatusCode)
	}
	return fmt.Errorf("s3 %s: %s (status %d)", op, resp.Code, resp.StatusCode)
}

// classifyingReader classifies read errors of an object body.
type classifyingReader struct{ r io.ReadCloser }

func (c classifyingReader) Close() error { return c.r.Close() }

func (c classifyingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		err = classify("get", err)
	}
	return n, err
}
