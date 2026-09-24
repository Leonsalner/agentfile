// Command agentfile installs portable instructions and skills for Claude Code
// and Codex, with a preview before every write and exact restore.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	tea "charm.land/bubbletea/v2"

	"agentfile/internal/installer"
	"agentfile/internal/render"
	"agentfile/internal/state"
	"agentfile/internal/tui"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and content provenance")
	flag.Parse()
	if *showVersion {
		p, err := render.LoadProvenance()
		if err != nil {
			fatal(err)
		}
		fmt.Printf("agentfile %s\ncontent %s (upstream %s, commit %s, imported %s)\n",
			version, p.ContentVersion, p.UpstreamVersion, p.UpstreamCommit, p.Imported)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fatal(fmt.Errorf("cannot resolve your home directory: %w", err))
	}
	env := installer.Env{Home: home, GOOS: runtime.GOOS, Getenv: os.Getenv, LookPath: exec.LookPath}
	root, err := state.DefaultRoot(runtime.GOOS, home, os.Getenv)
	if err != nil {
		fatal(err)
	}
	st, err := state.Open(root)
	if err != nil {
		fatal(err)
	}
	if _, err := tea.NewProgram(tui.New(env, st)).Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "agentfile:", err)
	os.Exit(1)
}
