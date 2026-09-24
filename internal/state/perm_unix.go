//go:build darwin || linux

package state

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// restrictToOwner requires the directory to belong to the current user and
// removes any group or other access.
func restrictToOwner(path string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("state path %s is not owned by the current user", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("cannot restrict %s to the current user: %w", path, err)
		}
	}
	return nil
}
