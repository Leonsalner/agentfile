package fsnode

// Content-only mode manages a regular file's default-stream bytes and nothing
// else. Existing files are updated in place through a handle whose file
// identity and bytes are rechecked after opening, so the file object and its
// other properties stay; only missing files are created.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrContentChanged means a target's bytes or file identity no longer match
// what was checked; nothing was written.
var ErrContentChanged = errors.New("content or file identity changed since it was checked")

func changed(path string) error { return fmt.Errorf("%s: %w", path, ErrContentChanged) }

// CaptureContent reads a managed file's bytes and file identity without its
// metadata. A missing path yields an Absent node. Anything but a regular,
// single-link file without other streams is refused.
func CaptureContent(path string) (*Node, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Node{Type: Absent}, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, unsupported(path, "not a regular file")
	}
	f, err := openContent(path, openRead)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	id, err := checkOpened(path, f)
	if err != nil {
		return nil, err
	}
	b, err := readAll(f)
	if err != nil {
		return nil, err
	}
	return &Node{Type: File, Data: b, FileID: id}, nil
}

// CaptureContentDir checks a parent directory's type without recording its
// metadata. A missing path yields an Absent node.
func CaptureContentDir(path string) (*Node, error) {
	err := CheckPlainDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Node{Type: Absent}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Node{Type: Dir, Children: map[string]*Node{}}, nil
}

// CheckPlainDir requires path to be a real directory, not a link, junction or
// other redirection.
func CheckPlainDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return checkDirPlatform(path, fi)
}

// ContentFingerprint identifies an entry's type and bytes only.
func ContentFingerprint(n *Node) string {
	h := sha256.New()
	fmt.Fprintf(h, "content\x00%s\x00", n.Type)
	if n.Type == File {
		d := sha256.Sum256(n.Data)
		h.Write(d[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WriteContent replaces the bytes of the existing file at path in place. It
// opens the file without following links, then requires the opened object to
// have identity expectID and bytes matching expectFP. If writing fails it
// puts the original bytes back through the same handle. It returns the
// file's identity, which is unchanged.
func WriteContent(path, expectID, expectFP string, data []byte) (string, error) {
	f, err := openContent(path, openWrite)
	if errors.Is(err, fs.ErrNotExist) {
		return "", changed(path)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	old, err := checkExpected(path, f, expectID, expectFP)
	if err != nil {
		return "", err
	}
	if err := overwrite(f, data); err != nil {
		if rerr := overwrite(f, old); rerr != nil {
			return "", fmt.Errorf("%s: %w; putting the original bytes back also failed: %v", path, err, rerr)
		}
		return "", err
	}
	if got, err := readAll(f); err != nil || !bytes.Equal(got, data) {
		return "", fmt.Errorf("%s did not verify after writing", path)
	}
	return expectID, nil
}

// CreateContent creates a missing file with default metadata. It never
// replaces an existing entry.
func CreateContent(path string, data []byte) (string, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return "", changed(path)
	}
	if err != nil {
		return "", err
	}
	id, err := checkOpened(path, f)
	if err == nil {
		err = overwrite(f, data)
	}
	if err != nil {
		f.Close()
		if id != "" {
			if cleanupErr := removeCreatedIfSame(path, id); cleanupErr != nil {
				return "", fmt.Errorf("%w; created file could not be safely removed: %v", err, cleanupErr)
			}
		}
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return id, SyncDir(filepath.Dir(path))
}

// removeCreatedIfSame refuses to remove a different file placed at path
// after a failed create. RemoveContent checks the opened file again.
func removeCreatedIfSame(path, id string) error {
	cur, err := CaptureContent(path)
	if err != nil {
		return err
	}
	if cur.Type != File || cur.FileID != id {
		return changed(path)
	}
	return RemoveContent(path, id, ContentFingerprint(cur))
}

// RemoveContent deletes the file at path only if its identity and bytes
// still match.
func RemoveContent(path, expectID, expectFP string) error {
	f, err := openContent(path, openDelete)
	if errors.Is(err, fs.ErrNotExist) {
		return changed(path)
	}
	if err != nil {
		return err
	}
	if _, err := checkExpected(path, f, expectID, expectFP); err != nil {
		f.Close()
		return err
	}
	if err := removeOpened(path, f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s was not removed", path)
	}
	return SyncDir(filepath.Dir(path))
}

type openMode int

const (
	openRead openMode = iota
	openWrite
	openDelete
)

func checkExpected(path string, f *os.File, expectID, expectFP string) ([]byte, error) {
	id, err := checkOpened(path, f)
	if err != nil {
		return nil, err
	}
	if id != expectID {
		return nil, changed(path)
	}
	b, err := readAll(f)
	if err != nil {
		return nil, err
	}
	if ContentFingerprint(&Node{Type: File, Data: b}) != expectFP {
		return nil, changed(path)
	}
	return b, nil
}

func readAll(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func overwrite(f *os.File, data []byte) error {
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return err
	}
	return f.Sync()
}
