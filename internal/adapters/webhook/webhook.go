// Package webhook is the webhook sink (docs/reference/sinks.md §4b,
// ADR-016): one POST per call to the URL fixed in the sink profile, signed
// as Standard Webhooks define it (HMAC-SHA256, the v1 scheme;
// https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md
// @ 36afae7, "Signature scheme", "Webhook headers"), with plain net/http.
// The client never retries and never follows redirects; the job queue is
// the only retry layer (architecture.md §5.3).
//
// Nothing here logs or returns the URL, a secret, the title, the links,
// the summary or a response body (privacy.md §6, §11.3).
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/sink"
)

// Type is the sink type in sink profiles.
const Type = "webhook"

// EventType is the body's type (sinks.md §4b.2).
const EventType = "recap.summary.delivered"

// DefaultTimeout is the default request_timeout (sinks.md §4b.1).
const DefaultTimeout = 30 * time.Second

// Content values of a profile (sinks.md §4b.1).
const (
	ContentNotice  = "notice"
	ContentSummary = "summary"
)

// maxResponseBytes bounds what is read of a response body; it is only
// drained, never parsed.
const maxResponseBytes = 64 << 10

// Outcomes is every value of the outcome label this sink counts in
// recap_external_requests_total: ok or an error class.
var Outcomes = []string{metrics.OK, string(sink.NotSent), string(sink.Uncertain), string(sink.Config),
	string(sink.Rejected)}

// Client sends to one webhook sink profile.
type Client struct {
	profile string
	url     string
	keys    [][]byte
	summary bool
	timeout time.Duration
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// New returns a client for profile with the receiver URL, the signing keys
// (the decoded bytes after whsec_, in file order), the content (notice or
// summary) and the request timeout (0: DefaultTimeout).
func New(profile, url string, keys [][]byte, content string, timeout time.Duration, log *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	metrics.InitExternal(profile, metrics.KindSink, Outcomes...)
	return &Client{
		profile: profile,
		url:     url,
		keys:    keys,
		summary: content == ContentSummary,
		timeout: timeout,
		client: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// The URL is fixed in the profile (sinks.md §4b.3): a redirect
			// is answered as it is, and classified as config.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log,
		now: time.Now,
	}
}

// payload is the body of sinks.md §4b.2.
type payload struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      data   `json:"data"`
}

type data struct {
	JobID         string      `json:"job_id"`
	SinkProfile   string      `json:"sink_profile"`
	Title         string      `json:"title"`
	Links         []sink.Link `json:"links"`
	Content       string      `json:"content"`
	SummaryMD     *string     `json:"summary_md"`
	SummarySHA256 *string     `json:"summary_sha256"`
}

// Precheck checks the request before anything is committed (sinks.md §2.4,
// §4b.3): the title always, the summary only with content: summary, and
// rendered_at. A violation is Rejected.
func (c *Client) Precheck(req sink.Request, maxBytes int64) *sink.Error {
	if c.summary {
		if err := sink.Check(req, maxBytes); err != nil {
			return err
		}
	} else if err := sink.CheckTitle(req); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, req.RenderedAt); err != nil {
		return &sink.Error{Class: sink.Rejected, Code: "rendered_at_invalid"}
	}
	return nil
}

// Body returns the JSON body of req: compact UTF-8 JSON, the same on every
// attempt (timestamp = rendered_at, sinks.md §4b.2).
func (c *Client) Body(req sink.Request) ([]byte, error) {
	d := data{JobID: req.JobID, SinkProfile: req.SinkProfile, Title: req.Title, Links: req.Links,
		Content: ContentNotice}
	if d.Links == nil {
		d.Links = []sink.Link{}
	}
	if c.summary {
		md, sum := req.SummaryMD, sha256.Sum256([]byte(req.SummaryMD))
		hexSum := hex.EncodeToString(sum[:])
		d.Content, d.SummaryMD, d.SummarySHA256 = ContentSummary, &md, &hexSum
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload{Type: EventType, Timestamp: req.RenderedAt, Data: d}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// MessageID is the webhook-id of a job and sink: the same on every attempt,
// so a receiver can drop repeats (sinks.md §4b.2). Job IDs and profile
// names contain no ".".
func MessageID(jobID, profile string) string { return jobID + ":" + profile }

// Signature is the webhook-signature header: one v1 signature per key over
// "<id>.<timestamp>.<body>", separated by a space.
func Signature(keys [][]byte, id, timestamp string, body []byte) string {
	sigs := make([]string, 0, len(keys))
	for _, k := range keys {
		mac := hmac.New(sha256.New, k)
		mac.Write([]byte(id + "." + timestamp + "."))
		mac.Write(body)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(sigs, " ")
}

// Send posts one signed request. Any 2xx is success with an empty document
// ID; otherwise it returns a *sink.Error (sinks.md §4b.3).
func (c *Client) Send(ctx context.Context, req sink.Request) (string, error) {
	start := time.Now()
	err := c.send(ctx, req)
	outcome := metrics.OK
	attrs := []any{"job_id", req.JobID, "profile", c.profile, "duration_ms", time.Since(start).Milliseconds()}
	var serr *sink.Error
	if errors.As(err, &serr) {
		outcome = string(serr.Class)
		attrs = append(attrs, "error_class", outcome, "error_code", serr.Code)
	}
	metrics.ExternalRequest(c.profile, metrics.KindSink, outcome)
	c.log.Info("webhook: post", attrs...)
	if err != nil {
		return "", err
	}
	return "", nil
}

func (c *Client) send(ctx context.Context, req sink.Request) error {
	body, err := c.Body(req)
	if err != nil {
		return &sink.Error{Class: sink.Rejected, Code: "encode_request"}
	}
	id := MessageID(req.JobID, req.SinkProfile)
	ts := strconv.FormatInt(c.now().Unix(), 10)

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var wrote atomic.Bool
	ctx = sink.TraceWrites(ctx, &wrote)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return &sink.Error{Class: sink.Config, Code: "bad_url"}
	}
	r.GetBody = nil // the transport must not replay the request on its own
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("webhook-id", id)
	r.Header.Set("webhook-timestamp", ts)
	r.Header.Set("webhook-signature", Signature(c.keys, id, ts, body))

	resp, err := c.client.Do(r)
	if err != nil {
		return sink.TransportError(err, wrote.Load())
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	c.log.Debug("webhook: response", "job_id", req.JobID, "profile", c.profile, "status", resp.StatusCode)
	return statusError(resp)
}

// statusError classifies a response (sinks.md §4b.3); nil for 2xx.
func statusError(resp *http.Response) error {
	s := resp.StatusCode
	code := "http_" + strconv.Itoa(s)
	switch {
	case s >= 200 && s < 300:
		return nil
	case s == http.StatusTooManyRequests:
		return &sink.Error{Class: sink.NotSent, Code: "throttled", RetryAfter: sink.RetryAfter(resp.Header.Get("Retry-After"))}
	case s >= 300 && s < 400, s == http.StatusUnauthorized, s == http.StatusForbidden, s == http.StatusNotFound,
		s == http.StatusGone:
		return &sink.Error{Class: sink.Config, Code: code}
	case s >= 400 && s < 500:
		return &sink.Error{Class: sink.Rejected, Code: code}
	}
	// 5xx, and 1xx that the client returns: the receiver may have acted.
	return &sink.Error{Class: sink.Uncertain, Code: code}
}
