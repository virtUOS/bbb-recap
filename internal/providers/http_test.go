package providers

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestAsrAttemptTimeout(t *testing.T) {
	if AsrAttemptTimeout(9000) != 600*time.Second || AsrAttemptTimeout(3_600_000) != 1800*time.Second {
		t.Error("AsrAttemptTimeout does not follow max(600 s, 0.5 × duration)")
	}
}

func TestRetryAfterDateAndCap(t *testing.T) {
	d := RetryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat))
	if d < 85*time.Second || d > 91*time.Second {
		t.Errorf("RetryAfter(date) = %v", d)
	}
	if RetryAfter("soon") != 0 || RetryAfter("") != 0 {
		t.Error("invalid Retry-After must give 0")
	}
	if RetryAfter("99999") != MaxRetryAfter {
		t.Error("Retry-After above MaxRetryAfter must be capped")
	}
}

func TestTransportCode(t *testing.T) {
	for want, err := range map[string]error{
		"dns":       &net.DNSError{Err: "no such host", Name: "asr.invalid"},
		"connect":   &net.OpError{Op: "dial", Err: errors.New("connection refused")},
		"tls":       errors.New("tls: handshake failure"),
		"transport": errors.New("EOF"),
	} {
		if got := TransportCode(err); got != want {
			t.Errorf("TransportCode(%v) = %q, want %q", err, got, want)
		}
	}
}
