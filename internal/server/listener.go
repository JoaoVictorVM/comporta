package server

import (
	"net"
	"sync/atomic"
	"time"
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
}

func (c *admittedConn) SetReadDeadline(t time.Time) error {
	if c.pending.CompareAndSwap(true, false) && t.IsZero() {
		return c.Conn.SetReadDeadline(c.deadline)
	}
	return c.Conn.SetReadDeadline(t)
}
