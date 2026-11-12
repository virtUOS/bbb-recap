// Package blobtest is the conformance suite for blob.Store backends
// (docs/reference/architecture.md §8). Each backend's tests call Run.
package blobtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/blob"
)

// Backend tells the suite how to make and damage a store.
type Backend struct {
	// New returns an empty store. It is called once per subtest.
	New func(t *testing.T) blob.Store
	// Corrupt changes the stored bytes of an existing blob behind the store's
	// back, keeping its key.
	Corrupt func(t *testing.T, s blob.Store, jobID, sha256 string)
}

// Run runs the conformance suite.
func Run(t *testing.T, b Backend) {
	tests := []struct {
		name string
		fn   func(t *testing.T, b Backend)
	}{
		{"PutGetRoundTrip", testRoundTrip},
		{"PutWrongHash", testWrongHash},
		{"PutWrongSize", testWrongSize},
		{"PutTwice", testPutTwice},
		{"PutRepairsCorrupted", testPutRepairs},
		{"GetMissing", testGetMissing},
		{"GetCorrupted", testGetCorrupted},
		{"Copy", testCopy},
		{"DeletePrefix", testDeletePrefix},
		{"ListJobs", testListJobs},
		{"InvalidKey", testInvalidKey},
		{"Ping", testPing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, b) })
	}
}

// NewJobID returns a random lowercase UUID (version 7 layout is not needed
// for keys).
func NewJobID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// content returns n deterministic bytes and their hex SHA-256.
func content(seed string, n int) ([]byte, string) {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed[i%len(seed)] ^ byte(i)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func ctx(t *testing.T) context.Context { return t.Context() }

func put(t *testing.T, s blob.Store, jobID string, data []byte, sha string) {
	t.Helper()
	if err := s.Put(ctx(t), jobID, sha, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// get reads a blob completely and returns its bytes and the read error.
func get(t *testing.T, s blob.Store, jobID, sha string) ([]byte, error) {
	t.Helper()
	rc, err := s.Get(ctx(t), jobID, sha)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func wantBlob(t *testing.T, s blob.Store, jobID, sha string, want []byte) {
	t.Helper()
	got, err := get(t, s, jobID, sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get: %d bytes differ from the %d bytes put", len(got), len(want))
	}
}

func wantNotFound(t *testing.T, s blob.Store, jobID, sha string) {
	t.Helper()
	if _, err := get(t, s, jobID, sha); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Get: err = %v, want ErrNotFound", err)
	}
}

func jobIDs(t *testing.T, s blob.Store) []string {
	t.Helper()
	jobs, err := s.ListJobs(ctx(t))
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	ids := []string{}
	for _, j := range jobs {
		if j.Modified.IsZero() {
			t.Errorf("ListJobs: job %s without Modified", j.ID)
		}
		ids = append(ids, j.ID)
	}
	return ids
}

func testRoundTrip(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	for _, n := range []int{0, 1, 300_000} {
		data, sha := content(fmt.Sprint("round", n), n)
		put(t, s, job, data, sha)
		wantBlob(t, s, job, sha, data)
	}
}

func testWrongHash(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	data, _ := content("wrong-hash", 1000)
	_, other := content("other", 1000)
	err := s.Put(ctx(t), job, other, int64(len(data)), bytes.NewReader(data))
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("Put: err = %v, want ErrHashMismatch", err)
	}
	wantNotFound(t, s, job, other)
	if ids := jobIDs(t, s); len(ids) != 0 {
		t.Fatalf("ListJobs after a failed Put = %v, want none", ids)
	}
}

func testWrongSize(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	data, sha := content("wrong-size", 1000)
	for _, size := range []int64{999, 1001} {
		err := s.Put(ctx(t), job, sha, size, bytes.NewReader(data))
		if !errors.Is(err, blob.ErrHashMismatch) {
			t.Fatalf("Put with size %d: err = %v, want ErrHashMismatch", size, err)
		}
		wantNotFound(t, s, job, sha)
	}
}

func testPutTwice(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	data, sha := content("twice", 5000)
	put(t, s, job, data, sha)
	put(t, s, job, data, sha)
	wantBlob(t, s, job, sha, data)
	if ids := jobIDs(t, s); !slices.Equal(ids, []string{job}) {
		t.Fatalf("ListJobs = %v, want [%s]", ids, job)
	}
}

func testPutRepairs(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	data, sha := content("repair", 2000)
	put(t, s, job, data, sha)
	b.Corrupt(t, s, job, sha)
	put(t, s, job, data, sha)
	wantBlob(t, s, job, sha, data)
}

func testGetMissing(t *testing.T, b Backend) {
	s := b.New(t)
	_, sha := content("missing", 10)
	wantNotFound(t, s, NewJobID(t), sha)
}

func testGetCorrupted(t *testing.T, b Backend) {
	s := b.New(t)
	job := NewJobID(t)
	data, sha := content("corrupt", 4000)
	put(t, s, job, data, sha)
	b.Corrupt(t, s, job, sha)
	if _, err := get(t, s, job, sha); !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("Get of a corrupted blob: err = %v, want ErrHashMismatch", err)
	}
}

func testCopy(t *testing.T, b Backend) {
	s := b.New(t)
	from, to := NewJobID(t), NewJobID(t)
	data, sha := content("copy", 3000)
	put(t, s, from, data, sha)
	if err := s.Copy(ctx(t), from, to, sha); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := s.Copy(ctx(t), from, to, sha); err != nil {
		t.Fatalf("Copy again: %v", err)
	}
	if err := s.DeletePrefix(ctx(t), from); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	wantBlob(t, s, to, sha, data)
	_, missing := content("copy-missing", 10)
	if err := s.Copy(ctx(t), from, to, missing); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Copy of a missing blob: err = %v, want ErrNotFound", err)
	}
}

func testDeletePrefix(t *testing.T, b Backend) {
	s := b.New(t)
	gone, kept := NewJobID(t), NewJobID(t)
	d1, sha1 := content("delete-1", 100)
	d2, sha2 := content("delete-2", 200)
	put(t, s, gone, d1, sha1)
	put(t, s, gone, d2, sha2)
	put(t, s, kept, d1, sha1)
	if err := s.DeletePrefix(ctx(t), gone); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	wantNotFound(t, s, gone, sha1)
	wantNotFound(t, s, gone, sha2)
	wantBlob(t, s, kept, sha1, d1)
	if err := s.DeletePrefix(ctx(t), gone); err != nil {
		t.Fatalf("DeletePrefix of a deleted job: %v", err)
	}
	if err := s.DeletePrefix(ctx(t), NewJobID(t)); err != nil {
		t.Fatalf("DeletePrefix of an unknown job: %v", err)
	}
	if ids := jobIDs(t, s); !slices.Equal(ids, []string{kept}) {
		t.Fatalf("ListJobs = %v, want [%s]", ids, kept)
	}
}

func testListJobs(t *testing.T, b Backend) {
	s := b.New(t)
	if ids := jobIDs(t, s); len(ids) != 0 {
		t.Fatalf("ListJobs of an empty store = %v", ids)
	}
	want := []string{NewJobID(t), NewJobID(t), NewJobID(t)}
	for i, job := range want {
		for j := 0; j <= i; j++ {
			data, sha := content(fmt.Sprint("list", j), 50)
			put(t, s, job, data, sha)
		}
	}
	slices.Sort(want)
	if ids := jobIDs(t, s); !slices.Equal(ids, want) {
		t.Fatalf("ListJobs = %v, want %v", ids, want)
	}
}

func testInvalidKey(t *testing.T, b Backend) {
	s := b.New(t)
	data, sha := content("invalid", 10)
	job := NewJobID(t)
	for _, k := range []struct{ job, sha string }{
		{"../" + job, sha},
		{"NOT-A-UUID", sha},
		{job, "../" + sha[3:]},
		{job, sha[:63]},
	} {
		if err := s.Put(ctx(t), k.job, k.sha, 10, bytes.NewReader(data)); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("Put(%q, %q): err = %v, want ErrInvalidKey", k.job, k.sha, err)
		}
		if _, err := s.Get(ctx(t), k.job, k.sha); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("Get(%q, %q): err = %v, want ErrInvalidKey", k.job, k.sha, err)
		}
	}
	if err := s.DeletePrefix(ctx(t), "../x"); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("DeletePrefix(../x): err = %v, want ErrInvalidKey", err)
	}
}

// testPing: an empty store is reachable, Ping writes nothing, and a
// cancelled context is an error.
func testPing(t *testing.T, b Backend) {
	s := b.New(t)
	if err := s.Ping(ctx(t)); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ids := jobIDs(t, s); len(ids) != 0 {
		t.Errorf("Ping left jobs behind: %v", ids)
	}
	cancelled, cancel := context.WithCancel(ctx(t))
	cancel()
	if err := s.Ping(cancelled); err == nil {
		t.Error("Ping with a cancelled context: no error")
	}
}
