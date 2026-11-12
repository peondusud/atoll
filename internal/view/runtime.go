package view

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// branchesKey is the runtime setting of mergerfs that holds its branches, on its control file (specs.md §21.1).
const branchesKey = "user.mergerfs.branches"

// ErrNoMergerfs says that no mergerfs serves the view's mount point, so there are no runtime branches to set.
var ErrNoMergerfs = errors.New("no mergerfs at the view's mount point")

// Runtime gives the branches as mergerfs takes them at runtime: each branch of the configuration whose disk
// present says is there, in configuration order, as `atoll mergerfs-config` writes it but not escaped. With no
// branch present it gives "", which must not be written: mergerfs would have no branch.
func (m *Mergerfs) Runtime(present func(disk string) bool) string {
	var parts []string
	for _, b := range m.Branches {
		if !present(b.Disk) {
			continue
		}
		part := b.Path + "=RW"
		if b.MinFree != nil {
			part += "," + mergerfsSize(*b.MinFree)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ":")
}

func controlFile(mount string) string {
	return path.Join(mount, ".mergerfs")
}

// ReadBranches reads the branches mergerfs has at runtime at the view's mount point.
func ReadBranches(mount string) (string, error) {
	buf := make([]byte, 64<<10)
	n, err := unix.Getxattr(controlFile(mount), branchesKey, buf)
	switch {
	case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP):
		return "", ErrNoMergerfs
	case err != nil:
		return "", fmt.Errorf("read the branches of mergerfs: %w", err)
	}
	return string(buf[:n]), nil
}

// WriteBranches gives mergerfs the whole list of its branches at runtime, which puts a branch back at its place
// (measured on mergerfs 2.40.2). The change does not outlive mergerfs.
func WriteBranches(mount, branches string) error {
	if branches == "" {
		return errors.New("write the branches of mergerfs: none")
	}
	if err := unix.Setxattr(controlFile(mount), branchesKey, []byte(branches), 0); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTSUP) {
			return ErrNoMergerfs
		}
		return fmt.Errorf("write the branches of mergerfs: %w", err)
	}
	return nil
}
