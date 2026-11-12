package s3blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

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
