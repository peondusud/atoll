package fsx

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// wrappedConn gives the connection it wraps, as a listener that caps its connections does (cmd/atoll/apilimit.go).
type wrappedConn struct{ net.Conn }

func (w wrappedConn) NetConn() net.Conn { return w.Conn }

// selfConn gives itself as the connection it wraps.
type selfConn struct{ net.Conn }

func (s selfConn) NetConn() net.Conn { return s }

// PeerOf gives the account, the group and the process that connected to the local API's socket, as the kernel
// recorded them (SO_PEERCRED), through a wrapper that gives its connection; and nothing for a TCP connection, or for a
// wrapper that never ends (specs.md §25.1).
func TestPeerOf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	l, err := ListenSocket(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	client, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, ok := <-accepted
	if !ok {
		t.Fatal("no connection accepted")
	}
	defer func() { _ = server.Close() }()
	want := Peer{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()), PID: int32(os.Getpid())} //nolint:gosec // this process's own ids
	for _, c := range []net.Conn{server, wrappedConn{server}, wrappedConn{wrappedConn{server}}} {
		if got, ok := PeerOf(c); !ok || got != want {
			t.Errorf("PeerOf(%T) = %+v, %v, want %+v", c, got, ok, want)
		}
	}

	tcp, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	go func() {
		if c, err := tcp.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	over, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Close() }()
	for _, c := range []net.Conn{over, wrappedConn{over}, selfConn{server}, nil, wrappedConn{}} {
		if got, ok := PeerOf(c); ok {
			t.Errorf("PeerOf(%T) = %+v", c, got)
		}
	}
}
