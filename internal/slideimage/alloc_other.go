//go:build !linux

package slideimage

import "context"

// withAllocator keeps wazero's default allocator outside Linux; the
// container images run on Linux (alloc_linux.go).
func withAllocator(ctx context.Context) context.Context { return ctx }
