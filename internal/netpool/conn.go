package netpool

import (
	"net"
	"sync"
)

// PoolConn is a wrapper around net.Conn to modify the the behavior of
// net.Conn's Close() method.
type NetConn struct {
	net.Conn
	mu       sync.RWMutex
	unusable bool
}

// Close() puts the given connects back to the pool instead of closing it.
func (p *NetConn) Close() error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.unusable {
		if p.Conn != nil {
			return p.Conn.Close()
		}
	}

	return nil
}

// MarkUnusable() marks the connection not usable any more, to let the pool close it instead of returning it to pool.
func (p *NetConn) MarkUnusable() {
	p.mu.Lock()
	p.unusable = true
	p.mu.Unlock()
}

// newConn wraps a standard net.Conn to a poolConn net.Conn.
func WrapConn(conn net.Conn) net.Conn {
	p := &NetConn{Conn: conn}
	return p
}
