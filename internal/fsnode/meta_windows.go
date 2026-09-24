package fsnode

import (
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Attribute bits agentfile can recreate: read-only (via permissions),
// directory, archive, normal and not-content-indexed.
const allowedAttrs = syscall.FILE_ATTRIBUTE_READONLY | syscall.FILE_ATTRIBUTE_DIRECTORY |
	syscall.FILE_ATTRIBUTE_ARCHIVE | syscall.FILE_ATTRIBUTE_NORMAL | 0x2000

func checkPlatform(path string, fi fs.FileInfo, n *Node) error {
	if n.Type == Symlink {
		return unsupported(path, "symbolic links and reparse points are not supported on Windows")
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return unsupported(path, "no attribute information")
	}
	if d.FileAttributes&^allowedAttrs != 0 {
		return unsupported(path, "hidden, system, compressed, encrypted, sparse or reparse attribute")
	}
	n.Attrs = d.FileAttributes
	if err := checkACL(path); err != nil {
		return err
	}
	if err := checkStreams(path); err != nil {
		return err
	}
	if n.Type == File {
		links, err := linkCount(path)
		if err != nil {
			return err
		}
		if links > 1 {
			return unsupported(path, "hard-linked file")
		}
	}
	return nil
}

func linkCount(path string) (uint32, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, err
	}
	return info.NumberOfLinks, nil
}

func setPlatformMeta(path string, n *Node) error {
	if n.Attrs == 0 {
		return nil // generated content uses the filesystem's normal attributes
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs := n.Attrs &^ syscall.FILE_ATTRIBUTE_DIRECTORY
	if attrs&^syscall.FILE_ATTRIBUTE_NORMAL != 0 {
		attrs &^= syscall.FILE_ATTRIBUTE_NORMAL
	}
	if attrs == 0 {
		attrs = syscall.FILE_ATTRIBUTE_NORMAL
	}
	return syscall.SetFileAttributes(p, attrs)
}

func verifyPlatformMeta(path string, n *Node) error {
	if n.Attrs == 0 {
		return nil
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs != n.Attrs {
		return unsupported(path, "file attributes changed while recreating the entry")
	}
	return nil
}

func setTimes(path string, n *Node) error { return os.Chtimes(path, n.ModTime, n.ModTime) }

// SyncDir is a no-op: Windows does not support flushing directory handles.
func SyncDir(string) error { return nil }

// checkACL rejects explicit (non-inherited) access control entries and
// protected DACLs: a recreated entry only receives inherited entries.
func checkACL(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return unsupported(path, "cannot inspect access control list")
	}
	control, _, err := sd.Control()
	if err != nil {
		return unsupported(path, "cannot inspect access control list")
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		return unsupported(path, "access control list does not inherit from its parent")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return unsupported(path, "missing or unreadable access control list")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return unsupported(path, "unreadable access control entry")
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			return unsupported(path, "explicit access control entry")
		}
	}
	return nil
}

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procFindFirstStr = kernel32.NewProc("FindFirstStreamW")
	procFindNextStr  = kernel32.NewProc("FindNextStreamW")
)

type findStreamData struct {
	StreamSize int64
	StreamName [windows.MAX_PATH + 36]uint16
}

// checkStreams rejects alternate data streams, which agentfile does not copy.
func checkStreams(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var d findStreamData
	h, _, callErr := procFindFirstStr.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&d)), 0)
	if windows.Handle(h) == windows.InvalidHandle {
		if callErr == windows.ERROR_HANDLE_EOF {
			return nil // no streams at all (directories)
		}
		return unsupported(path, "cannot enumerate data streams")
	}
	defer windows.FindClose(windows.Handle(h))
	for {
		name := windows.UTF16ToString(d.StreamName[:])
		if name != "::$DATA" {
			return unsupported(path, "alternate data stream "+name)
		}
		r, _, callErr := procFindNextStr.Call(h, uintptr(unsafe.Pointer(&d)))
		if r == 0 {
			if callErr == windows.ERROR_HANDLE_EOF {
				return nil
			}
			return unsupported(path, "cannot enumerate data streams")
		}
	}
}
