package blob_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/blob/blobtest"
)

func newFS(t *testing.T) blob.Store {
	t.Helper()
	s, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFSResultsConformance(t *testing.T) { blobtest.RunResults(t, newFS) }

// The fs layout: results/<job_id>/summary.md below the root (architecture.md
// §8), which CT-SINK-12 reads directly.
func TestFSSummaryPath(t *testing.T) {
	s := newFS(t).(*blob.FS)
	const job = "019a2f8c-3b40-7000-8000-000000000023"
	if err := s.PutSummary(context.Background(), job, []byte("# Summary\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root(), "results", job, "summary.md"))
	if err != nil || string(b) != "# Summary\n" {
		t.Fatalf("file at the key: %q, %v", b, err)
	}
}
