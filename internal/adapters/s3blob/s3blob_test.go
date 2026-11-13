package s3blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/virtUOS/bbb-recap/internal/blob"
	"github.com/virtUOS/bbb-recap/internal/blob/blobtest"
)

// The conformance suite runs against a local MinIO:
//
//	TEST_S3_ENDPOINT=http://127.0.0.1:59013 TEST_S3_ACCESS_KEY=… TEST_S3_SECRET_KEY=…
//
// Each subtest creates its own bucket.
func TestS3Conformance(t *testing.T) {
	admin, newStore := minioStores(t)
	blobtest.Run(t, blobtest.Backend{
		New: newStore,
		Corrupt: func(t *testing.T, s blob.Store, jobID, sha256 string) {
			key, err := blob.Key(jobID, sha256)
			if err != nil {
				t.Fatal(err)
			}
			bad := []byte("corrupted behind the store's back")
			_, err = admin.PutObject(t.Context(), s.(*Store).bucket, key, bytes.NewReader(bad), int64(len(bad)), minio.PutObjectOptions{})
			if err != nil {
				t.Fatalf("corrupt: %v", err)
			}
		},
	})
}

// TestS3ResultsConformance runs the blob.Results suite (the frontend sink's
// summaries, sinks.md §4a) against the same local MinIO.
func TestS3ResultsConformance(t *testing.T) {
	_, newStore := minioStores(t)
	blobtest.RunResults(t, newStore)
}

// minioStores returns an admin client for the local MinIO of
// TEST_S3_ENDPOINT and a function that makes a store on a new bucket. It
// skips without the variables.
func minioStores(t *testing.T) (*minio.Client, func(t *testing.T) blob.Store) {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	access, secret := os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY")
	if endpoint == "" || access == "" || secret == "" {
		t.Skip("skipped: TEST_S3_ENDPOINT, TEST_S3_ACCESS_KEY and TEST_S3_SECRET_KEY not set")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(access, secret, ""),
		Secure:       u.Scheme == "https",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return admin, func(t *testing.T) blob.Store {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		bucket := "recap-t013-" + hex.EncodeToString(b[:])
		if err := admin.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("make bucket: %v", err)
		}
		s, err := New(Config{Endpoint: endpoint, Bucket: bucket, AccessKeyID: access, SecretAccessKey: secret})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

func TestNewRejectsBadEndpoint(t *testing.T) {
	for _, ep := range []string{"", "127.0.0.1:9000", "ftp://h", "http://h/path", "http://h?x=1", "http://u:p@h"} {
		if _, err := New(Config{Endpoint: ep, Bucket: "b"}); err == nil {
			t.Errorf("New(%q): no error", ep)
		}
	}
	if _, err := New(Config{Endpoint: "https://s3.recap.test", Bucket: ""}); err == nil {
		t.Error("New without bucket: no error")
	}
	if _, err := New(Config{Endpoint: "https://s3.recap.test", Bucket: "b"}); err != nil {
		t.Errorf("New: %v", err)
	}
}

func TestClassifyContext(t *testing.T) {
	for _, e := range []error{context.Canceled, context.DeadlineExceeded} {
		got := classify("op", e)
		if got != e || errors.Is(got, blob.ErrTransient) {
			t.Errorf("classify(%v) = %v, want it unchanged", e, got)
		}
	}
}

func TestConditionFailed(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{minio.ErrorResponse{Code: "PreconditionFailed", StatusCode: 412}, true},
		{minio.ErrorResponse{Code: "Conflict", StatusCode: 409}, true},
		{minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, false},
		{errors.New("connection refused"), false},
		{nil, false},
	} {
		if got := conditionFailed(c.err); got != c.want {
			t.Errorf("conditionFailed(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want error // nil = permanent
	}{
		{minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}, blob.ErrNotFound},
		{minio.ErrorResponse{Code: "InternalError", StatusCode: 500}, blob.ErrTransient},
		{minio.ErrorResponse{Code: "SlowDown", StatusCode: 503}, blob.ErrTransient},
		{&url.Error{Op: "Get", URL: "http://h/b/k?uploadId=SECRET", Err: errors.New("connection refused")}, blob.ErrTransient},
		{minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "raw body text"}, nil},
	}
	for _, c := range cases {
		got := classify("op", c.err)
		if c.want != nil && !errors.Is(got, c.want) {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
		if c.want == nil && (errors.Is(got, blob.ErrTransient) || errors.Is(got, blob.ErrNotFound)) {
			t.Errorf("classify(%v) = %v, want permanent", c.err, got)
		}
		if msg := got.Error(); bytes.Contains([]byte(msg), []byte("SECRET")) || bytes.Contains([]byte(msg), []byte("raw body")) {
			t.Errorf("classify leaks URL or message: %q", msg)
		}
	}
}

// gatedReader returns data, but before its first byte it signals reached
// and waits for release.
type gatedReader struct {
	r        io.Reader
	reached  chan struct{}
	release  chan struct{}
	signaled bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.signaled {
		g.signaled = true
		close(g.reached)
		<-g.release
	}
	return g.r.Read(p)
}

// Two concurrent Puts of the same key, one with bytes that do not match
// the hash: the bad Put never removes or replaces the object the good one
// wrote. The bad Put has seen no object and holds its upload open while the
// good one runs; on MinIO the conditional write makes the good Put wait
// for it.
func TestS3ConcurrentPut(t *testing.T) {
	_, newStore := minioStores(t)
	s := newStore(t)
	job := blobtest.NewJobID(t)
	good := bytes.Repeat([]byte("good blob bytes "), 256)
	bad := bytes.Repeat([]byte("BAD! blob bytes "), 256)
	sum := sha256.Sum256(good)
	sha := hex.EncodeToString(sum[:])

	g := &gatedReader{r: bytes.NewReader(bad), reached: make(chan struct{}), release: make(chan struct{})}
	badDone := make(chan error, 1)
	go func() { badDone <- s.Put(t.Context(), job, sha, int64(len(good)), g) }()
	select {
	case <-g.reached:
	case err := <-badDone:
		t.Fatalf("bad Put ended before sending its body: %v", err)
	}
	goodDone := make(chan error, 1)
	go func() { goodDone <- s.Put(t.Context(), job, sha, int64(len(good)), bytes.NewReader(good)) }()
	time.Sleep(200 * time.Millisecond) // without conditional writes the good Put is done by now
	close(g.release)

	if err := <-badDone; !errors.Is(err, blob.ErrHashMismatch) {
		t.Errorf("bad Put: %v, want ErrHashMismatch", err)
	}
	if err := <-goodDone; err != nil {
		t.Fatalf("good Put: %v", err)
	}
	rc, err := s.Get(t.Context(), job, sha)
	if err != nil {
		t.Fatalf("Get after both Puts: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, good) {
		t.Fatalf("Get after both Puts: %d bytes, %v", len(got), err)
	}
}

// removeOwn removes an object only while it has the given ETag, so a bad
// upload never removes an object another Put wrote over it.
func TestS3RemoveOwn(t *testing.T) {
	admin, newStore := minioStores(t)
	s := newStore(t).(*Store)
	key := "jobs/" + blobtest.NewJobID(t) + "/sha256/" + strings.Repeat("0", 64)
	up, err := admin.PutObject(t.Context(), s.bucket, key, strings.NewReader("other"), 5, minio.PutObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.removeOwn(t.Context(), key, `"not-this-etag"`); err != nil {
		t.Fatalf("removeOwn other ETag: %v", err)
	}
	if _, err := admin.StatObject(t.Context(), s.bucket, key, minio.StatObjectOptions{}); err != nil {
		t.Fatalf("object removed with another ETag: %v", err)
	}
	if err := s.removeOwn(t.Context(), key, up.ETag); err != nil {
		t.Fatalf("removeOwn: %v", err)
	}
	if _, err := admin.StatObject(t.Context(), s.bucket, key, minio.StatObjectOptions{}); err == nil {
		t.Fatal("object still there after removeOwn with its ETag")
	}
	if err := s.removeOwn(t.Context(), key, up.ETag); err != nil {
		t.Fatalf("removeOwn of a missing object: %v", err)
	}
}
