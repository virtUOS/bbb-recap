package providers

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Constants of providers.md §1.5 that the HTTP adapters share.
const (
	ConnectTimeout       = 30 * time.Second  // ASR and LLM
	MinAsrAttemptTimeout = 600 * time.Second // see AsrAttemptTimeout
	// AsrTimeoutRetry is the first retry after an ASR timeout: the request
	// may still run on the GPU.
	AsrTimeoutRetry   = 150 * time.Second
	LlmRequestTimeout = 600 * time.Second
	RepairAttempts    = 2 // JSON repair calls per LLM call (§3.4)
)

// AsrAttemptTimeout is max(600 s, 0.5 × duration) (§1.5).
func AsrAttemptTimeout(durationMS int64) time.Duration {
	return max(MinAsrAttemptTimeout, time.Duration(durationMS)*time.Millisecond/2)
}

// NewHTTPClient returns the HTTP client of an adapter: connect and TLS
// handshake bounded by ConnectTimeout, no overall timeout (the caller's
// context bounds a request), and no redirects: a redirect is answered as it
// is, a configuration problem. The client never retries; the job queue does
// (ADR-008).
func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = ConnectTimeout
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// AsrStatusError maps the HTTP status of an ASR reply to an error class
// (§1.4).
func AsrStatusError(resp *http.Response) *Error {
	code := "http_" + strconv.Itoa(resp.StatusCode)
	switch resp.StatusCode {
	case 408, 429, 500, 502, 503, 504:
		return &Error{Class: Retryable, Code: code, RetryAfter: RetryAfter(resp.Header.Get("Retry-After"))}
	case 400, 413, 415, 422:
		return &Error{Class: PermanentInput, Code: code}
	case 401, 403, 404:
		return &Error{Class: PermanentConfig, Code: code}
	}
	// Not in §1.4: other 5xx are treated as temporary, anything else
	// (redirects, other 4xx) as a configuration problem (assumption).
	if resp.StatusCode >= 500 {
		return &Error{Class: Retryable, Code: code}
	}
	return &Error{Class: PermanentConfig, Code: code}
}

// RetryAfter parses a Retry-After header value (delta-seconds or HTTP date),
// capped at MaxRetryAfter (architecture.md §5.3). Absent or invalid: 0.
func RetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
		if n > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0).Round(time.Second), MaxRetryAfter)
	}
	return 0
}

// TransportCode names a transport failure without its message (which may
// contain the URL).
func TransportCode(err error) string {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
		return "tls"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connect"
	}
	return "transport"
}
