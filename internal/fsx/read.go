package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ReadFile reads a whole file that is not on a pool disk, such as the configuration.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}

// Readlink reads where a symbolic link that is not on a pool disk leads, such as /proc's link to a process's
// executable, or to a file a descriptor holds.
func Readlink(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("readlink: %w", err)
	}
	return target, nil
}

// ReadAt reads n bytes of a file or a block device from off, such as a few sectors of a device that holds no mounted
// filesystem; fewer when it ends first. An error wraps fs.ErrNotExist when there is nothing at path.
func ReadAt(path string, off int64, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read: %w", err)
	}
	return buf[:got], nil
}
