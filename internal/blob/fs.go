package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// FS is the filesystem backend: the same key layout as S3 below a root
// directory (RECAP_FS_ROOT). The compose default (one host, one volume shared
// by api and worker); not for Kubernetes (docs/reference/architecture.md §8).
type FS struct {
	root string
}

var _ Store = (*FS)(nil)

// NewFS returns a store below root. The directory must exist.
func NewFS(root string) (*FS, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("blob fs root: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("blob fs root %s: not a directory", root)
	}
	return &FS{root: root}, nil
}

// tempPrefix starts the names of files that are still being written. Their
// names can never be a key, which is 64 hex digits.
const tempPrefix = ".tmp-"

// Ping checks that the root is still a directory.
func (s *FS) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fi, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("%w: blob fs root: %w", ErrTransient, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: blob fs root is not a directory", ErrTransient)
	}
	return nil
}

// Root returns the root directory.
func (s *FS) Root() string { return s.root }

func (s *FS) path(key string) string { return filepath.Join(s.root, filepath.FromSlash(key)) }

// Put writes to a temp file next to the key, fsyncs it, checks the hash and
// renames it into place, then fsyncs the directory.
func (s *FS) Put(ctx context.Context, jobID, sha256 string, size int64, r io.Reader) error {
	key, err := Key(jobID, sha256)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := s.path(key)
	if s.intact(dst, sha256) {
		return nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, NewVerifyingReader(ctxReader{ctx, r}, sha256, size)); err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("blob fs put: %w", err)
	}
	done = true
	return syncDir(dir)
}

// intact reports whether path holds a file that matches sha256.
func (s *FS) intact(path, sha256 string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return Verify(f, sha256, -1) == nil
}

// Get opens the file and verifies it while it is read.
func (s *FS) Get(ctx context.Context, jobID, sha256 string) (io.ReadCloser, error) {
	key, err := Key(jobID, sha256)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("blob fs get: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("blob fs get: %w", err)
	}
	return NewVerifyingReadCloser(f, sha256, -1), nil
}

// Copy reads the source (verifying it) and puts it under the target job.
func (s *FS) Copy(ctx context.Context, fromJob, toJob, sha256 string) error {
	if err := CheckJobID(toJob); err != nil {
		return err
	}
	src, err := s.Get(ctx, fromJob, sha256)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	return s.Put(ctx, toJob, sha256, -1, src)
}

// DeletePrefix removes the job's directory.
func (s *FS) DeletePrefix(ctx context.Context, jobID string) error {
	if err := CheckJobID(jobID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(s.path(JobPrefix(jobID))); err != nil {
		return fmt.Errorf("blob fs delete: %w", err)
	}
	return nil
}

// ListJobs lists the job directories that hold at least one blob.
func (s *FS) ListJobs(ctx context.Context) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob fs list: %w", err)
	}
	var jobs []Job
	for _, e := range entries {
		if !e.IsDir() || !ValidJobID(e.Name()) {
			continue
		}
		job := Job{ID: e.Name()}
		blobs, err := os.ReadDir(s.path(JobPrefix(e.Name()) + "sha256"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("blob fs list: %w", err)
		}
		// Leftover temp files count too, so the cleanup loop finds them.
		for _, b := range blobs {
			info, err := b.Info()
			if err != nil {
				continue // removed meanwhile
			}
			if info.ModTime().After(job.Modified) {
				job.Modified = info.ModTime()
			}
		}
		if !job.Modified.IsZero() {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}

// syncDir fsyncs a directory so a rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("blob fs sync dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("blob fs sync dir: %w", err)
	}
	return nil
}

// ctxReader stops reading once the context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
