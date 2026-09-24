package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Native release gates: fixtures that cannot be built fail, never skip.

func descriptor(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|
		windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("required fixture: %v", err)
	}
	return sd.String()
}

func TestWindowsApplyAndRestoreKeepExplicitDescriptor(t *testing.T) {
	f := newFixture(t)
	claudeMD := filepath.Join(f.home, ".claude", "CLAUDE.md")
	must(t, os.MkdirAll(filepath.Dir(claudeMD), 0o755))
	must(t, os.WriteFile(claudeMD, []byte("mine\n"), 0o644))
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	must(t, err)
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + u.User.Sid.String() + ")(A;;FR;;;BA)S:(ML;;NW;;;LW)")
	must(t, err)
	dacl, _, err := sd.DACL()
	must(t, err)
	label, _, err := sd.SACL()
	must(t, err)
	if err := windows.SetNamedSecurityInfo(claudeMD, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, dacl, label); err != nil {
		t.Fatalf("required fixture: %v", err)
	}
	want := descriptor(t, claudeMD)
	m := install(t, f, claudeOnly, replaceAll)
	if got := descriptor(t, claudeMD); got != want {
		t.Fatalf("apply changed the descriptor:\n before %s\n after  %s", want, got)
	}
	restore(t, f, m.ID, nil)
	if b, _ := os.ReadFile(claudeMD); string(b) != "mine\n" {
		t.Fatalf("restore bytes %q", b)
	}
	if got := descriptor(t, claudeMD); got != want {
		t.Fatalf("restore changed the descriptor:\n before %s\n after  %s", want, got)
	}
}

func TestWindowsUnsupportedSkillTargetsBlocked(t *testing.T) {
	f := newFixture(t)
	ads := contentSkill(f, "routing")
	must(t, os.MkdirAll(filepath.Dir(ads), 0o755))
	must(t, os.WriteFile(ads, []byte("x"), 0o644))
	must(t, os.WriteFile(ads+":extra", []byte("hidden"), 0o644))
	ro := contentSkill(f, "handoff")
	must(t, os.MkdirAll(filepath.Dir(ro), 0o755))
	must(t, os.WriteFile(ro, []byte("x"), 0o444))
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	for _, p := range []string{ads, ro} {
		if it := findItem(t, plan, p); it.Blocked == "" {
			t.Errorf("%s was not blocked", p)
		}
	}
}

func TestWindowsJunctionedSkillDirectoryBlocked(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	named := filepath.Dir(contentSkill(f, "routing"))
	must(t, os.MkdirAll(filepath.Dir(named), 0o755))
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", named, outside).CombinedOutput(); err != nil {
		t.Fatalf("required fixture: mklink /J: %v: %s", err, out)
	}
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	for i := range plan.Items {
		if plan.Items[i].Blocked == "" {
			t.Errorf("%s not blocked by a junctioned skill directory", plan.Items[i].Path)
		}
		plan.Items[i].Replace = true
	}
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatal("wrote through a junction")
	}
}
