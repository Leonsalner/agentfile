package fsnode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func roundTrip(t *testing.T, src string) (*Node, *Node) {
	t.Helper()
	n, err := Capture(src)
	must(t, err)
	dst := filepath.Join(t.TempDir(), "copy")
	must(t, Write(dst, n))
	got, err := Capture(dst)
	must(t, err)
	return n, got
}

func TestRoundTripTreeModesTimes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skill")
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))
	must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("hello\n"), 0o640))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "ro.txt"), []byte("ro"), 0o444))
	must(t, os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600))
	if runtime.GOOS != "windows" {
		must(t, os.Symlink("../SKILL.md", filepath.Join(dir, "sub", "link")))
	}
	old := time.Date(2023, 5, 6, 7, 8, 9, 0, time.UTC)
	for _, p := range []string{filepath.Join(dir, "SKILL.md"), filepath.Join(dir, "sub"), dir} {
		must(t, os.Chtimes(p, old, old))
	}
	n, got := roundTrip(t, dir)
	if Fingerprint(n) != Fingerprint(got) {
		t.Fatal("fingerprint changed across round trip")
	}
	if !got.ModTime.Equal(old) || !got.Children["sub"].ModTime.Equal(old) || !got.Children["SKILL.md"].ModTime.Equal(old) {
		t.Error("modification times not restored")
	}
	if runtime.GOOS != "windows" {
		if got.Children["sub"].Mode != 0o750 || got.Children["SKILL.md"].Mode != 0o640 {
			t.Error("permissions not restored")
		}
		if got.Children["sub"].Children["link"].Target != "../SKILL.md" {
			t.Error("symlink target not preserved verbatim")
		}
	}
}

func TestAbsent(t *testing.T) {
	n, err := Capture(filepath.Join(t.TempDir(), "missing"))
	must(t, err)
	if n.Type != Absent {
		t.Fatal("missing path should capture as absent")
	}
}

func TestDanglingSymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are rejected on Windows")
	}
	p := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink("/nonexistent/target", p))
	n, got := roundTrip(t, p)
	if n.Type != Symlink || got.Target != "/nonexistent/target" {
		t.Fatal("dangling symlink not captured as a link")
	}
}

func TestHardLinkRejected(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	must(t, os.WriteFile(a, []byte("x"), 0o644))
	must(t, os.Link(a, filepath.Join(dir, "b")))
	if _, err := Capture(a); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("hard link accepted: %v", err)
	}
}

func TestSpecialFileRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo on Windows")
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if err := exec.Command("mkfifo", p).Run(); err != nil {
		t.Skip("mkfifo unavailable")
	}
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("fifo accepted: %v", err)
	}
}

func TestSetuidRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no setuid on Windows")
	}
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, nil, 0o644))
	must(t, os.Chmod(p, 0o644|os.ModeSetgid))
	if fi, _ := os.Stat(p); fi.Mode()&os.ModeSetgid == 0 {
		t.Skip("filesystem ignored setgid")
	}
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("setgid accepted: %v", err)
	}
}

func TestFingerprintIgnoresTimes(t *testing.T) {
	a := &Node{Type: File, Mode: 0o644, Data: []byte("x"), ModTime: time.Unix(1, 0)}
	b := &Node{Type: File, Mode: 0o644, Data: []byte("x"), ModTime: time.Unix(2, 0)}
	c := &Node{Type: File, Mode: 0o600, Data: []byte("x")}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("times affect fingerprint")
	}
	if Fingerprint(a) == Fingerprint(c) {
		t.Error("permissions do not affect fingerprint")
	}
}
