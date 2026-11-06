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
