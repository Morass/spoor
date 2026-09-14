package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PutFileFiltered captures path like PutFile, but reads the private copy
// first and keeps it only when reject(body) is false. The hash is returned
// either way so changes to rejected files are still detected.
func (s *Store) PutFileFiltered(path string, reject func([]byte) bool) (hash string, stored bool, err error) {
	tmp := filepath.Join(s.Root, "tmp", NewID())
	if err := cloneOrCopy(path, tmp); err != nil {
		os.Remove(tmp)
		return "", false, err
	}
	// A path swapped for a symlink between the scan's Lstat and the clone
	// would otherwise land in objects/ and be followed on every read.
	if fi, err := os.Lstat(tmp); err != nil || !fi.Mode().IsRegular() {
		os.Remove(tmp)
		return "", false, errors.New("not a regular file")
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", false, err
	}
	h := sha256.Sum256(b)
	sum := hex.EncodeToString(h[:])
	if reject != nil && reject(b) {
		os.Remove(tmp)
		return sum, false, nil
	}
	return sum, true, s.settle(tmp, sum)
}

// WriteFileAtomic writes data to path through a freshly created private
// temporary file that is renamed over the destination: an existing file's
// permissions are not inherited and an existing symlink is replaced rather
// than followed.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".spoor-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		os.Remove(tmp)
		return fmt.Errorf("%s is a directory", path)
	}
	return os.Rename(tmp, path)
}

// openNoFollow opens a regular file without following a final symlink.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	return f, nil
}

var _ = io.EOF
