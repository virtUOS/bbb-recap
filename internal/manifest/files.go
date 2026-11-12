package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// Roles of package files (§5).
const (
	RoleEvents       = "events"
	RoleMicrophone   = "audio-microphone"
	RoleNotes        = "notes"
	RolePresentation = "presentation"
)

// RoleOf returns the role of a path relative to raw/<record_id>/ under the
// allow-list of §5, or "" if the file is not part of a package. It looks at
// the path only.
func RoleOf(p string) string {
	switch {
	case p == "events.xml":
		return RoleEvents
	case strings.HasPrefix(p, "audio/microphone-") && strings.HasSuffix(p, ".webm") &&
		len(p) > len("audio/microphone-.webm") && !strings.Contains(p[len("audio/"):], "/"):
		return RoleMicrophone
	case p == "notes/notes.html", p == "notes/notes.md", p == "notes/notes.txt", p == "notes/export_failed":
		return RoleNotes
	case strings.HasPrefix(p, "presentation/") && len(p) > len("presentation/"):
		return RolePresentation
	}
	return ""
}

// ValidPath reports whether p is a manifest path (§4): relative,
// /-separated, no empty, "." or ".." segment, no backslash, no control
// characters, valid UTF-8, at most 1024 bytes.
func ValidPath(p string) bool {
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || strings.Contains(p, `\`) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Selection is the result of Select.
type Selection struct {
	Files []File // sorted by path, bytewise
	// Skipped counts entries in the selected areas that are not regular
	// files (symlinks, devices, sockets, …) or whose names are not valid
	// manifest paths. They are never followed or read.
	Skipped int
}

// Select applies the allow-list of §5 to a raw recording directory
// raw/<record_id>/: events.xml, audio/microphone-*.webm, the fixed notes
// files and everything below presentation/. Only regular files are taken;
// it reads them to compute size and SHA-256. It never follows a path out of
// dir.
func Select(dir string) (*Selection, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	sel := &Selection{}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if p == "audio" || p == "notes" || p == "presentation" || strings.HasPrefix(p, "presentation/") {
				return nil
			}
			return fs.SkipDir
		}
		if !d.Type().IsRegular() || !ValidPath(p) {
			sel.Skipped++
			return nil
		}
		role := RoleOf(p)
		if role == "" {
			return nil
		}
		f, err := hashFile(root, p)
		if err != nil {
			return err
		}
		f.Role = role
		sel.Files = append(sel.Files, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sel.Files, func(i, j int) bool { return sel.Files[i].Path < sel.Files[j].Path })
	return sel, nil
}

// hashFile reads a regular file below root and returns its path, size and
// SHA-256.
func hashFile(root *os.Root, p string) (File, error) {
	fh, err := root.Open(p)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = fh.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, fh)
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", p, err)
	}
	return File{Path: p, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// CheckFile is V-I05 for one file: r must yield exactly f.Size bytes with
// SHA-256 f.SHA256. A mismatch is a violation (422 file-hash-mismatch); a
// read error is returned as error.
func CheckFile(r io.Reader, f File) (*Violation, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, f.Size+1))
	if err != nil {
		return nil, err
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return violation("V-I05", ProblemFileHashMismatch, 422, "file %q does not match its size and sha256", f.Path), nil
	}
	return nil, nil
}

// CheckFiles applies CheckFile to each file at its manifest path below dir.
// A missing file or one that is not a regular file is a violation too. Paths
// that would leave dir are never opened.
func CheckFiles(dir string, files []File) (*Violation, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, f := range files {
		if !ValidPath(f.Path) {
			return violation("V-I05", ProblemFileHashMismatch, 422, "file %q: not a valid path", f.Path), nil
		}
		fi, err := root.Lstat(f.Path)
		if err != nil || !fi.Mode().IsRegular() {
			return violation("V-I05", ProblemFileHashMismatch, 422, "file %q: missing or not a regular file", f.Path), nil
		}
		v, err := checkOne(root, f)
		if v != nil || err != nil {
			return v, err
		}
	}
	return nil, nil
}

func checkOne(root *os.Root, f File) (*Violation, error) {
	fh, err := root.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	return CheckFile(fh, f)
}
