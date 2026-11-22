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

	"golang.org/x/sys/unix"
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

// Peer is who connected to a Unix socket, as the kernel recorded it when the connection was made (SO_PEERCRED): the
// effective account and group, and the process, which may have ended since, its number then free for another.
type Peer struct {
	UID, GID uint32
	PID      int32
}

// maxWrappers is how many connections PeerOf looks through for a Unix one.
const maxWrappers = 8

// PeerOf gives who connected to a Unix socket, through the wrappers that give the connection they wrap (NetConn), as
// the kernel recorded it (specs.md §25.1). It says nothing of another kind of connection, or when the kernel does not
// say.
func PeerOf(c net.Conn) (Peer, bool) {
	for range maxWrappers {
		switch conn := c.(type) {
		case *net.UnixConn:
			return peerOfUnix(conn)
		case interface{ NetConn() net.Conn }:
			c = conn.NetConn()
		default:
			return Peer{}, false
		}
	}
	return Peer{}, false
}

func peerOfUnix(conn *net.UnixConn) (Peer, bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return Peer{}, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // a descriptor, small
	}); err != nil || credErr != nil {
		return Peer{}, false
	}
	return Peer{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, true
}
