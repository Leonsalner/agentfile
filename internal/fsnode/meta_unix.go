//go:build darwin || linux

package fsnode

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkPlatform(path string, fi fs.FileInfo, n *Node) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return unsupported(path, "no ownership information")
	}
	if int(st.Uid) != os.Geteuid() {
		return unsupported(path, "owned by another user")
	}
	if n.Type == File && st.Nlink > 1 {
		return unsupported(path, "hard-linked file")
	}
	gid := int(st.Gid)
	if !memberOf(gid) && gid != inheritedGid(path) {
		return unsupported(path, "group is neither one of the current user's groups nor inherited from its directory")
	}
	n.Gid = &gid
	if err := checkFlags(path, st); err != nil {
		return err
	}
	if err := checkACL(path); err != nil {
		return err
	}
	x, err := readXattrs(path)
	if err != nil {
		return err
	}
	n.Xattrs = x
	return nil
}

func memberOf(gid int) bool {
	if gid == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// inheritedGid is the group a new entry created next to path receives:
// the directory's group on macOS or under a setgid directory, otherwise the
// process's effective group.
func inheritedGid(path string) int {
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return -1
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	if runtime.GOOS == "darwin" || fi.Mode()&fs.ModeSetgid != 0 {
		return int(st.Gid)
	}
	return os.Getegid()
}

func readXattrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if name == "" || xattrIgnored(name) {
			continue
		}
		if !xattrRestorable(name) {
			return nil, unsupported(path, "extended attribute "+name)
		}
		vsize, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		val := make([]byte, vsize)
		if vsize > 0 {
			if vsize, err = unix.Lgetxattr(path, name, val); err != nil {
				return nil, err
			}
		}
		out[name] = val[:vsize]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func setPlatformMeta(path string, n *Node) error {
	current, err := readXattrs(path)
	if err != nil {
		return err
	}
	for name := range current {
		if _, keep := n.Xattrs[name]; !keep {
			if err := unix.Lremovexattr(path, name); err != nil {
				return &fs.PathError{Op: "removexattr " + name, Path: path, Err: err}
			}
		}
	}
	for name, val := range n.Xattrs {
		if err := unix.Lsetxattr(path, name, val, 0); err != nil {
			return &fs.PathError{Op: "setxattr " + name, Path: path, Err: err}
		}
	}
	if n.Gid != nil {
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Gid) != *n.Gid {
			if err := os.Lchown(path, -1, *n.Gid); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyPlatformMeta(path string, n *Node) error {
	got, err := readXattrs(path)
	if err != nil {
		return err
	}
	if len(got) != len(n.Xattrs) {
		return unsupported(path, "extended attributes changed while recreating the entry")
	}
	for name, want := range n.Xattrs {
		if !bytes.Equal(got[name], want) {
			return unsupported(path, "extended attribute "+name+" changed while recreating the entry")
		}
	}
	if n.Gid != nil {
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Gid) != *n.Gid {
			return unsupported(path, "group changed while recreating the entry")
		}
	}
	return nil
}

func setTimes(path string, n *Node) error {
	ts := unix.NsecToTimespec(n.ModTime.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

// SyncDir flushes a directory's entries to stable storage.
func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
