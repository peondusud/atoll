package fsx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrChangedMeanwhile says that a file no longer holds what was read of it.
var ErrChangedMeanwhile = errors.New("the file changed since it was read")

// ReplaceFile writes data over the file at path, all or nothing: a file beside it, <name>.new, with the mode and
// owner of the old one, synced, renamed over it, then the folder synced. It refuses with ErrChangedMeanwhile when
// the file no longer holds was, so that an edit made meanwhile is never lost (specs.md §10). A symbolic link is
// followed, and the file it leads to replaced.
func ReplaceFile(path string, was, data []byte) error {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	now, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !bytes.Equal(now, was) {
		return fmt.Errorf("%s: %w", path, ErrChangedMeanwhile)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	tmp := path + ".new"
	if err := writeSynced(tmp, data, info); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Dir(path), err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Dir(path), err)
	}
	return nil
}

func writeSynced(path string, data []byte, like os.FileInfo) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, like.Mode().Perm())
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("%s: %w", path, closeErr)
		}
	}()
	// The umask may have taken bits away, and a file root writes is root's.
	if err := f.Chmod(like.Mode().Perm()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if st, ok := like.Sys().(*syscall.Stat_t); ok {
		if err := f.Chown(int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
