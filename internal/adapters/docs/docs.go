// Package docs is the Docs sink (docs/reference/sinks.md §3): one
// POST <docs_base_url>/api/v1.0/documents/create-for-owner/ per call with
// plain net/http (suitenumerique/docs src/backend/core/api/viewsets.py:972-995
// @ 6c730f7).
// The client never retries and never follows redirects; the job queue is
// the only retry layer (architecture.md §5.3).
//
// Response bodies are read only to pick the error class (§1.6). Nothing
// here logs or returns the title, the content, the owner or a body.
package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/virtUOS/bbb-recap/internal/metrics"
	"github.com/virtUOS/bbb-recap/internal/sink"
)

// Type is the sink type in sink profiles.
const Type = "docs"

// CreatePath is the create-for-owner endpoint (sinks.md §3.2).
const CreatePath = "/api/v1.0/documents/create-for-owner/"

// DefaultTimeout is the default request_timeout (sinks.md §3.1).
const DefaultTimeout = 60 * time.Second

// maxResponseBytes bounds what is read of a response body.
const maxResponseBytes = 64 << 10

// Outcomes is every value of the outcome label this sink counts in
// recap_external_requests_total: ok or an error class.
var Outcomes = []string{metrics.OK, string(sink.NotSent), string(sink.Uncertain), string(sink.Config),
	string(sink.RecipientUnresolved), string(sink.Rejected)}

// Client sends to one Docs sink profile.
type Client struct {
	profile string
	url     string
	key     string
	timeout time.Duration
	client  *http.Client
	log     *slog.Logger
}

// New returns a client for profile with the Docs base URL, the
// server-to-server key and the request timeout (0: DefaultTimeout).
func New(profile, baseURL, key string, timeout time.Duration, log *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	metrics.InitExternal(profile, metrics.KindSink, Outcomes...)
	return &Client{
		profile: profile,
		url:     strings.TrimRight(baseURL, "/") + CreatePath,
		key:     key,
		timeout: timeout,
		client: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// A redirect is answered as it is: following it would turn the
			// POST into a GET or send the body twice.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log,
	}
}

// createBody is the JSON body: exactly title, content, sub and email
// (sinks.md §3.2).
type createBody struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Sub     string `json:"sub"`
	Email   string `json:"email"`
}

// Send sends one create-for-owner request and returns the document ID of
// a 201, or a *sink.Error.
func (c *Client) Send(ctx context.Context, req sink.Request) (string, error) {
	start := time.Now()
	id, err := c.send(ctx, req)
	outcome := metrics.OK
	attrs := []any{"job_id", req.JobID, "profile", c.profile, "duration_ms", time.Since(start).Milliseconds()}
	var serr *sink.Error
	if errors.As(err, &serr) {
		outcome = string(serr.Class)
		attrs = append(attrs, "error_class", outcome, "error_code", serr.Code)
	}
	metrics.ExternalRequest(c.profile, metrics.KindSink, outcome)
	c.log.Info("docs: create-for-owner", attrs...)
	return id, err
}

func (c *Client) send(ctx context.Context, req sink.Request) (string, error) {
	body, err := json.Marshal(createBody{Title: req.Title, Content: req.SummaryMD, Sub: req.Owner.Sub, Email: req.Owner.Email})
	if err != nil {
		return "", &sink.Error{Class: sink.Rejected, Code: "encode_request"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// wrote is set once any part of the request may have left this process;
	// before that, a failure means Docs cannot have seen the request.
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaderField: func(string, []string) { wrote.Store(true) },
		WroteHeaders:     func() { wrote.Store(true) },
		WroteRequest:     func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", &sink.Error{Class: sink.Config, Code: "bad_url"}
	}
	r.GetBody = nil // the transport must not replay the request on its own
	r.Header.Set("Authorization", "Bearer "+c.key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(r)
	if err != nil {
		return "", transportError(err, wrote.Load())
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	c.log.Debug("docs: response", "job_id", req.JobID, "profile", c.profile, "status", resp.StatusCode)
	if resp.StatusCode == http.StatusCreated {
		var created struct {
			ID string `json:"id"`
		}
		if err != nil || json.Unmarshal(b, &created) != nil || created.ID == "" {
			return "", &sink.Error{Class: sink.Uncertain, Code: "bad_response"}
		}
		return created.ID, nil
	}
	return "", statusError(resp, b)
}

// transportError classifies a failed request: nothing written → not_sent
// (connection refused, DNS failure, TLS failure before sending), else
// uncertain (timeout or connection lost after sending).
func transportError(err error, wrote bool) error {
	timeout := errors.Is(err, context.DeadlineExceeded)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		timeout = true
	}
	switch {
	case !wrote && timeout:
		return &sink.Error{Class: sink.NotSent, Code: "connect_timeout"}
	case !wrote:
		return &sink.Error{Class: sink.NotSent, Code: "connect_failed"}
	case timeout:
		return &sink.Error{Class: sink.Uncertain, Code: "timeout"}
	}
	return &sink.Error{Class: sink.Uncertain, Code: "connection_lost"}
}

// statusError classifies a response other than 201 (sinks.md §1.6).
func statusError(resp *http.Response, body []byte) error {
	s := resp.StatusCode
	switch {
	case s == http.StatusTooManyRequests:
		return &sink.Error{Class: sink.NotSent, Code: "throttled", RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case s == http.StatusUnauthorized:
		return &sink.Error{Class: sink.Config, Code: "unauthorized"}
	case s == http.StatusForbidden:
		return &sink.Error{Class: sink.Config, Code: "forbidden"}
	case s == http.StatusBadRequest && hasField(body, "email"):
		return &sink.Error{Class: sink.RecipientUnresolved, Code: "email_ambiguous"}
	case s >= 400 && s < 500:
		return &sink.Error{Class: sink.Rejected, Code: "http_" + strconv.Itoa(s)}
	case s >= 500:
		return &sink.Error{Class: sink.Uncertain, Code: "http_" + strconv.Itoa(s)}
	}
	// 1xx, 2xx other than 201, 3xx: Docs may have created the document.
	return &sink.Error{Class: sink.Uncertain, Code: "http_" + strconv.Itoa(s)}
}

// hasField reports whether body is a JSON object with the field (the DRF
// field error shape {"email": [...]}; suitenumerique/docs
// src/backend/core/api/serializers.py:485-486 @ 6c730f7).
func hasField(body []byte, field string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

// retryAfter parses Retry-After in seconds or as an HTTP date; 0 if absent
// or invalid.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
