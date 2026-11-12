package exporter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/virtUOS/bbb-recap/internal/manifest"
)

// Push constants (§6.5).
const (
	retryBase       = 30 * time.Second
	retryMax        = time.Hour
	retryAfterMax   = time.Hour // §6.5: Retry-After is honoured up to 1 h
	requestTimeout  = 60 * time.Second
	uploadRate      = 256 << 10 // bytes per second assumed for the per-file timeout
	maxResponseBody = 4 << 20
)

// Problem slugs the exporter tells apart (§6.2).
const (
	problemPrefix            = "urn:recap:problem:"
	problemRecordIngested    = problemPrefix + "record-already-ingested"
	problemPackageIncomplete = problemPrefix + "package-incomplete"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// pushError is a failed push attempt: either transient (retry later, same
// state) or permanent with a failure reason.
type pushError struct {
	reason     string // "" = transient
	problem    string // problem type of the response, if any
	status     int    // HTTP status, 0 if there was no response
	retryAfter time.Duration
	msg        string // for logs: no bodies, no file names
	err        error  // DEBUG only
}

func (e *pushError) Error() string { return e.msg }

func transient(msg string, err error) *pushError { return &pushError{msg: msg, err: err} }

func permanent(reason, msg string) *pushError { return &pushError{reason: reason, msg: msg} }

// ingestStatus is IngestStatus (§6.2).
type ingestStatus struct {
	PackageID string   `json:"package_id"`
	State     string   `json:"state"`
	Missing   []string `json:"missing"`
}

// ingestAck is IngestAck (§6.2).
type ingestAck struct {
	PackageID string `json:"package_id"`
	State     string `json:"state"`
	JobID     string `json:"job_id"`
	RecordID  string `json:"record_id"`
	FileCount int    `json:"file_count"`
	Size      int64  `json:"size"`
}

// client talks to the ingest API. It never follows redirects.
type client struct {
	base  string
	token string
	http  *http.Client
	agent string
	now   func() time.Time
}

func newClient(base, token string) *client {
	return &client{
		base:  base,
		token: token,
		agent: ExporterName + "/" + Version(),
		now:   time.Now,
		http: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *client) packageURL(packageID string) string {
	return c.base + "/api/v1/ingest/packages/" + packageID
}

// do sends one request with its own timeout and returns the response body
// of a 2xx answer. Any other answer or failure is a *pushError.
func (c *client) do(ctx context.Context, method, url string, body io.Reader, size int64, ctype string,
	timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, permanent(ReasonRejected, "request cannot be built")
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", c.agent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transient("request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err != nil {
			return nil, transient("response cannot be read", err)
		}
		return data, nil
	}
	return nil, classify(resp, data, c.now())
}

// classify maps an error response to the rules of §5.1 and §6.5; now is
// for a date-form Retry-After.
func classify(resp *http.Response, body []byte, now time.Time) *pushError {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &p)
	if !strings.HasPrefix(p.Type, problemPrefix) {
		p.Type = ""
	}
	e := &pushError{problem: p.Type, status: resp.StatusCode, msg: "service answered with an error"}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		e.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), now)
	case http.StatusUnauthorized, http.StatusForbidden:
		e.reason = ReasonUnauthorized
	case http.StatusConflict:
		switch p.Type {
		case problemRecordIngested:
			e.reason = ReasonConflict
		case problemPackageIncomplete:
			// Files went missing on the service (e.g. staging expired):
			// start again with the manifest PUT, which lists them (§6.3).
		default:
			e.reason = ReasonRejected
		}
	default:
		e.reason = ReasonRejected
	}
	return e
}

// parseRetryAfter reads delay-seconds or an HTTP date (RFC 9110 §10.2.3).
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if n < 0 {
			return 0
		}
		if n > int(retryAfterMax/time.Second) {
			return retryAfterMax
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), retryAfterMax)
}

// retryDelay is min(1h, 30 s × 2^(attempts−1)) ±20 % jitter, at least
// retryAfter (§6.5). jitter is in [-1, 1].
func retryDelay(attempts int, retryAfter time.Duration, jitter float64) time.Duration {
	d := retryBase
	for i := 1; i < attempts && d < retryMax; i++ {
		d *= 2
	}
	d = min(d, retryMax)
	d = time.Duration(float64(d) * (1 + 0.2*jitter))
	return max(d, retryAfter)
}

// verifyAck checks the ack against the manifest (§6.3 step 2).
func verifyAck(a *ingestAck, packageID string, m *manifest.Manifest) error {
	switch {
	case a.PackageID != packageID:
		return errors.New("ack package_id differs")
	case a.RecordID != m.RecordID:
		return errors.New("ack record_id differs")
	case a.FileCount != m.Totals.FileCount:
		return errors.New("ack file_count differs")
	case a.Size != m.Totals.Size:
		return errors.New("ack size differs")
	case !uuidRE.MatchString(a.JobID):
		return errors.New("ack job_id is not a UUID")
	}
	return nil
}

// errAckMismatch marks a commit answer that does not match the manifest.
var errAckMismatch = errors.New("ack does not match the manifest")

// push runs one attempt of §6.3 for a record in ready or delivered: manifest
// PUT, upload what is missing, manifest PUT again, then commit and verify
// the ack. State changes are saved before the next request. It returns nil
// once the record is acked.
func (d *Daemon) push(ctx context.Context, rec *Record, mb []byte) error {
	m, err := parseManifest(mb)
	if err != nil {
		return permanent(ReasonRejected, "stored manifest cannot be read")
	}
	url := d.client.packageURL(rec.PackageID)
	putManifest := func() (*ingestStatus, error) {
		body, err := d.client.do(ctx, http.MethodPut, url, bytes.NewReader(mb), int64(len(mb)),
			"application/json", requestTimeout)
		if err != nil {
			return nil, err
		}
		var st ingestStatus
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, transient("invalid status response", err)
		}
		return &st, nil
	}

	st, err := putManifest()
	if err != nil {
		return err
	}
	if st.State != "committed" && len(st.Missing) > 0 {
		if err := d.upload(ctx, rec, m, st.Missing); err != nil {
			return err
		}
		if st, err = putManifest(); err != nil {
			return err
		}
		if st.State != "committed" && len(st.Missing) > 0 {
			return transient("files still missing after upload", nil)
		}
	}

	if rec.State == StateReady {
		rec.setState(StateDelivered, "", d.now())
		if err := d.store.Save(rec); err != nil {
			return transient("state cannot be saved", err)
		}
		d.log.Info("package delivered", "record_id", rec.RecordID, "package_id", rec.PackageID,
			"state", StateDelivered)
	}

	body, err := d.client.do(ctx, http.MethodPost, url+"/commit", strings.NewReader("{}"), 2,
		"application/json", requestTimeout)
	if err != nil {
		return err
	}
	var ack ingestAck
	if err := json.Unmarshal(body, &ack); err != nil {
		return transient("invalid ack", err)
	}
	if err := verifyAck(&ack, rec.PackageID, m); err != nil {
		d.log.Error("ack does not match the manifest, not trusted", "record_id", rec.RecordID,
			"package_id", rec.PackageID, "state", rec.State, "check", err.Error())
		return transient(errAckMismatch.Error(), errAckMismatch)
	}
	rec.JobID = ack.JobID
	rec.LastError = nil
	rec.LastAttemptAt = d.now().UTC()
	rec.NextAttemptAt = time.Time{}
	rec.setState(StateAcked, "", d.now())
	if err := d.store.Save(rec); err != nil {
		return transient("state cannot be saved", err)
	}
	d.log.Info("package acked", "record_id", rec.RecordID, "package_id", rec.PackageID,
		"job_id", rec.JobID, "state", StateAcked, "size", m.Totals.Size)
	return nil
}

// upload sends each missing hash once, in the order of missing (§6.3 step 5,
// §8.1). Each file is read in place and re-hashed while it streams (§4.2).
func (d *Daemon) upload(ctx context.Context, rec *Record, m *manifest.Manifest, missing []string) error {
	byHash := map[string]manifest.File{}
	for _, f := range m.Files {
		if _, ok := byHash[f.SHA256]; !ok {
			byHash[f.SHA256] = f
		}
	}
	root, err := os.OpenRoot(filepath.Join(d.cfg.RawDir, rec.RecordID))
	if err != nil {
		return permanent(ReasonSourceMissing, "raw dir is gone")
	}
	defer func() { _ = root.Close() }()

	sent := map[string]bool{}
	for _, sha := range missing {
		if sent[sha] {
			continue
		}
		f, ok := byHash[sha]
		if !ok {
			return transient("service lists a hash that is not in the manifest", nil)
		}
		start := d.now()
		if err := d.uploadFile(ctx, root, rec.PackageID, f); err != nil {
			return err
		}
		sent[sha] = true
		d.log.Debug("file uploaded", "record_id", rec.RecordID, "path", f.Path, "size", f.Size,
			"duration_ms", d.now().Sub(start).Milliseconds())
	}
	return nil
}

func (d *Daemon) uploadFile(ctx context.Context, root *os.Root, packageID string, f manifest.File) error {
	fi, err := root.Lstat(f.Path)
	if err != nil {
		return permanent(ReasonSourceMissing, "a package file is missing")
	}
	if !fi.Mode().IsRegular() || fi.Size() != f.Size {
		return permanent(ReasonSourceChanged, "a package file changed")
	}
	fh, err := root.Open(f.Path)
	if err != nil {
		return permanent(ReasonSourceMissing, "a package file cannot be opened")
	}
	defer func() { _ = fh.Close() }()

	hr := &hashingReader{r: fh, want: f}
	timeout := requestTimeout + time.Duration(f.Size/uploadRate)*time.Second
	_, err = d.client.do(ctx, http.MethodPut, d.client.packageURL(packageID)+"/files/"+f.SHA256,
		hr, f.Size, "application/octet-stream", timeout)
	if hr.err != nil { // the source decides first: the service may have seen a broken body
		if errors.Is(hr.err, errSourceChanged) {
			return permanent(ReasonSourceChanged, "a package file changed")
		}
		return permanent(ReasonSourceMissing, "a package file cannot be read")
	}
	return err
}

var errSourceChanged = errors.New("file differs from the manifest")

// hashingReader streams a file and checks size and SHA-256 against the
// manifest. The last chunk is held back if the hash does not match, so the
// service never receives a complete wrong body. err records why reading
// stopped early.
type hashingReader struct {
	r    io.Reader
	want manifest.File
	h    hash.Hash
	n    int64
	err  error
}

func (h *hashingReader) Read(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}
	if h.h == nil {
		h.h = sha256.New()
	}
	if rest := h.want.Size - h.n; int64(len(p)) > rest {
		p = p[:rest]
	}
	if len(p) == 0 {
		return 0, io.EOF
	}
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	switch {
	case h.n == h.want.Size:
		if hex.EncodeToString(h.h.Sum(nil)) != h.want.SHA256 {
			h.err = errSourceChanged
			return 0, h.err
		}
		return n, io.EOF
	case err == io.EOF: // shorter than the manifest says
		h.err = errSourceChanged
		return 0, h.err
	case err != nil:
		h.err = fmt.Errorf("read: %w", err)
		return 0, h.err
	}
	return n, nil
}

// jitter returns a value in [-1, 1].
func jitter() float64 { return rand.Float64()*2 - 1 }
