package fsnode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinXattrsRestoredProvenanceIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	must(t, unix.Lsetxattr(p, "user.agentfile-test", []byte("v"), 0))
	n, got := roundTrip(t, p)
	if _, ok := n.Xattrs["com.apple.provenance"]; ok {
		t.Error("com.apple.provenance should be ignored")
	}
	if string(got.Xattrs["user.agentfile-test"]) != "v" {
		t.Error("user extended attribute not restored")
	}
}

func TestDarwinACLRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	if err := exec.Command("/bin/chmod", "+a", "everyone allow read", p).Run(); err != nil {
		t.Skip("cannot set ACL here")
	}
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ACL accepted: %v", err)
	}
}

func TestDarwinFlagsRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	if err := exec.Command("/usr/bin/chflags", "hidden", p).Run(); err != nil {
		t.Skip("chflags unavailable")
	}
	if _, err := Capture(p); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("file flags accepted: %v", err)
	}
}
