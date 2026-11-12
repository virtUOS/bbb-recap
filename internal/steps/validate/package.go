package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// Querier is what the step code needs from a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Job is a job's committed package as the steps see it (packages and
// package_files rows, architecture.md §6.1). The normalize step uses it too.
type Job struct {
	JobID       string
	PackageID   string
	BBBServerID string
	RecordID    string
	MeetingID   string
	Files       []manifest.File // sorted by path
}

// LoadJob reads the package of a job.
func LoadJob(ctx context.Context, q Querier, jobID string) (*Job, error) {
	j := &Job{JobID: jobID}
	if err := q.QueryRow(ctx, `
		SELECT p.package_id, p.bbb_server_id, p.record_id, p.meeting_id
		FROM jobs j JOIN packages p ON p.package_id = j.package_id
		WHERE j.job_id = $1`, jobID).Scan(&j.PackageID, &j.BBBServerID, &j.RecordID, &j.MeetingID); err != nil {
		return nil, fmt.Errorf("load job package: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT path, role, size, sha256 FROM package_files
		WHERE package_id = $1 ORDER BY path COLLATE "C"`, j.PackageID)
	if err != nil {
		return nil, fmt.Errorf("load package files: %w", err)
	}
	j.Files, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (manifest.File, error) {
		var f manifest.File
		err := r.Scan(&f.Path, &f.Role, &f.Size, &f.SHA256)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("load package files: %w", err)
	}
	return j, nil
}

// Microphones returns the paths of the job's microphone files.
func (j *Job) Microphones() []string {
	var out []string
	for _, f := range j.Files {
		if f.Role == manifest.RoleMicrophone {
			out = append(out, f.Path)
		}
	}
	return out
}

// ReadManifest reads the job's manifest: the blob whose SHA-256 is the
// package_id (the ingest stores it under that key). The blob store verifies the hash.
func ReadManifest(ctx context.Context, store blob.Store, j *Job) (*manifest.Manifest, error) {
	rc, err := store.Get(ctx, j.JobID, j.PackageID)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, manifest.MaxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// FS returns a read-only fs.FS of the job's package files backed by the blob
// store: path → package_files.sha256 → blob.Get, which verifies the hash
// while reading. Paths not in the package do not exist. Errors carry no
// paths (they contain BBB user IDs).
func (j *Job) FS(ctx context.Context, store blob.Store) fs.FS {
	files := make(map[string]manifest.File, len(j.Files))
	for _, f := range j.Files {
		files[f.Path] = f
	}
	return &blobFS{ctx: ctx, store: store, jobID: j.JobID, files: files}
}

type blobFS struct {
	ctx   context.Context
	store blob.Store
	jobID string
	files map[string]manifest.File
}

func (b *blobFS) Open(name string) (fs.File, error) {
	f, ok := b.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	rc, err := b.store.Get(b.ctx, b.jobID, f.SHA256)
	if err != nil {
		return nil, fmt.Errorf("open package file: %w", err)
	}
	return &blobFile{ReadCloser: rc, info: fileInfo{name: path.Base(name), size: f.Size}}, nil
}

type blobFile struct {
	io.ReadCloser
	info fileInfo
}

func (f *blobFile) Stat() (fs.FileInfo, error) { return f.info, nil }

type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

// OnlyFiles returns fsys restricted to the given package files; every other
// path does not exist. The tools use it to apply the allow-list of
// meeting-package.md §5 to a directory.
func OnlyFiles(fsys fs.FS, files []manifest.File) fs.FS {
	allowed := make(map[string]bool, len(files))
	for _, f := range files {
		allowed[f.Path] = true
	}
	return onlyFS{fsys, allowed}
}

type onlyFS struct {
	fsys    fs.FS
	allowed map[string]bool
}

func (o onlyFS) Open(name string) (fs.File, error) {
	if !o.allowed[name] {
		return nil, fs.ErrNotExist
	}
	return o.fsys.Open(name)
}
