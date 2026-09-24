package fsnode

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWindowsHiddenAttributeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	ptr, _ := syscall.UTF16PtrFromString(p)
	must(t, syscall.SetFileAttributes(ptr, syscall.FILE_ATTRIBUTE_HIDDEN))
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("hidden attribute accepted: %v", err)
	}
}

func TestWindowsReadOnlyRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o444))
	n, got := roundTrip(t, p)
	if Fingerprint(n) != Fingerprint(got) {
		t.Fatal("read-only file not restored")
	}
}

func TestWindowsAlternateStreamRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	must(t, os.WriteFile(p+":extra", []byte("hidden"), 0o644))
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("alternate data stream accepted: %v", err)
	}
}

func TestWindowsPlainFileAccepted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	if _, err := Capture(p); err != nil {
		t.Fatalf("plain file rejected: %v", err)
	}
}

func TestWindowsIndexingAttributeRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	ptr, err := syscall.UTF16PtrFromString(p)
	must(t, err)
	attrs, err := syscall.GetFileAttributes(ptr)
	must(t, err)
	must(t, syscall.SetFileAttributes(ptr, attrs|0x2000)) // NOT_CONTENT_INDEXED
	n, got := roundTrip(t, p)
	if n.Attrs&0x2000 == 0 || got.Attrs != n.Attrs {
		t.Fatalf("file attributes changed: before %#x, after %#x", n.Attrs, got.Attrs)
	}
}
