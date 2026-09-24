package fsnode

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCaptureContentRecordsBytesAndIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	n, err := CaptureContent(p)
	must(t, err)
	if n.Type != Absent {
		t.Fatalf("missing file captured as %s", n.Type)
	}
	must(t, os.WriteFile(p, []byte("hello\n"), 0o644))
	n, err = CaptureContent(p)
	must(t, err)
	if n.Type != File || string(n.Data) != "hello\n" || n.FileID == "" || n.Mode != 0 || !n.ModTime.IsZero() {
		t.Fatalf("unexpected content capture %+v", n)
	}
	if ContentFingerprint(n) == ContentFingerprint(&Node{Type: File, Data: []byte("other")}) {
		t.Fatal("content fingerprint ignores bytes")
	}
	if ContentFingerprint(n) != ContentFingerprint(&Node{Type: File, Mode: 0o600, Data: []byte("hello\n")}) {
		t.Fatal("content fingerprint depends on metadata")
	}
}

func TestCaptureContentRefusesUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	must(t, os.Mkdir(sub, 0o755))
	linked := filepath.Join(dir, "linked")
	must(t, os.WriteFile(linked, []byte("x"), 0o644))
	must(t, os.Link(linked, filepath.Join(dir, "other-name")))
	cases := []string{sub, linked}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		must(t, os.Symlink(linked, link))
		cases = append(cases, link)
	}
	for _, p := range cases {
		if _, err := CaptureContent(p); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s accepted: %v", p, err)
		}
	}
}

func TestWriteContentUpdatesSameFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("a much longer original\n"), 0o640))
	before, err := os.Stat(p)
	must(t, err)
	cur, err := CaptureContent(p)
	must(t, err)
	id, err := WriteContent(p, cur.FileID, ContentFingerprint(cur), []byte("short\n"))
	must(t, err)
	after, err := os.Stat(p)
	must(t, err)
	if b, _ := os.ReadFile(p); string(b) != "short\n" {
		t.Fatalf("bytes %q", b)
	}
	if !os.SameFile(before, after) || id != cur.FileID {
		t.Fatal("file was replaced instead of updated in place")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0o640 {
		t.Errorf("permissions changed to %o", after.Mode().Perm())
	}
}

func TestWriteContentRefusesChangedTarget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	cur, err := CaptureContent(p)
	must(t, err)
	must(t, os.WriteFile(p, []byte("edited\n"), 0o644))
	if _, err := WriteContent(p, cur.FileID, ContentFingerprint(cur), []byte("new\n")); !errors.Is(err, ErrContentChanged) {
		t.Fatalf("edited target accepted: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "edited\n" {
		t.Fatal("edited target was overwritten")
	}
	repl := p + ".tmp"
	must(t, os.WriteFile(repl, []byte("original\n"), 0o644))
	must(t, os.Rename(repl, p))
	if _, err := WriteContent(p, cur.FileID, ContentFingerprint(cur), []byte("new\n")); !errors.Is(err, ErrContentChanged) {
		t.Fatalf("replaced file object accepted: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "original\n" {
		t.Fatal("replaced target was overwritten")
	}
}

func TestCreateAndRemoveContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	id, err := CreateContent(p, []byte("new\n"))
	must(t, err)
	if _, err := CreateContent(p, []byte("again\n")); !errors.Is(err, ErrContentChanged) {
		t.Fatal("create replaced an existing file")
	}
	n, err := CaptureContent(p)
	must(t, err)
	if n.FileID != id || string(n.Data) != "new\n" {
		t.Fatalf("created file %+v, id %s", n, id)
	}
	if err := RemoveContent(p, id, ContentFingerprint(&Node{Type: File, Data: []byte("other")})); !errors.Is(err, ErrContentChanged) {
		t.Fatalf("remove ignored changed bytes: %v", err)
	}
	must(t, RemoveContent(p, id, ContentFingerprint(n)))
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file not removed")
	}
}

func TestFailedCreateCleanupKeepsReplacementFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	id, err := CreateContent(p, []byte("created\n"))
	must(t, err)
	replacement := p + ".new"
	must(t, os.WriteFile(replacement, []byte("someone else's file\n"), 0o644))
	must(t, os.Remove(p))
	must(t, os.Rename(replacement, p))
	if err := removeCreatedIfSame(p, id); !errors.Is(err, ErrContentChanged) {
		t.Fatalf("cleanup accepted a replacement file: %v", err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "someone else's file\n" {
		t.Fatalf("cleanup removed or changed the replacement: %q, %v", b, err)
	}
}

func TestCheckPlainDir(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	must(t, os.WriteFile(f, nil, 0o644))
	must(t, CheckPlainDir(dir))
	if err := CheckPlainDir(f); err == nil {
		t.Error("file accepted as a directory")
	}
	if err := CheckPlainDir(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		must(t, os.Symlink(dir, link))
		if err := CheckPlainDir(link); err == nil {
			t.Error("symlinked directory accepted")
		}
	}
}
