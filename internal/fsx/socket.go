package fsx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// ListenSocket makes a Unix socket of the system disk with mode 0660, never wider for a moment, in a folder that must
// be there already: the local API's (specs.md §25.1). A socket an earlier run left at that path is replaced; anything
// else there stays, and is an error. Closing the listener removes the socket.
func ListenSocket(ctx context.Context, path string) (net.Listener, error) {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("its folder: %w", err)
	}
	switch st, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("%w", err)
	case st.Mode().Type() != fs.ModeSocket:
		return nil, fmt.Errorf("%s is no socket, and stays", path)
	default:
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("the socket an earlier run left: %w", err)
		}
	}
	// The caller makes no other file meanwhile: the mask is the process's own for that moment.
	old := syscall.Umask(0o177)
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return nil, errors.Join(fmt.Errorf("%w", err), l.Close())
	}
	return l, nil
}
