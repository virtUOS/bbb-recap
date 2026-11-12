package metrics

import "time"

// SetJobsClock replaces the clock of c (tests only).
func SetJobsClock(c *JobsCollector, now func() time.Time) { c.now = now }
