package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// VerifyingReader hashes the bytes read through it. When the underlying reader
// ends, it compares hash and size with the expected values and returns
// ErrHashMismatch instead of io.EOF if they differ. Backends use it for both
// Put and Get.
type VerifyingReader struct {
	r    io.Reader
	want string
	size int64 // expected size, or -1 if unknown
	n    int64
	h    hash.Hash
	err  error // sticky result once the end is reached
}

// NewVerifyingReader returns a reader that checks r against sha256 and size.
// Pass size -1 to check the hash only.
func NewVerifyingReader(r io.Reader, sha256Hex string, size int64) *VerifyingReader {
	return &VerifyingReader{r: r, want: sha256Hex, size: size, h: sha256.New()}
}

func (v *VerifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	if v.size >= 0 && v.n > v.size {
		v.err = fmt.Errorf("%w: more than %d bytes", ErrHashMismatch, v.size)
		return n, v.err
	}
	if err == io.EOF {
		v.err = v.check()
		return n, v.err
	}
	return n, err
}

// check compares the bytes read so far with the expected hash and size.
func (v *VerifyingReader) check() error {
	if v.size >= 0 && v.n != v.size {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrHashMismatch, v.n, v.size)
	}
	if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
		return fmt.Errorf("%w: content does not match its sha256", ErrHashMismatch)
	}
	return io.EOF
}

// Finish reads whatever is left of the underlying reader and returns the
// verdict: nil if the bytes matched, an error wrapping ErrHashMismatch if
// not, or the read error. Backends whose upload stops after exactly size
// bytes call it to make sure the end of the stream is checked.
func (v *VerifyingReader) Finish() error {
	_, err := io.Copy(io.Discard, v)
	return err
}

// Mismatch reports whether the reader has already found a mismatch.
func (v *VerifyingReader) Mismatch() bool { return errors.Is(v.err, ErrHashMismatch) }

// readCloser pairs a reader with the Close of another value.
type readCloser struct {
	io.Reader
	io.Closer
}

// NewVerifyingReadCloser wraps rc in a VerifyingReader and keeps its Close.
func NewVerifyingReadCloser(rc io.ReadCloser, sha256Hex string, size int64) io.ReadCloser {
	return readCloser{NewVerifyingReader(rc, sha256Hex, size), rc}
}

// Verify reads r to the end and checks it against sha256Hex and size (-1 =
// any size). It returns nil, an error wrapping ErrHashMismatch, or the read
// error.
func Verify(r io.Reader, sha256Hex string, size int64) error {
	return NewVerifyingReader(r, sha256Hex, size).Finish()
}
