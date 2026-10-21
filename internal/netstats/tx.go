// Package netstats counts AnyKVM's outgoing socket bytes in one global counter.
package netstats

import (
	"net"
	"sync/atomic"
)

var sent atomic.Uint64

func Add(n int) {
	if n > 0 {
		sent.Add(uint64(n))
	}
}

func Total() uint64 { return sent.Load() }

type conn struct{ net.Conn }

func WrapConn(base net.Conn) net.Conn { return &conn{base} }

func (c *conn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	Add(n)
	return n, err
}

type listener struct{ net.Listener }

func WrapListener(base net.Listener) net.Listener { return &listener{base} }

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return WrapConn(c), nil
}
