package s3blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

var _ blob.Results = (*Store)(nil)

// resultsRoot is the prefix of every results object (architecture.md §8).
const resultsRoot = "results/"

// PutSummary uploads md as results/<jobID>/summary.md in one PUT, which
// replaces an earlier object atomically (a reader sees the old or the new
// one). A summary is at most max_document_bytes (sinks.md §2.4), far below
// PartSize.
func (s *Store) PutSummary(ctx context.Context, jobID string, md []byte) error {
	key, err := blob.SummaryKey(jobID)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(md), int64(len(md)), minio.PutObjectOptions{
		ContentType: "text/markdown; charset=utf-8",
	})
	return classify("put", err)
}

// GetSummary opens the job's summary. A missing object is blob.ErrNotFound.
func (s *Store) GetSummary(ctx context.Context, jobID string) (io.ReadCloser, error) {
	key, err := blob.SummaryKey(jobID)
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
	return classifyingReader{obj}, nil
}

// DeleteResults deletes every object below results/<jobID>/.
func (s *Store) DeleteResults(ctx context.Context, jobID string) error {
	if err := blob.CheckJobID(jobID); err != nil {
		return err
	}
	opts := minio.ListObjectsOptions{Prefix: blob.ResultsPrefix(jobID), Recursive: true}
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

// ListResults lists every object below results/ and groups them by job.
func (s *Store) ListResults(ctx context.Context) ([]blob.Job, error) {
	newest := map[string]time.Time{}
	opts := minio.ListObjectsOptions{Prefix: resultsRoot, Recursive: true}
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return nil, classify("list", obj.Err)
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(obj.Key, resultsRoot), "/")
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
