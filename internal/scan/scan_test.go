// Package scan checks that shareable files carry no personal machine paths,
// account labels or machine-only helpers. Generic patterns live here; names
// that must not appear in a public repo are supplied privately through
// AGENTFILE_PRIVATE_DENYLIST (a file with one case-insensitive whole-word term per line).
package scan

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agentfile/internal/profile"
	"agentfile/internal/render"
)

var generic = []*regexp.Regexp{
	regexp.MustCompile(`/Users/[A-Za-z0-9_.-]+`),
	regexp.MustCompile(`/home/[a-z0-9_.-]+/`),
	regexp.MustCompile(`(?i)C:\\Users\\[A-Za-z0-9_.-]+`),
	regexp.MustCompile(`CODEX_HOME=`),
	regexp.MustCompile(`(?i)codexbar`),
	regexp.MustCompile(`\.codex-[a-z]`),
	regexp.MustCompile(`\bcodex2\b`),
	regexp.MustCompile(`\brtk\b`),
	regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z.]{2,}`),
}

// allowed matches legitimate generic text, e.g. the attribution address.
var allowed = regexp.MustCompile(`noreply@anthropic\.com`)

func privateTerms(t *testing.T) []*regexp.Regexp {
	p := os.Getenv("AGENTFILE_PRIVATE_DENYLIST")
	if p == "" {
		t.Log("AGENTFILE_PRIVATE_DENYLIST not set; scanning generic patterns only")
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("private denylist: %v", err)
	}
	defer f.Close()
	var terms []*regexp.Regexp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" && !strings.HasPrefix(s, "#") {
			terms = append(terms, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(s)+`\b`))
		}
	}
	return terms
}

func check(t *testing.T, name, text string, terms []*regexp.Regexp) {
	t.Helper()
	for _, re := range generic {
		for _, m := range re.FindAllString(text, -1) {
			if !allowed.MatchString(m) {
				t.Errorf("%s: personal or machine-specific string %q", name, m)
			}
		}
	}
	for _, term := range terms {
		if term.MatchString(text) {
			t.Errorf("%s: contains a private denylist term", name)
		}
	}
}

func TestRepositoryFiles(t *testing.T) {
	terms := privateTerms(t)
	root, _ := filepath.Abs("../..")
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" && !d.IsDir() {
			return nil // worktree pointer file
		}
		if d.IsDir() {
			switch rel {
			case ".git", "plans", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "go.sum" || rel == filepath.Join("internal", "scan", "scan_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		check(t, rel, string(b), terms)
		return nil
	})
}

func TestRenderedContent(t *testing.T) {
	terms := privateTerms(t)
	for _, claude := range []string{"", "pro", "max-5x", "max-20x", "team-enterprise", "unknown"} {
		for _, codex := range []string{"", "plus", "pro", "business-enterprise", "unknown"} {
			var p profile.Profile
			if claude != "" {
				p.Claude = &profile.AgentProfile{Plan: claude, Credits: true}
			}
			if codex != "" {
				p.Codex = &profile.AgentProfile{Plan: codex}
			}
			if p.Claude == nil && p.Codex == nil {
				continue
			}
			for _, goos := range []string{"darwin", "linux", "windows"} {
				arts, err := render.Render(p, goos)
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range arts {
					text := string(a.Data)
					for _, b := range a.Files {
						text += string(b)
					}
					check(t, goos+":"+string(a.Agent)+"/"+a.Path, text, terms)
				}
			}
		}
	}
}
