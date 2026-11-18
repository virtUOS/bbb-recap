//go:build race

package slideimage

// raceEnabled: the race detector keeps shadow memory next to the Go heap, so
// the peak RSS bounds of memory_test.go do not apply.
const raceEnabled = true
