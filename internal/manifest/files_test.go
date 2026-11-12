package manifest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Select on the F-01 raw directory gives exactly the golden file list
// (CT-PKG-03): no video/, deskshare/, learning-dashboard/, screen-share
// audio, notes.pdf or symlink.
func TestSelectF01(t *testing.T) {
	m, v := Check(readFile(t, f01Golden), Options{})
	if v != nil {
		t.Fatal(v)
	}
	sel, err := Select(f01RawDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Files) != len(m.Files) {
		t.Fatalf("selected %d files, want %d:\n%+v", len(sel.Files), len(m.Files), sel.Files)
	}
	for i := range m.Files {
		if sel.Files[i] != m.Files[i] {
			t.Errorf("files[%d] = %+v, want %+v", i, sel.Files[i], m.Files[i])
		}
	}
	if sel.Skipped != 1 { // audio/link-to-elsewhere.webm
		t.Errorf("Skipped = %d, want 1", sel.Skipped)
	}
}

// Select never follows a symlink, also not one that points out of the
// directory or one in place of a directory, and counts what it skips.
func TestSelectSkipsNonRegular(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.webm"), "outside")
	write(t, filepath.Join(outside, "slides", "s.svg"), "outside")

	dir := t.TempDir()
	write(t, filepath.Join(dir, "events.xml"), "<recording/>")
	write(t, filepath.Join(dir, "audio", "microphone-u1-t1-1.webm"), "a")
	write(t, filepath.Join(dir, "audio", "sub", "microphone-u2-t2-2.webm"), "nested: not selected")
	write(t, filepath.Join(dir, "presentation", "deck", "p.pdf"), "pdf")
	write(t, filepath.Join(dir, "notes", "notes.pdf"), "pdf: not selected")
	write(t, filepath.Join(dir, "notes", "export_failed"), "")
	symlink(t, filepath.Join(outside, "secret.webm"), filepath.Join(dir, "audio", "microphone-u3-t3-3.webm"))
	symlink(t, filepath.Join(outside, "slides"), filepath.Join(dir, "presentation", "linked"))
	if err := syscall.Mkfifo(filepath.Join(dir, "presentation", "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}

	sel, err := Select(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []File{
		{"audio/microphone-u1-t1-1.webm", RoleMicrophone, 1, "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"},
		{"events.xml", RoleEvents, 12, ""},
		{"notes/export_failed", RoleNotes, 0, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"presentation/deck/p.pdf", RolePresentation, 3, ""},
	}
	if len(sel.Files) != len(want) {
		t.Fatalf("selected %+v", sel.Files)
	}
	for i, w := range want {
		g := sel.Files[i]
		if g.Path != w.Path || g.Role != w.Role || g.Size != w.Size || len(g.SHA256) != 64 {
			t.Errorf("files[%d] = %+v, want %+v", i, g, w)
		}
	}
	if sel.Files[0].SHA256 != want[0].SHA256 || sel.Files[2].SHA256 != want[2].SHA256 {
		t.Errorf("hashes: %+v", sel.Files)
	}
	if sel.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3 (two symlinks, one fifo)", sel.Skipped)
	}
}

func TestRoleOf(t *testing.T) {
	for p, want := range map[string]string{
		"events.xml":                              RoleEvents,
		"audio/microphone-w_a-TR_b-1.webm":        RoleMicrophone,
		"audio/microphone-.webm":                  "",
		"audio/x/microphone-a.webm":               "",
		"audio/screen_share_audio-a-b-1.webm":     "",
		"audio/microphone-a.opus":                 "",
		"notes/notes.html":                        RoleNotes,
		"notes/notes.md":                          RoleNotes,
		"notes/notes.txt":                         RoleNotes,
		"notes/notes.pdf":                         "",
		"notes/export_failed":                     RoleNotes,
		"presentation/a/b/c.svg":                  RolePresentation,
		"presentation/":                           "",
		"video/x.webm":                            "",
		"deskshare/x.webm":                        "",
		"learning-dashboard/data.json":            "",
		"sub/events.xml":                          "",
		"presentation/../audio/microphone-a.webm": RolePresentation, // ValidPath rejects it
	} {
		if got := RoleOf(p); got != want {
			t.Errorf("RoleOf(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestValidPath(t *testing.T) {
	for p, want := range map[string]bool{
		"events.xml": true, "a/b/c": true, "": false, "/abs": false, "a//b": false, "a/": false,
		"./a": false, "a/../b": false, "..": false, `a\b`: false, "a\x01b": false, "a\x7fb": false,
		"bad\xffutf8": false, string(make([]byte, 1025)): false,
	} {
		if got := ValidPath(p); got != want {
			t.Errorf("ValidPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
