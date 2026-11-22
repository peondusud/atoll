package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Owner is whose a file or a folder of the system disk is, and its mode, as stat gives them.
type Owner struct {
	UID, GID uint32
	Mode     fs.FileMode
}

// Folder is a folder of the system disk, with whose it is.
type Folder struct {
	Path string
	Owner
}

// ownerOf reads whose a file is from what stat gave of it.
func ownerOf(st fs.FileInfo) Owner {
	o := Owner{Mode: st.Mode()}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		o.UID, o.GID = sys.Uid, sys.Gid
	}
	return o
}

// ReadSmallFile reads a regular file of the system disk that holds at most limit bytes, such as a token: a larger one
// is an error, and is not read whole.
func ReadSmallFile(path string, limit int64) (data []byte, err error) {
	data, _, err = ReadSecretFile(path, limit)
	return data, err
}

// ReadSecretFile is ReadSmallFile for a file that holds a secret, such as a token: it also gives whose the file it read
// is, and its mode, as that file says them, with no second look at its path. A path that leads to what is no regular
// file, a FIFO among them, is refused without waiting for a writer.
func ReadSecretFile(path string, limit int64) (data []byte, owner Owner, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Owner{}, fmt.Errorf("read: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, err := f.Stat()
	if err != nil {
		return nil, Owner{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, Owner{}, fmt.Errorf("read %s: it is no regular file", path)
	}
	data, err = readLimited(f, path, limit)
	return data, ownerOf(st), err
}

// StatSecretFile gives whose the file at path is, and its mode, following symbolic links, without reading it.
func StatSecretFile(path string) (Owner, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Owner{}, fmt.Errorf("stat: %w", err)
	}
	return ownerOf(st), nil
}

func readLimited(f *os.File, path string, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("read %s: more than %d bytes", path, limit)
	}
	return data, nil
}

// maxLinks is how many symbolic links a path may go through, as the kernel's own limit.
const maxLinks = 40

// SecretFolders gives the folders that hold a name on the way to the file at an absolute path, each once, in the order
// the path goes through them: the folder of each symbolic link the path goes through, a folder's or the file's, then
// the folder of the file itself, with whose each is and its mode. Whoever may write one of them may put a file of their
// own at that name, or point that link elsewhere (specs.md §25.1).
func SecretFolders(path string) ([]Folder, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s is no absolute path", path)
	}
	var holders []string
	pending := strings.Split(path, "/")
	resolved, links := "/", 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		st, err := os.Lstat(next)
		if err != nil {
			return nil, fmt.Errorf("stat: %w", err)
		}
		if st.Mode()&fs.ModeSymlink == 0 {
			if !slices.ContainsFunc(pending, func(p string) bool { return p != "" && p != "." }) {
				holders = append(holders, resolved)
			}
			resolved = next
			continue
		}
		if links++; links > maxLinks {
			return nil, fmt.Errorf("%s: more than %d symbolic links on the way", path, maxLinks)
		}
		holders = append(holders, resolved)
		target, err := os.Readlink(next)
		if err != nil {
			return nil, fmt.Errorf("readlink: %w", err)
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	var out []Folder
	for _, dir := range holders {
		if slices.ContainsFunc(out, func(f Folder) bool { return f.Path == dir }) {
			continue
		}
		st, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("stat: %w", err)
		}
		out = append(out, Folder{Path: dir, Owner: ownerOf(st)})
	}
	return out, nil
}
