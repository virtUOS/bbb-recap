package slideimage

import (
	"context"
	"sync"
	"syscall"

	"github.com/tetratelabs/wazero/experimental"
)

// Linear memory of the renderer, reserved once with mmap and grown in place
// (templates.md §3.2 item 4). wazero's default grows a Go slice by copying
// it, so old and new buffers are live together and wait for the GC; a
// module near the cap then holds several times the cap. Here the cap is
// reserved as address space (MAP_NORESERVE) and pages are committed only
// when the module touches them, so the module never holds more than the
// cap. A freed mapping is returned to the OS with MADV_DONTNEED and kept
// for the next instance; it is never unmapped, so a guest call that is
// still running after its module was closed cannot fault.

// mappings are freed mappings, ready for the next instance.
var (
	mappingsMu sync.Mutex
	mappings   [][]byte
)

type mmapMemory struct {
	buf []byte
}

// Reallocate returns the first size bytes of the reservation; nil above it.
func (m *mmapMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.buf)) {
		return nil
	}
	return m.buf[:size]
}

// Free gives the pages back to the OS and keeps the reservation.
func (m *mmapMemory) Free() {
	if m.buf == nil {
		return
	}
	_ = syscall.Madvise(m.buf, syscall.MADV_DONTNEED)
	mappingsMu.Lock()
	mappings = append(mappings, m.buf)
	mappingsMu.Unlock()
	m.buf = nil
}

// goMemory is the fallback when mmap fails: a Go slice of the full size.
type goMemory struct{ buf []byte }

func (m *goMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.buf)) {
		return nil
	}
	return m.buf[:size]
}

func (m *goMemory) Free() { m.buf = nil }

func allocate(_, max uint64) experimental.LinearMemory {
	size := uint64(MemoryLimit)
	if max > 0 && max < size {
		size = max
	}
	mappingsMu.Lock()
	for i, b := range mappings {
		if uint64(len(b)) == size {
			mappings = append(mappings[:i], mappings[i+1:]...)
			mappingsMu.Unlock()
			return &mmapMemory{buf: b}
		}
	}
	mappingsMu.Unlock()
	b, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		return &goMemory{buf: make([]byte, size)}
	}
	return &mmapMemory{buf: b}
}

// withAllocator returns a context that makes wazero use the in-place
// allocator for the modules instantiated with it.
func withAllocator(ctx context.Context) context.Context {
	return experimental.WithMemoryAllocator(ctx, experimental.MemoryAllocatorFunc(allocate))
}
