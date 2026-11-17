package sink

import (
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": 0, "x": 0, "-1": 0, "0": 0, "7": 7 * time.Second, "1800": MaxRetryAfter, "1801": MaxRetryAfter,
		"9223372036854775807": MaxRetryAfter,
	} {
		if got := RetryAfter(v); got != want {
			t.Errorf("RetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
