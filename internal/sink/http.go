package sink

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"
	"time"
)

// Helpers for the HTTP sinks (internal/adapters/docs, internal/adapters/webhook).

// MaxRetryAfter caps a Retry-After (architecture.md §5.3: at most 30 min).
const MaxRetryAfter = 30 * time.Minute

// TraceWrites returns ctx with a client trace that sets wrote once any part
// of the request may have left this process. Before that, a failure means
// the sink cannot have seen the request.
func TraceWrites(ctx context.Context, wrote *atomic.Bool) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaderField: func(string, []string) { wrote.Store(true) },
		WroteHeaders:     func() { wrote.Store(true) },
		WroteRequest:     func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	})
}

// TransportError classifies a failed request (§1.6): nothing written →
// not_sent (connection refused, DNS failure, TLS failure before sending),
// else uncertain (timeout or connection lost after sending).
func TransportError(err error, wrote bool) *Error {
	timeout := errors.Is(err, context.DeadlineExceeded)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		timeout = true
	}
	switch {
	case !wrote && timeout:
		return &Error{Class: NotSent, Code: "connect_timeout"}
	case !wrote:
		return &Error{Class: NotSent, Code: "connect_failed"}
	case timeout:
		return &Error{Class: Uncertain, Code: "timeout"}
	}
	return &Error{Class: Uncertain, Code: "connection_lost"}
}

// RetryAfter parses Retry-After in seconds or as an HTTP date, capped at
// MaxRetryAfter; 0 if absent or invalid.
func RetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
		// Compared before the multiplication, which could overflow.
		if n > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), MaxRetryAfter)
	}
	return 0
}
