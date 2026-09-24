// Package state owns agentfile's private state root: the lock, the
// interrupted-operation journal, and verified backup snapshots. It executes
// every change to managed paths as a journaled transaction that can be
// rolled back after a failure or crash.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agentfile/internal/fsnode"
)

// Root locates the state root: Base must already exist; Rel components
// below it are created with owner-only permissions when missing.
type Root struct {
	Base string
	Rel  []string
}

func (r Root) Path() string { return filepath.Join(append([]string{r.Base}, r.Rel...)...) }

// DefaultRoot resolves the per-OS state root. getenv is os.Getenv in
// production and a fixture in tests.
func DefaultRoot(goos, home string, getenv func(string) string) (Root, error) {
	switch goos {
	case "windows":
		base := getenv("LOCALAPPDATA")
		if base == "" || !filepath.IsAbs(base) {
			return Root{}, errors.New("LOCALAPPDATA is not set to an absolute path; refusing to guess a backup location")
		}
		return Root{Base: base, Rel: []string{"agentfile"}}, nil
	case "darwin":
		if err := checkHome(home); err != nil {
			return Root{}, err
		}
		return Root{Base: home, Rel: []string{"Library", "Application Support", "agentfile"}}, nil
	default:
		if x := getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
			return Root{Base: x, Rel: []string{"agentfile"}}, nil
		}
		if err := checkHome(home); err != nil {
			return Root{}, err
		}
		return Root{Base: home, Rel: []string{".local", "state", "agentfile"}}, nil
	}
}

func checkHome(home string) error {
	if home == "" || !filepath.IsAbs(home) {
		return errors.New("cannot resolve the current user's home directory safely")
	}
	return nil
}

// Store is a safety-checked state root. Opening it writes nothing; the
// directories are created on the first operation that needs them.
type Store struct {
	Dir  string
	root Root
}

// Open checks existing state components without creating anything and
// rejects symlinked or non-directory components that could redirect backups.
func Open(r Root) (*Store, error) {
	fi, err := os.Lstat(r.Base)
	if err != nil {
		return nil, fmt.Errorf("state base %s: %w", r.Base, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("state base %s is not a real directory", r.Base)
	}
	for _, p := range r.components() {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return nil, fmt.Errorf("state path %s is not a real directory; refusing to write backups through it", p)
		}
	}
	return &Store{Dir: r.Path(), root: r}, nil
}

func (r Root) components() []string {
	var out []string
	cur := r.Base
	for _, c := range append(append([]string{}, r.Rel...), "backups") {
		cur = filepath.Join(cur, c)
		out = append(out, cur)
	}
	return out
}

// ensure creates missing state directories owner-only and re-verifies them.
// Only agentfile's own directories (the last two) are restricted when they
// already exist; shared parents such as ~/.local keep their permissions.
func (s *Store) ensure() error {
	if _, err := Open(s.root); err != nil {
		return err
	}
	comps := s.root.components()
	for i, p := range comps {
		if err := ensureDir(p, i >= len(comps)-2); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) lockPath() string    { return filepath.Join(s.Dir, "lock") }
func (s *Store) journalPath() string { return filepath.Join(s.Dir, "journal.json") }
func (s *Store) backupsDir() string  { return filepath.Join(s.Dir, "backups") }

// ErrLocked means another agentfile run holds the lock, or one crashed.
var ErrLocked = errors.New("state is locked")

func (s *Store) lock() error {
	if err := s.ensure(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w by %s: another agentfile may be running; if none is, choose Recover", ErrLocked, s.lockPath())
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "pid %d\nstarted %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) unlock() { os.Remove(s.lockPath()) }

// Locked reports whether a lock file is present.
func (s *Store) Locked() bool {
	_, err := os.Lstat(s.lockPath())
	return err == nil
}

// writeDurable atomically replaces path with data and flushes it.
func writeDurable(path string, data []byte) error {
	tmp := path + ".tmp-" + randomID()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return fsnode.SyncDir(filepath.Dir(path))
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(path, append(b, '\n'))
}

func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newSnapshotID(now time.Time) string {
	return now.UTC().Format("20060102T150405Z") + "-" + randomID()
}

// hostOS is recorded in manifests; overridable in tests.
var hostOS = runtime.GOOS

func ensureDir(path string, private bool) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		fi, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("state path %s is not a real directory; refusing to write backups through it", path)
	}
	if private {
		return restrictToOwner(path, fi)
	}
	return nil
}
