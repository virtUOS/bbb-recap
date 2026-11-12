package blob_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/blob/blobtest"
)

func TestFSConformance(t *testing.T) {
	blobtest.Run(t, blobtest.Backend{
		New: func(t *testing.T) blob.Store {
			s, err := blob.NewFS(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		Corrupt: func(t *testing.T, s blob.Store, jobID, sha256 string) {
			key, err := blob.Key(jobID, sha256)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.(*blob.FS).Root(), filepath.FromSlash(key))
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0xff
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
}

func TestFSLayout(t *testing.T) {
	root := t.TempDir()
	s, err := blob.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	job := blobtest.NewJobID(t)
	const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // empty input
	if err := s.Put(t.Context(), job, sha, 0, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", job, "sha256", sha)); err != nil {
		t.Fatalf("blob not at jobs/<job_id>/sha256/<hex>: %v", err)
	}
}

func TestNewFSMissingRoot(t *testing.T) {
	if _, err := blob.NewFS(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("NewFS of a missing directory: no error")
	}
}

// Ping fails once the root is gone.
func TestFSPingRootGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := blob.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(t.Context()); !errors.Is(err, blob.ErrTransient) {
		t.Errorf("Ping = %v, want ErrTransient", err)
	}
}
