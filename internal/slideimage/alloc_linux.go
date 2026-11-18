package slideimage

import (
	"context"
	"sync"
	"syscall"

	"github.com/tetratelabs/wazero/experimental"
)

// Linear memory of the renderer on Linux, reserved once with mmap and grown
// in place (templates.md §3.2 item 4). wazero's default grows a Go slice by
// copying it, so old and new buffers are live together and wait for the
// GC; a module near the cap then holds several times the cap. Here the cap
// is reserved as address space (MAP_NORESERVE) and pages are committed only
// when the module touches them, so the module never holds more than the cap.
//
// A freed mapping is never unmapped, so a guest call that is still running
// after its module was closed cannot fault. It goes back to a pool for the
// next instance, with two guards:
//
//   - its pages are dropped (MADV_DONTNEED) when it is freed and again when
//     it is taken, so a new instance always starts on zero memory, also when
//     a killed guest wrote into it after the free; a mapping whose pages
//     cannot be dropped is not reused;
//   - while a killed call has not returned (Renderer.within), mappings freed
//     by that kill are held back; if the call returns within killGrace they
//     join the pool, else they are never reused (the abandoned call may
//     still write into them). Such a mapping keeps its address space and
//     whatever the abandoned call writes until the process ends; a warning
//     with the count is logged each time. Not observed in tests: every
//     killed call returned at once.
//
// If mmap fails (for example with strict overcommit), the memory falls back
// to a Go slice grown by copying, as wazero's default does, and a warning is
// logged once; that memory is not bounded by the in-place growth.

var (
	mappingsMu sync.Mutex
	mappings   [][]byte // freed, clean mappings, ready for the next instance
	holding    bool     // a killed call is pending: frees go to held
	held       [][]byte
	abandoned  int // mappings never reused because their call did not return
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
	b := m.buf
	m.buf = nil
	if syscall.Madvise(b, syscall.MADV_DONTNEED) != nil {
		return // not reused; the reservation stays mapped
	}
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	if holding {
		held = append(held, b)
		return
	}
	mappings = append(mappings, b)
}

// holdFrees starts holding back the mappings freed by a kill.
func holdFrees() {
	mappingsMu.Lock()
	holding = true
	mappingsMu.Unlock()
}

// releaseFrees ends a kill: the held mappings join the pool when the
// killed call returned, else they are never reused.
func releaseFrees(returned bool) {
	mappingsMu.Lock()
	n, total := len(held), 0
	if returned {
		mappings = append(mappings, held...)
	} else {
		abandoned += n
		total = abandoned
	}
	held, holding = nil, false
	mappingsMu.Unlock()
	if !returned && n > 0 {
		warn("slide renderer: a killed call did not stop; its memory is held until the worker restarts",
			"mappings", n, "mappings_total", total)
	}
}

// growMemory is the fallback when mmap fails: a Go slice grown by copying.
type growMemory struct {
	buf []byte
	max uint64
}

func (m *growMemory) Reallocate(size uint64) []byte {
	if size > m.max {
		return nil
	}
	if size <= uint64(cap(m.buf)) {
		m.buf = m.buf[:size]
		return m.buf
	}
	nb := make([]byte, size, min(max(size, 2*uint64(cap(m.buf))), m.max))
	copy(nb, m.buf)
	m.buf = nb
	return m.buf
}

func (m *growMemory) Free() { m.buf = nil }

var mmapWarn sync.Once

func allocate(capacity, max uint64) experimental.LinearMemory {
	size := uint64(MemoryLimit)
	if max > 0 && max < size {
		size = max
	}
	mappingsMu.Lock()
	for len(mappings) > 0 {
		b := mappings[len(mappings)-1]
		mappings = mappings[:len(mappings)-1]
		if uint64(len(b)) != size {
			continue // another size: not reused
		}
		mappingsMu.Unlock()
		// Zero memory for the new instance, also if a killed guest wrote
		// into the mapping after it was freed.
		if syscall.Madvise(b, syscall.MADV_DONTNEED) == nil {
			return &mmapMemory{buf: b}
		}
		mappingsMu.Lock()
	}
	mappingsMu.Unlock()
	b, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		mmapWarn.Do(func() {
			warn("slide renderer: cannot reserve its memory with mmap; its memory is not bounded",
				"err", err.Error())
		})
		return &growMemory{buf: make([]byte, 0, min(capacity, size)), max: size}
	}
	return &mmapMemory{buf: b}
}

// withAllocator returns a context that makes wazero use the in-place
// allocator for the modules instantiated with it.
func withAllocator(ctx context.Context) context.Context {
	return experimental.WithMemoryAllocator(ctx, experimental.MemoryAllocatorFunc(allocate))
}
