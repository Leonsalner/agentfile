package fsnode

import (
	"strings"
	"syscall"
)

// security.selinux labels are assigned by policy on creation; ignore them.
func xattrIgnored(name string) bool { return name == "security.selinux" }

// POSIX ACLs are stored as system.posix_acl_* attributes and are captured
// and restored like user attributes; other namespaces need privileges.
func xattrRestorable(name string) bool {
	return strings.HasPrefix(name, "user.") ||
		name == "system.posix_acl_access" || name == "system.posix_acl_default"
}

// Inode flags (chattr) are not inspected on Linux; an immutable or
// append-only target makes the write fail and the transaction roll back.
func checkFlags(string, *syscall.Stat_t) error { return nil }

func checkACL(string) error { return nil }
