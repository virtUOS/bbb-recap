package exporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempts   int
		retryAfter time.Duration
		jitter     float64
		want       time.Duration
	}{
		{1, 0, 0, 30 * time.Second},
		{2, 0, 0, 60 * time.Second},
		{3, 0, 0, 120 * time.Second},
		{8, 0, 0, time.Hour}, // 30 s × 128 > 1 h
		{50, 0, 0, time.Hour},
		{1, 0, -1, 24 * time.Second},
		{1, 0, 1, 36 * time.Second},
		{50, 0, 1, 72 * time.Minute},
		{1, 40 * time.Second, -1, 40 * time.Second}, // at least Retry-After
		{1, 300 * time.Second, 1, 300 * time.Second},
		{1, 10 * time.Second, 0, 30 * time.Second},
	}
	for _, c := range cases {
		if got := retryDelay(c.attempts, c.retryAfter, c.jitter); got != c.want {
			t.Errorf("retryDelay(%d, %v, %v) = %v, want %v", c.attempts, c.retryAfter, c.jitter, got, c.want)
		}
	}
	for range 100 {
		j := jitter()
		if j < -1 || j > 1 {
			t.Fatalf("jitter %v out of [-1, 1]", j)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2025, 10, 29, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"40":                            40 * time.Second,
		" 300 ":                         300 * time.Second,
		"-5":                            0,
		"junk":                          0,
		"999999999999":                  retryAfterMax,
		"Wed, 29 Oct 2025 10:02:00 GMT": 2 * time.Minute,
		"7200":                          time.Hour,
		"Wed, 29 Oct 2025 13:00:00 GMT": time.Hour,
		"Wed, 29 Oct 2025 09:00:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	problem := func(slug string) []byte { return []byte(`{"type":"urn:recap:problem:` + slug + `"}`) }
	cases := []struct {
		status int
		body   []byte
		reason string // "" = transient
	}{
		{408, nil, ""}, {429, problem("rate-limited"), ""}, {500, nil, ""}, {502, nil, ""}, {503, nil, ""}, {504, nil, ""},
		{401, nil, ReasonUnauthorized}, {403, problem("server-mismatch"), ReasonUnauthorized},
		{409, problem("record-already-ingested"), ReasonConflict},
		{409, problem("package-incomplete"), ""},
		{409, problem("package-committed"), ReasonRejected},
		{422, problem("manifest-invalid"), ReasonRejected}, {413, nil, ReasonRejected}, {400, nil, ReasonRejected},
		{302, nil, ReasonRejected},
	}
	for _, c := range cases {
		resp := &http.Response{StatusCode: c.status, Header: http.Header{}}
		e := classify(resp, c.body, time.Now())
		if e.reason != c.reason || e.status != c.status {
			t.Errorf("%d %s: reason %q status %d, want %q", c.status, c.body, e.reason, e.status, c.reason)
		}
	}
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"40"}}}
	if e := classify(resp, []byte(`{"type":"https://evil.example/x"}`), time.Now()); e.retryAfter != 40*time.Second || e.problem != "" {
		t.Errorf("429: retryAfter %v problem %q", e.retryAfter, e.problem)
	}
}

func TestVerifyAck(t *testing.T) {
	m := &manifest.Manifest{RecordID: "rec-1", Totals: manifest.Totals{FileCount: 3, Size: 100}}
	pkg := "aa"
	good := ingestAck{PackageID: pkg, RecordID: "rec-1", FileCount: 3, Size: 100, JobID: "019a3000-0000-7000-8000-000000000001"}
	if err := verifyAck(&good, pkg, m); err != nil {
		t.Fatalf("good ack: %v", err)
	}
	for name, mutate := range map[string]func(a *ingestAck){
		"package_id": func(a *ingestAck) { a.PackageID = "bb" },
		"record_id":  func(a *ingestAck) { a.RecordID = "rec-2" },
		"file_count": func(a *ingestAck) { a.FileCount = 4 },
		"size":       func(a *ingestAck) { a.Size = 101 },
		"job_id":     func(a *ingestAck) { a.JobID = "not-a-uuid" },
		"no job_id":  func(a *ingestAck) { a.JobID = "" },
	} {
		a := good
		mutate(&a)
		if err := verifyAck(&a, pkg, m); err == nil {
			t.Errorf("%s: mismatch accepted", name)
		}
	}
}

func fileOf(b []byte) manifest.File {
	sum := sha256.Sum256(b)
	return manifest.File{Path: "audio/x.webm", Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
}

func TestHashingReader(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 4096)
	f := fileOf(data)

	hr := &hashingReader{r: bytes.NewReader(data), want: f}
	got, err := io.ReadAll(hr)
	if err != nil || !bytes.Equal(got, data) || hr.err != nil {
		t.Fatalf("unchanged file: %d bytes, %v, %v", len(got), err, hr.err)
	}

	changed := bytes.Clone(data)
	changed[len(changed)-1] ^= 0xff
	hr = &hashingReader{r: bytes.NewReader(changed), want: f}
	got, err = io.ReadAll(hr)
	if !errors.Is(err, errSourceChanged) || len(got) == len(data) {
		t.Errorf("changed file: %v after %d bytes; the full wrong body must never be returned", err, len(got))
	}

	hr = &hashingReader{r: bytes.NewReader(data[:100]), want: f}
	if _, err = io.ReadAll(hr); !errors.Is(err, errSourceChanged) {
		t.Errorf("short file: %v", err)
	}

	// A longer file yields exactly the manifest size.
	hr = &hashingReader{r: bytes.NewReader(append(bytes.Clone(data), 'x')), want: f}
	if got, err = io.ReadAll(hr); err != nil || len(got) != len(data) {
		t.Errorf("longer file: %d bytes, %v", len(got), err)
	}
}
