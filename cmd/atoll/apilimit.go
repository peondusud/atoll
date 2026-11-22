package main

import (
	"net"
	"sync"
)

// apiMaxConns is how many connections each listener of the local API holds at once, the socket, api.listen and
// metrics.listen each its own (specs.md §25.1): a phone, Home Assistant, Prometheus and a browser need a few. Past it,
// a connection waits in the kernel's queue, unaccepted, until one of those closes, so that clients that open
// connections and send nothing cost the daemon no more descriptors than that, and a flood over TCP leaves the socket's
// places alone. Each place is held at most as long as the server's times allow.
const apiMaxConns = 32

// capped is a listener that holds at most so many connections at once: Accept waits for one of them to close before it
// takes the next. Closing it ends a wait.
type capped struct {
	net.Listener
	places  chan struct{}
	closed  chan struct{}
	closing sync.Once
}

// capConns caps the connections a listener holds at once to n.
func capConns(l net.Listener, n int) net.Listener {
	return &capped{Listener: l, places: make(chan struct{}, n), closed: make(chan struct{})}
}

// Accept waits for a place, then takes the next connection, which frees its place once closed.
func (c *capped) Accept() (net.Conn, error) {
	select {
	case c.places <- struct{}{}:
	case <-c.closed:
		return nil, net.ErrClosed
	}
	conn, err := c.Listener.Accept()
	if err != nil {
		<-c.places
		return nil, err //nolint:wrapcheck // the listener's own, which the server reads as it would without a cap
	}
	return &placedConn{Conn: conn, free: func() { <-c.places }}, nil
}

// Close closes the listener and ends a wait for a place.
func (c *capped) Close() error {
	c.closing.Do(func() { close(c.closed) })
	return c.Listener.Close() //nolint:wrapcheck // the listener's own
}

// placedConn is a connection that holds a place of its listener until it is closed, once however often it is.
type placedConn struct {
	net.Conn
	freeing sync.Once
	free    func()
}

func (p *placedConn) Close() error {
	err := p.Conn.Close()
	p.freeing.Do(p.free)
	return err //nolint:wrapcheck // the connection's own
}

// NetConn gives the connection it holds a place for, as tls.Conn gives its own: fsx.PeerOf reads the socket's peer
// through it.
func (p *placedConn) NetConn() net.Conn { return p.Conn }

// CloseWrite closes the connection's sending side when it has one, as the server does before it closes a connection
// whose request it did not read whole.
func (p *placedConn) CloseWrite() error {
	if cw, ok := p.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() //nolint:wrapcheck // the connection's own
	}
	return nil
}
