package slideimage

import (
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// resident counts the pages of b that are in memory (mincore).
func resident(t *testing.T, b []byte) int {
	t.Helper()
	vec := make([]byte, (len(b)+4095)/4096)
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		t.Fatalf("mincore: %v", e)
	}
	n := 0
	for _, v := range vec {
		if v&1 != 0 {
			n++
		}
	}
	return n
}

// After a time-limit kill the killed guest may still write into its memory
// after it was freed; the next instance must still start on zero memory
// (alloc_linux.go): a mapping taken from the pool has no resident page.
func TestPooledMappingCleanAfterKill(t *testing.T) {
	r := New()
	defer r.Close()
	pdf := f14File(t, chartDeck, "pdf")
	for i := range 40 {
		if _, err := r.OpenPDF(pdf); err != nil {
			t.Fatal(i, err)
		}
		inst, doc := r.inst, *r.doc
		_, _ = within(r, time.Duration(3+i%20)*time.Millisecond, func() (int, error) {
			for i := 0; ; i++ {
				if _, err := renderPage(inst, doc, i%3); err != nil {
					return i, err
				}
			}
		})
		m := allocate(0, MemoryLimit)
		mm, ok := m.(*mmapMemory)
		if !ok {
			t.Skip("mmap not available: fallback memory in use")
		}
		if n := resident(t, mm.buf); n != 0 {
			t.Fatalf("kill %d: a mapping from the pool has %d resident pages, want 0", i, n)
		}
		m.Free()
	}
}

// A mapping freed by a kill whose call did not return within killGrace is
// never reused; one whose call returned joins the pool.
func TestAbandonedMappingNotReused(t *testing.T) {
	m := allocate(0, MemoryLimit).(*mmapMemory)
	buf := m.buf
	mappingsMu.Lock()
	before := abandoned
	mappingsMu.Unlock()
	holdFrees()
	m.Free()
	releaseFrees(false)
	mappingsMu.Lock()
	if abandoned != before+1 {
		t.Errorf("abandoned = %d, want %d", abandoned, before+1)
	}
	for _, b := range mappings {
		if &b[0] == &buf[0] {
			t.Error("the mapping of an abandoned call is in the pool")
		}
	}
	mappingsMu.Unlock()

	m = allocate(0, MemoryLimit).(*mmapMemory)
	buf = m.buf
	holdFrees()
	m.Free()
	releaseFrees(true)
	found := false
	mappingsMu.Lock()
	for _, b := range mappings {
		found = found || &b[0] == &buf[0]
	}
	mappingsMu.Unlock()
	if !found {
		t.Error("the mapping of a call that returned is not in the pool")
	}
}
