//go:build darwin || linux

package fsnode

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestApplyingCapturedMetadataRemovesUnexpectedXattr(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	n, err := Capture(p)
	must(t, err)
	const name = "user.agentfile-unexpected"
	must(t, unix.Lsetxattr(p, name, []byte("later"), 0))
	must(t, setPlatformMeta(p, n))
	got, err := Capture(p)
	must(t, err)
	if _, exists := got.Xattrs[name]; exists {
		t.Fatal("metadata application left an unexpected extended attribute")
	}
}

func TestShallowDirectoryCaptureDoesNotInspectChildren(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "other-skill"), 0o600); err != nil {
		t.Skipf("cannot create a special child here: %v", err)
	}
	if _, err := Capture(dir); err == nil {
		t.Fatal("full capture unexpectedly accepted a special child")
	}
	n, err := CaptureShallowDir(dir)
	if err != nil || n.Type != Dir || len(n.Children) != 0 {
		t.Fatalf("shallow capture inspected children: node=%+v err=%v", n, err)
	}
}
