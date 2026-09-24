// Package fsnode captures a managed path (file, symlink, or directory tree)
// into memory without following links, and recreates it exactly. Capture
// rejects any present property it cannot faithfully recreate on this OS.
package fsnode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Type string

const (
	Absent  Type = "absent"
	File    Type = "file"
	Dir     Type = "dir"
	Symlink Type = "symlink"
)

// Node is a captured or desired filesystem entry.
type Node struct {
	Type     Type              `json:"type"`
	Mode     fs.FileMode       `json:"mode,omitempty"` // permission bits only
	Data     []byte            `json:"data,omitempty"`
	Target   string            `json:"target,omitempty"` // symlink destination, verbatim
	Children map[string]*Node  `json:"children,omitempty"`
	ModTime  time.Time         `json:"mtime,omitzero"`
	Xattrs   map[string][]byte `json:"xattrs,omitempty"`
	Gid      *int              `json:"gid,omitempty"`     // owning group, where recorded
	Attrs    uint32            `json:"attrs,omitempty"`   // restorable Windows file attributes
	FileID   string            `json:"file_id,omitempty"` // content-only mode: identity of the file object
}

// ErrUnsupported marks a present property that cannot be restored faithfully.
var ErrUnsupported = errors.New("unsupported for faithful restore")

func unsupported(path, why string) error {
	return fmt.Errorf("%s: %s: %w", path, why, ErrUnsupported)
}

// Capture reads path into a Node. A missing path yields an Absent node.
func Capture(path string) (*Node, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Node{Type: Absent}, nil
	}
	if err != nil {
		return nil, err
	}
	return capture(path, fi)
}

// CaptureShallowDir reads a directory's own restorable metadata without
// enumerating its children. Parent cleanup uses this for agent homes and
// skills directories, which may contain unrelated user entries.
func CaptureShallowDir(path string) (*Node, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	m := fi.Mode()
	if !m.IsDir() || m&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return nil, unsupported(path, "not a restorable plain directory")
	}
	n := &Node{Type: Dir, Mode: m.Perm(), ModTime: fi.ModTime(), Children: map[string]*Node{}}
	if err := checkPlatform(path, fi, n); err != nil {
		return nil, err
	}
	return n, nil
}

func capture(path string, fi fs.FileInfo) (*Node, error) {
	m := fi.Mode()
	if m&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return nil, unsupported(path, "setuid, setgid or sticky bit")
	}
	n := &Node{Mode: m.Perm(), ModTime: fi.ModTime()}
	switch {
	case m.IsRegular():
		n.Type = File
	case m.IsDir():
		n.Type = Dir
	case m&fs.ModeSymlink != 0:
		n.Type = Symlink
		n.Mode = 0 // link permission bits are not meaningful or portable
	default:
		return nil, unsupported(path, "not a regular file, directory or symbolic link")
	}
	if err := checkPlatform(path, fi, n); err != nil {
		return nil, err
	}
	switch n.Type {
	case File:
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		n.Data = b
	case Symlink:
		t, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		n.Target = t
	case Dir:
		ents, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		n.Children = map[string]*Node{}
		for _, e := range ents {
			cfi, err := os.Lstat(filepath.Join(path, e.Name()))
			if err != nil {
				return nil, err
			}
			c, err := capture(filepath.Join(path, e.Name()), cfi)
			if err != nil {
				return nil, err
			}
			n.Children[e.Name()] = c
		}
	}
	return n, nil
}

// Write materializes n at path, which must not exist. Metadata (extended
// attributes, group, times) is applied after contents, directories last.
func Write(path string, n *Node) error {
	switch n.Type {
	case Absent:
		return nil
	case File:
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(n.Data); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	case Symlink:
		if err := os.Symlink(n.Target, path); err != nil {
			return err
		}
	case Dir:
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		for _, name := range sortedNames(n) {
			if err := Write(filepath.Join(path, name), n.Children[name]); err != nil {
				return err
			}
		}
		if err := SyncDir(path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s: cannot write node type %q", path, n.Type)
	}
	return applyMeta(path, n)
}

func applyMeta(path string, n *Node) error {
	if err := setPlatformMeta(path, n); err != nil {
		return err
	}
	if n.Type != Symlink {
		if err := os.Chmod(path, n.Mode); err != nil {
			return err
		}
	}
	if err := verifyPlatformMeta(path, n); err != nil {
		return err
	}
	if !n.ModTime.IsZero() {
		return setTimes(path, n)
	}
	return nil
}

// ApplyMeta restores only n's metadata on an existing entry.
func ApplyMeta(path string, n *Node) error { return applyMeta(path, n) }

func sortedNames(n *Node) []string {
	names := make([]string, 0, len(n.Children))
	for k := range n.Children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Fingerprint identifies an entry's type, permissions, contents and link
// targets. Times, extended attributes and group are excluded so that a
// touched-but-unchanged file does not count as a user edit.
func Fingerprint(n *Node) string {
	h := sha256.New()
	var walk func(prefix string, n *Node)
	walk = func(prefix string, n *Node) {
		fmt.Fprintf(h, "%s\x00%s\x00%o\x00", prefix, n.Type, n.Mode)
		switch n.Type {
		case File:
			d := sha256.Sum256(n.Data)
			h.Write(d[:])
		case Symlink:
			h.Write([]byte(n.Target))
		case Dir:
			for _, name := range sortedNames(n) {
				walk(prefix+"/"+name, n.Children[name])
			}
		}
		h.Write([]byte{0})
	}
	walk("", n)
	return hex.EncodeToString(h.Sum(nil))
}

// RestoreFingerprint also covers metadata that Restore can recreate. Apply
// keeps using Fingerprint so timestamps alone do not cause an update.
func RestoreFingerprint(n *Node) string {
	h := sha256.New()
	h.Write([]byte(Fingerprint(n)))
	var walk func(*Node)
	walk = func(n *Node) {
		fmt.Fprintf(h, "%s\x00%d\x00", n.ModTime.UTC().Format(time.RFC3339Nano), n.Attrs)
		if n.Gid == nil {
			h.Write([]byte("gid:nil\x00"))
		} else {
			fmt.Fprintf(h, "gid:%d\x00", *n.Gid)
		}
		names := make([]string, 0, len(n.Xattrs))
		for name := range n.Xattrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(h, "%s\x00%d\x00", name, len(n.Xattrs[name]))
			h.Write(n.Xattrs[name])
			h.Write([]byte{0})
		}
		h.Write([]byte{0})
		for _, name := range sortedNames(n) {
			walk(n.Children[name])
		}
	}
	walk(n)
	return hex.EncodeToString(h.Sum(nil))
}

// Size is the total byte size of file contents in n.
func Size(n *Node) int64 {
	var s int64
	s += int64(len(n.Data))
	for _, c := range n.Children {
		s += Size(c)
	}
	return s
}

// Summary lists every entry below n as "path type hash" lines, for the
// preview of non-Markdown content.
func Summary(n *Node) []string {
	var out []string
	var walk func(prefix string, n *Node)
	walk = func(prefix string, n *Node) {
		line := fmt.Sprintf("%-40s %-7s %04o", prefix, n.Type, n.Mode)
		switch n.Type {
		case File:
			d := sha256.Sum256(n.Data)
			line += fmt.Sprintf(" sha256:%s %dB", hex.EncodeToString(d[:6]), len(n.Data))
		case Symlink:
			line += " -> " + n.Target
		}
		out = append(out, strings.TrimRight(line, " "))
		for _, name := range sortedNames(n) {
			walk(prefix+"/"+name, n.Children[name])
		}
	}
	walk(".", n)
	return out
}

// RemoveAll removes path without following a final symlink.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Canonical returns a copy of n with permission bits as this OS reports
// them, so desired and captured nodes fingerprint alike. On Windows only the
// read-only attribute exists: files read back as 0444 or 0666, directories
// additionally carry 0111.
func Canonical(n *Node) *Node {
	c := *n
	if runtime.GOOS == "windows" && c.Type != Absent && c.Type != Symlink {
		m := fs.FileMode(0o666)
		if c.Mode&0o200 == 0 {
			m = 0o444
		}
		if c.Type == Dir {
			m |= 0o111
		}
		c.Mode = m
	}
	if c.Children != nil {
		c.Children = make(map[string]*Node, len(n.Children))
		for k, v := range n.Children {
			c.Children[k] = Canonical(v)
		}
	}
	return &c
}
