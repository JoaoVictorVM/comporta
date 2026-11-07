package server

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	lingerTimeout  = 250 * time.Millisecond
	lingerMaxBytes = 1 << 20
)

// admissionListener gives each new connection the header deadline of the current
// configuration. http.Server reads its own ReadHeaderTimeout as a fixed field, which
// a reload could not change, so that field stays unset.
type admissionListener struct {
	net.Listener
	source SnapshotSource
}

func (l *admissionListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(l.source.Capture().Listeners().ReadHeaderTimeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		// A connection that died right after accept must not stop the listener.
		_ = conn.Close()
		deadline = time.Now()
	}
	admitted := &admittedConn{Conn: conn, deadline: deadline}
	admitted.pending.Store(true)
	return admitted, nil
}

// admittedConn protects the admission deadline from http.Server, which clears the
// read deadline right before reading headers when its own timeout is unset. Only
// that first clear is replaced; every later call, such as a handler removing the
// deadline to read the body, goes through.
type admittedConn struct {
	net.Conn
	deadline time.Time
	pending  atomic.Bool
	closing  sync.Once
	closed   error
}

func (c *admittedConn) SetReadDeadline(t time.Time) error {
	if c.pending.CompareAndSwap(true, false) && t.IsZero() {
		return c.Conn.SetReadDeadline(c.deadline)
	}
	return c.Conn.SetReadDeadline(t)
}

// CloseWrite keeps net/http able to half-close before closing a connection whose
// request body was not read.
func (c *admittedConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

// Close lingers briefly for what the client is still sending. Closing a socket with
// unread input resets it, which can destroy a response the client has not read yet:
// that is the common case here, since a request is refused without reading its body.
// It returns at once because http.Server may call it while holding its own lock.
func (c *admittedConn) Close() error {
	c.closing.Do(func() {
		if c.CloseWrite() != nil || c.Conn.SetReadDeadline(time.Now().Add(lingerTimeout)) != nil {
			c.closed = c.Conn.Close()
			return
		}
		go func() {
			_, _ = io.CopyN(io.Discard, c.Conn, lingerMaxBytes)
			_ = c.Conn.Close()
		}()
	})
	return c.closed
}
