package fsx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrChangedMeanwhile says that a file no longer holds what was read of it.
var ErrChangedMeanwhile = errors.New("the file changed since it was read")

// ErrNotSynced says that a file was replaced, and that its folder could not be synced after: the new file is there
// for every reader, and a power cut may still bring the one before back.
var ErrNotSynced = errors.New("the file is replaced, but its folder could not be synced")

// syncFolder makes what a folder lists durable.
func syncFolder(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ReplaceFile writes data over the file at path, all or nothing: a file beside it, made for this alone under a name
// nobody can tell beforehand, with the mode and owner of the old one, synced, renamed over it, then the folder synced.
// A name anyone could tell, as <name>.new, would be written through whatever was put there: a draft, or a link to
// another file. It refuses with ErrChangedMeanwhile when the file no longer holds was, so that an edit made meanwhile
// is never lost (specs.md §10). A symbolic link at path is followed, and the file it leads to replaced. Any error
// leaves the file as it was, but ErrNotSynced: the file is replaced then, and its folder could not be synced.
func ReplaceFile(path string, was, data []byte) error {
	return replaceFile(path, was, data, syncFolder)
}

func replaceFile(path string, was, data []byte, sync func(string) error) error {
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
	tmp, err := writeSynced(path, data, info)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s: %w", path, err)
	}
	// The file has its name: what fails from here on has not left it as it was.
	if err := sync(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSynced, err)
	}
	return nil
}

// writeSynced writes data to a new file beside path, like the file there, and gives its name. The file is made by
// this call alone, never opened if something has its name, and removed when it could not be written whole.
func writeSynced(path string, data []byte, like os.FileInfo) (name string, err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.new")
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	name = f.Name()
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("%s: %w", name, closeErr)
		}
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	// Made 0600 and the writer's: the mode and the owner are the old file's, as far as the writer may give them.
	if st, ok := like.Sys().(*syscall.Stat_t); ok {
		if err := f.Chown(int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := f.Chmod(like.Mode().Perm()); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return name, nil
}

// RemoveFile removes a file of the system disk, the catalog's or the configuration's, never one of a pool disk.
func RemoveFile(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// RenameFile renames a file of the system disk, replacing what is at the new name.
func RenameFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("rename %s: %w", from, err)
	}
	return nil
}

// ListNames gives the names in a folder of the system disk, in no order.
func ListNames(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// OpenSystemFile opens a file of the system disk for reading: a catalog backup to copy to the disks.
func OpenSystemFile(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

// ExchangeFiles swaps two names of one folder of the system disk, both at once (renameat2 with RENAME_EXCHANGE): at no
// moment is either name free.
func ExchangeFiles(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("exchange %s and %s: %w", a, b, err)
	}
	return nil
}
