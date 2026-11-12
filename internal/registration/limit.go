package registration

import (
	"strconv"
	"sync"
	"time"
)

// limiter allows max requests per key in fixed windows of length window
// (§2.6: 600 per minute per front end). It is per process.
type limiter struct {
	max    int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	start time.Time
	count int
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{max: n, window: window, buckets: map[string]*bucket{}}
}

// allow counts one request of key at now. If the limit is reached, it
// returns false and how long until the window ends.
func (l *limiter) allow(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		// One entry per front end: a short list of credentials.
		b = &bucket{start: now}
		l.buckets[key] = b
	}
	if b.count >= l.max {
		return b.start.Add(l.window).Sub(now), false
	}
	b.count++
	return 0, true
}

// retryAfter renders a wait as a Retry-After value in whole seconds, at
// least 1.
func retryAfter(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	return strconv.Itoa(max(s, 1))
}
