package fsnode

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxUserXattrRestored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	if err := unix.Lsetxattr(p, "user.agentfile-test", []byte("v"), 0); err != nil {
		t.Skipf("user xattrs unsupported on this filesystem: %v", err)
	}
	_, got := roundTrip(t, p)
	if string(got.Xattrs["user.agentfile-test"]) != "v" {
		t.Error("user extended attribute not restored")
	}
}

func TestLinuxInheritedACLRemovedWhenOriginalHasNone(t *testing.T) {
	parent := t.TempDir()
	acl := make([]byte, 4)
	binary.LittleEndian.PutUint32(acl, 2) // POSIX_ACL_XATTR_VERSION
	for _, entry := range []struct {
		tag, perm uint16
		id        uint32
	}{
		{1, 7, ^uint32(0)},           // ACL_USER_OBJ
		{2, 6, uint32(os.Geteuid())}, // named ACL_USER
		{4, 5, ^uint32(0)},           // ACL_GROUP_OBJ
		{16, 5, ^uint32(0)},          // ACL_MASK
		{32, 5, ^uint32(0)},          // ACL_OTHER
	} {
		acl = binary.LittleEndian.AppendUint16(acl, entry.tag)
		acl = binary.LittleEndian.AppendUint16(acl, entry.perm)
		acl = binary.LittleEndian.AppendUint32(acl, entry.id)
	}
	if err := unix.Lsetxattr(parent, "system.posix_acl_default", acl, 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skip("filesystem does not support POSIX ACLs")
		}
		t.Fatal(err)
	}
	source := filepath.Join(parent, "source")
	must(t, os.WriteFile(source, []byte("x"), 0o644))
	if _, err := unix.Lgetxattr(source, "system.posix_acl_access", nil); err != nil {
		t.Skip("filesystem did not materialize an inherited access ACL")
	}
	must(t, unix.Lremovexattr(source, "system.posix_acl_access"))
	n, err := Capture(source)
	must(t, err)
	if _, exists := n.Xattrs["system.posix_acl_access"]; exists {
		t.Fatal("source still has an access ACL")
	}
	destination := filepath.Join(parent, "copy")
	must(t, Write(destination, n))
	got, err := Capture(destination)
	must(t, err)
	if _, exists := got.Xattrs["system.posix_acl_access"]; exists {
		t.Fatal("copy inherited an ACL absent from the original")
	}
}
