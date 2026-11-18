//go:build !linux

package slideimage

import "context"

// withAllocator keeps wazero's default allocator outside Linux: there the
// renderer's memory is not bounded by in-place growth (alloc_linux.go). The
// container images run on Linux.
func withAllocator(ctx context.Context) context.Context { return ctx }

func holdFrees()        {}
func releaseFrees(bool) {}
