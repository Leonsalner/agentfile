package diff

import (
	"strings"
	"testing"
)

func TestEqualIsEmpty(t *testing.T) {
	if Unified("a\n", "a\n", "x", "y") != "" {
		t.Fatal("equal inputs should produce no diff")
	}
}

func TestSingleChange(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	b := "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n"
	got := Unified(a, b, "current", "new")
	want := "--- current\n+++ new\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCreateAndDelete(t *testing.T) {
	if got := Unified("", "x\ny\n", "a", "b"); !strings.Contains(got, "@@ -1,0 +1,2 @@\n+x\n+y\n") {
		t.Fatalf("create diff wrong:\n%s", got)
	}
	if got := Unified("x\n", "", "a", "b"); !strings.Contains(got, "@@ -1,1 +1,0 @@\n-x\n") {
		t.Fatalf("delete diff wrong:\n%s", got)
	}
}

func TestSeparateHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, "line")
		b = append(b, "line")
	}
	a[2], b[2] = "old1", "new1"
	a[25], b[25] = "old2", "new2"
	got := Unified(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", "a", "b")
	if strings.Count(got, "@@ -") != 2 {
		t.Fatalf("expected two hunks:\n%s", got)
	}
}
