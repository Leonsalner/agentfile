package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsWriteFailureRollsBackEarlierSteps(t *testing.T) {
	s, home := newStore(t)
	first, later := filepath.Join(home, "first.md"), filepath.Join(home, "later.md")
	must(t, os.WriteFile(first, []byte("first\n"), 0o644))
	must(t, os.WriteFile(later, []byte("later\n"), 0o644))
	changes := []Change{contentChange(t, first, contentFile("first managed\n")), contentChange(t, later, contentFile("later managed\n"))}
	var other *os.File
	testHook = func(stage string, i int) error {
		if stage == "started" && i == 1 {
			var err error
			other, err = os.OpenFile(later, os.O_RDWR, 0) // a concurrent writer blocks the in-place write
			return err
		}
		return nil
	}
	_, err := s.Execute(changes, contentMeta("apply"))
	testHook = nil
	if other != nil {
		other.Close()
	}
	if err == nil {
		t.Fatal("write succeeded while another writer held the file")
	}
	for p, want := range map[string]string{first: "first\n", later: "later\n"} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s: %q, want %q", p, b, want)
		}
	}
	if s.Pending() || s.Locked() {
		t.Fatal("failed apply left journal or lock")
	}
}
