package state

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Regression: on macOS new files inherit the directory's group. Writing into a
// directory owned by a group the user is not in must still verify and restore.
func TestApplyUnderForeignInheritedGroup(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "agentfile-gid-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(dir)
	fi, _ := os.Stat(dir)
	gid := int(fi.Sys().(*syscall.Stat_t).Gid)
	groups, _ := os.Getgroups()
	for _, g := range append(groups, os.Getegid()) {
		if g == gid {
			t.Skip("user is a member of the directory's group")
		}
	}
	s, err := Open(Root{Base: dir, Rel: []string{"state"}})
	must(t, err)
	p := filepath.Join(dir, "CLAUDE.md")
	m, err := s.Execute([]Change{change(t, p, file("x\n"))}, Meta{Kind: "apply"})
	if err != nil {
		t.Fatalf("apply under inherited foreign group: %v", err)
	}
	restoreFrom(t, s, m)
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("restore did not remove the created file")
	}
}
