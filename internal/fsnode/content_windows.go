package fsnode

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openContent opens the file itself, never a reparse point's target. Writers
// and deleters exclude other writers and deleters while the handle is open.
func openContent(path string, mode openMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	share := uint32(windows.FILE_SHARE_READ)
	switch mode {
	case openRead:
		share |= windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	case openWrite:
		access |= windows.GENERIC_WRITE
	case openDelete:
		access |= windows.DELETE
	}
	h, err := windows.CreateFile(p, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// checkOpened returns the opened file's volume and file index after
// rejecting reparse points, directories, read-only files, hard links and
// alternate data streams.
func checkOpened(path string, f *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	switch {
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return "", unsupported(path, "symbolic link, junction or other reparse point")
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return "", unsupported(path, "not a regular file")
	case info.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0:
		return "", unsupported(path, "read-only attribute is set; agentfile updates files in place and does not change attributes")
	case info.NumberOfLinks > 1:
		return "", unsupported(path, "hard-linked file")
	}
	if err := checkStreams(path); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x-%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

type fileDispositionInfo struct{ DeleteFile bool }

// removeOpened marks the verified file object itself for deletion; it is
// removed when the handle closes.
func removeOpened(_ string, f *os.File) error {
	d := fileDispositionInfo{DeleteFile: true}
	return windows.SetFileInformationByHandle(windows.Handle(f.Fd()), windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d)))
}

func checkDirPlatform(path string, fi fs.FileInfo) error {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok || d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a junction or other reparse point; agentfile will not write through it", path)
	}
	return nil
}
