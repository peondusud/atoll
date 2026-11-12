package fsx

import (
	"fmt"
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
