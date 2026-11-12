package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

var _ Results = (*FS)(nil)

// PutSummary writes a temp file next to the key, fsyncs it and renames it
// into place, then fsyncs the directory.
func (s *FS) PutSummary(ctx context.Context, jobID string, md []byte) error {
	key, err := SummaryKey(jobID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := s.path(key)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, ctxReader{ctx, bytes.NewReader(md)}); err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("blob fs put summary: %w", err)
	}
	done = true
	return syncDir(dir)
}

// GetSummary opens the summary file.
func (s *FS) GetSummary(ctx context.Context, jobID string) (io.ReadCloser, error) {
	key, err := SummaryKey(jobID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("blob fs get summary: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("blob fs get summary: %w", err)
	}
	return f, nil
}

// DeleteResults removes the job's results directory.
func (s *FS) DeleteResults(ctx context.Context, jobID string) error {
	if err := CheckJobID(jobID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(s.path(ResultsPrefix(jobID))); err != nil {
		return fmt.Errorf("blob fs delete results: %w", err)
	}
	return nil
}

// ListResults lists the job directories below results/, with the newest
// modification time of their files (also leftover temp files).
func (s *FS) ListResults(ctx context.Context) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "results"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob fs list results: %w", err)
	}
	var jobs []Job
	for _, e := range entries {
		if !e.IsDir() || !ValidJobID(e.Name()) {
			continue
		}
		job := Job{ID: e.Name()}
		files, err := os.ReadDir(s.path(ResultsPrefix(e.Name())))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("blob fs list results: %w", err)
		}
		for _, f := range files {
			info, err := f.Info()
			if err != nil {
				continue // removed meanwhile
			}
			if info.ModTime().After(job.Modified) {
				job.Modified = info.ModTime()
			}
		}
		// An empty directory is listed too, so the cleanup loop removes it.
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}
