package queue

import (
	"regexp"
	"testing"
	"time"
)

func TestNewJobID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	before := time.Now().UnixMilli()
	id := NewJobID()
	after := time.Now().UnixMilli()
	if !re.MatchString(id) {
		t.Fatalf("NewJobID() = %q, want a lowercase UUIDv7", id)
	}
	var ms int64
	for _, c := range id[0:8] + id[9:13] {
		ms = ms*16 + int64(hexValue(c))
	}
	if ms < before || ms > after {
		t.Errorf("timestamp %d not in [%d, %d]", ms, before, after)
	}
	if NewJobID() == id {
		t.Error("two IDs are equal")
	}
}

func hexValue(c rune) int {
	if c >= 'a' {
		return int(c-'a') + 10
	}
	return int(c - '0')
}

// The formula of architecture.md §5.3; r = 0.5 is no jitter.
func TestBackoff(t *testing.T) {
	tests := []struct {
		n          int
		retryAfter time.Duration
		r          float64
		want       time.Duration
	}{
		{1, 0, 0.5, 30 * time.Second},
		{2, 0, 0.5, time.Minute},
		{3, 0, 0.5, 2 * time.Minute},
		{6, 0, 0.5, 16 * time.Minute},
		{7, 0, 0.5, 30 * time.Minute}, // 32 m capped
		{9, 0, 0.5, 30 * time.Minute},
		{100, 0, 0.5, 30 * time.Minute},
		{1, 0, 0, 24 * time.Second},                      // −20 %
		{1, 0, 0.75, 33 * time.Second},                   // +10 %
		{7, 0, 0, 24 * time.Minute},                      // jitter applies to the capped value
		{1, 5 * time.Minute, 0.5, 5 * time.Minute},       // at least Retry-After
		{3, 30 * time.Second, 0.5, 2 * time.Minute},      // Retry-After below the backoff
		{1, 2 * time.Hour, 0.5, 30 * time.Minute},        // Retry-After capped at 30 m
		{7, 2 * time.Hour, 1.0 - 1e-9, 36 * time.Minute}, // capped Retry-After does not lower +20 %
	}
	for _, tt := range tests {
		got := Backoff(tt.n, tt.retryAfter, tt.r)
		if diff := got - tt.want; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("Backoff(%d, %v, %v) = %v, want %v", tt.n, tt.retryAfter, tt.r, got, tt.want)
		}
	}
}

func TestMaxAttempts(t *testing.T) {
	want := map[string]int{
		"validate": 3, "normalize": 3, "transcribe": 5, "assemble": 3,
		"summarize": 5, "render": 3, "deliver": 10, "cleanup": 3,
	}
	for _, s := range Steps {
		if got := MaxAttempts(s); got != want[s] {
			t.Errorf("MaxAttempts(%s) = %d, want %d", s, got, want[s])
		}
	}
}
