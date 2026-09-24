package fsnode

import (
	"bufio"
	"bytes"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

// com.apple.provenance is assigned by the kernel to every file a process
// writes; it is neither captured nor restorable, so it is ignored.
func xattrIgnored(name string) bool { return name == "com.apple.provenance" }

func xattrRestorable(name string) bool {
	return !strings.HasPrefix(name, "com.apple.system.") &&
		name != "com.apple.rootless" && name != "com.apple.macl"
}

func checkFlags(path string, st *syscall.Stat_t) error {
	if st.Flags != 0 {
		return unsupported(path, "file flags (chflags) are set")
	}
	return nil
}

var aclEntry = regexp.MustCompile(`^\s*\d+: `)

// checkACL rejects entries with an access control list. macOS exposes ACLs
// only through libc, so this reads them via ls(1) -e.
func checkACL(path string) error {
	out, err := exec.Command("/bin/ls", "-lde", "--", path).Output()
	if err != nil {
		return unsupported(path, "cannot inspect access control list")
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Scan() // the ls -l line itself
	for sc.Scan() {
		if aclEntry.MatchString(sc.Text()) {
			return unsupported(path, "access control list")
		}
	}
	return nil
}
