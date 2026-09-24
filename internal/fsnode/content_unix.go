//go:build darwin || linux

package fsnode

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func openContent(path string, mode openMode) (*os.File, error) {
	flag := os.O_RDONLY
	if mode == openWrite {
		flag = os.O_RDWR
	}
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, 0)
}

// checkOpened returns the opened file's identity after requiring a regular
// file with a single link.
func checkOpened(path string, f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() {
		return "", unsupported(path, "not a regular file")
	}
	if st.Nlink > 1 {
		return "", unsupported(path, "hard-linked file")
	}
	return fmt.Sprintf("%d-%d", st.Dev, st.Ino), nil
}

// removeOpened unlinks path after checking it still names the opened file.
func removeOpened(path string, f *os.File) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	cur, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, cur) {
		return changed(path)
	}
	return os.Remove(path)
}

func checkDirPlatform(string, fs.FileInfo) error { return nil }
