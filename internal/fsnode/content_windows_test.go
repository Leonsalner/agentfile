package fsnode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These native cases are release gates for Windows content-only mode. A
// fixture that cannot be built fails the test; it is never skipped.

const sdInfo windows.SECURITY_INFORMATION = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION |
	windows.DACL_SECURITY_INFORMATION | windows.LABEL_SECURITY_INFORMATION

func securityString(t *testing.T, path string, info windows.SECURITY_INFORMATION) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info)
	if err != nil {
		t.Fatalf("required fixture: cannot read security descriptor of %s: %v", path, err)
	}
	return sd.String()
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// setSecurity applies the parts of sddl selected by info to path.
func setSecurity(t *testing.T, path, sddl string, info windows.SECURITY_INFORMATION) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	var owner *windows.SID
	var dacl, sacl *windows.ACL
	if info&windows.OWNER_SECURITY_INFORMATION != 0 {
		owner, _, _ = sd.Owner()
	}
	if info&windows.DACL_SECURITY_INFORMATION != 0 {
		dacl, _, _ = sd.DACL()
	}
	if info&(windows.LABEL_SECURITY_INFORMATION|windows.SACL_SECURITY_INFORMATION) != 0 {
		sacl, _, _ = sd.SACL()
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, sacl); err != nil {
		t.Fatalf("required fixture: cannot set %s on %s: %v", sddl, path, err)
	}
}

// setPrivilege returns the token to its previous state. Keep the goroutine
// on one OS thread so GetLastError reads this AdjustTokenPrivileges call.
func setPrivilege(t *testing.T, name string, enabled bool) func() {
	t.Helper()
	runtime.LockOSThread()
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("required fixture: %v", err)
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		tok.Close()
		runtime.UnlockOSThread()
		t.Fatalf("required fixture: %v", err)
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0].Luid = luid
	if enabled {
		tp.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	}
	var previous windows.Tokenprivileges
	var returned uint32
	err := windows.AdjustTokenPrivileges(tok, false, &tp, uint32(unsafe.Sizeof(previous)), &previous, &returned)
	if err == nil {
		err = windows.GetLastError() // success can still mean ERROR_NOT_ALL_ASSIGNED
	}
	if err != nil {
		tok.Close()
		runtime.UnlockOSThread()
		t.Fatalf("required fixture: cannot set %s enabled=%t: %v", name, enabled, err)
	}
	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		defer runtime.UnlockOSThread()
		defer tok.Close()
		if previous.PrivilegeCount == 0 {
			return
		}
		if err := windows.AdjustTokenPrivileges(tok, false, &previous, 0, nil, nil); err != nil {
			t.Errorf("required fixture: cannot restore %s: %v", name, err)
		} else if err := windows.GetLastError(); err != nil {
			t.Errorf("required fixture: cannot restore all %s privileges: %v", name, err)
		}
	}
}

func updateInPlace(t *testing.T, p string) {
	t.Helper()
	cur, err := CaptureContent(p)
	must(t, err)
	before, err := os.Stat(p)
	must(t, err)
	if _, err := WriteContent(p, cur.FileID, ContentFingerprint(cur), []byte("updated contents\n")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("file object replaced")
	}
}

func TestWindowsContentUpdateKeepsExplicitDACLAndLabel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	setSecurity(t, p, "D:P(A;;FA;;;"+currentUserSID(t)+")(A;;FR;;;BA)", windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setSecurity(t, p, "S:(ML;;NW;;;LW)", windows.LABEL_SECURITY_INFORMATION)
	want := securityString(t, p, sdInfo)
	updateInPlace(t, p)
	if got := securityString(t, p, sdInfo); got != want {
		t.Fatalf("security descriptor changed:\n before %s\n after  %s", want, got)
	}
}

func TestWindowsContentUpdateKeepsNondefaultOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	setSecurity(t, p, "O:BA", windows.OWNER_SECURITY_INFORMATION)
	want := securityString(t, p, sdInfo)
	updateInPlace(t, p)
	if got := securityString(t, p, sdInfo); got != want {
		t.Fatalf("owner or descriptor changed:\n before %s\n after  %s", want, got)
	}
}

func TestWindowsContentUpdateKeepsAuditSACL(t *testing.T) {
	restoreOriginal := setPrivilege(t, "SeSecurityPrivilege", false)
	defer restoreOriginal()
	disableForWrite := setPrivilege(t, "SeSecurityPrivilege", true)
	defer disableForWrite()
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	setSecurity(t, p, "S:(AU;SAFA;FW;;;WD)", windows.SACL_SECURITY_INFORMATION)
	info := sdInfo | windows.SACL_SECURITY_INFORMATION
	want := securityString(t, p, info)
	if !strings.Contains(want, "AU;") {
		t.Fatalf("required fixture: audit entry not readable back: %s", want)
	}
	disableForWrite()
	updateInPlace(t, p)
	disableAfterRead := setPrivilege(t, "SeSecurityPrivilege", true)
	defer disableAfterRead()
	if got := securityString(t, p, info); got != want {
		t.Fatalf("audit SACL or descriptor changed:\n before %s\n after  %s", want, got)
	}
}

func TestWindowsContentRefusesStreamsAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	ads := filepath.Join(dir, "ads")
	must(t, os.WriteFile(ads, []byte("x"), 0o644))
	must(t, os.WriteFile(ads+":extra", []byte("hidden"), 0o644))
	ro := filepath.Join(dir, "ro")
	must(t, os.WriteFile(ro, []byte("x"), 0o444))
	for _, p := range []string{ads, ro} {
		if _, err := CaptureContent(p); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s accepted: %v", p, err)
		}
	}
}

func TestWindowsContentAllowsHiddenAttribute(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	ptr, _ := syscall.UTF16PtrFromString(p)
	must(t, syscall.SetFileAttributes(ptr, syscall.FILE_ATTRIBUTE_HIDDEN))
	updateInPlace(t, p)
	attrs, err := syscall.GetFileAttributes(ptr)
	must(t, err)
	if attrs&syscall.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Fatal("in-place update dropped the hidden attribute")
	}
}

func TestWindowsJunctionIsNotAPlainDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	must(t, os.Mkdir(target, 0o755))
	junction := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("required fixture: mklink /J: %v: %s", err, out)
	}
	if err := CheckPlainDir(junction); err == nil {
		t.Fatal("junction accepted as a plain directory")
	}
}

func TestWindowsWriteFailureLeavesBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	cur, err := CaptureContent(p)
	must(t, err)
	other, err := os.OpenFile(p, os.O_RDWR, 0) // a concurrent writer
	must(t, err)
	defer other.Close()
	if _, err := WriteContent(p, cur.FileID, ContentFingerprint(cur), []byte("new\n")); err == nil {
		t.Fatal("write succeeded while another writer held the file")
	}
	b, err := os.ReadFile(p)
	must(t, err)
	if string(b) != "original\n" {
		t.Fatalf("bytes changed: %q", b)
	}
}
