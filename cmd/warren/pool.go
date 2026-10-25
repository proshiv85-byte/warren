package main

import (
	"errors"
	"net"
	"time"
)

// Idle connection pool ("pool" capability)
//
// Opening a stream the normal way costs a round trip for "conn", then a TCP
// and TLS handshake and "join" before the visitor's first byte can move. With
// "pool" the ok message carries a pool id, and the client keeps a few data
// connections open ahead of time:
//
//   client -> {"type":"idle","id":<pool id>}
//   ...waits until a visitor arrives...
//   server -> {"type":"conn","remote":..}
//   client -> {"type":"ack"}
//
// after which the connection carries the visitor's bytes as usual. The ack
// tells the server the connection was still alive; if it doesn't come, or the
// pool is empty, the server falls back to "conn"/"join".

const (
	poolSize    = 4               // idle connections a client keeps open
	poolMax     = 16              // most the server will hold per session
	ackTimeout  = 3 * time.Second // before giving up on an idle connection
	poolBackoff = 2 * time.Second // between failed attempts to refill
)

// addIdle parks an idle connection from the client that owns pool id.
func (srv *Server) addIdle(id string, c net.Conn) {
	srv.mu.Lock()
	s := srv.pools[id]
	if s == nil || len(s.idle) >= poolMax {
		srv.mu.Unlock()
		c.Close()
		return
	}
	s.idle = append(s.idle, c)
	srv.mu.Unlock()
}

// takeIdle hands a visitor to a pooled connection, newest first since those
// are the least likely to have gone stale. It returns nil if none worked.
func (s *session) takeIdle(remote string) net.Conn {
	srv := s.srv
	for {
		srv.mu.Lock()
		n := len(s.idle)
		if n == 0 {
			srv.mu.Unlock()
			return nil
		}
		c := s.idle[n-1]
		s.idle = s.idle[:n-1]
		srv.mu.Unlock()

		c.SetDeadline(time.Now().Add(ackTimeout))
		err := writeMsg(c, Msg{Type: "conn", Remote: remote})
		if err == nil {
			var m Msg
			if m, err = readMsg(c); err == nil && m.Type != "ack" {
				err = errors.New("no ack")
			}
		}
		if err == nil {
			c.SetDeadline(time.Time{})
			return c
		}
		c.Close()
	}
}

// openPool registers s as having a pool and returns its id.
func (s *session) openPool() string {
	id := randHex(16)
	s.srv.mu.Lock()
	s.srv.pools[id] = s
	s.srv.mu.Unlock()
	return id
}

// closePool forgets the pool and closes what's left in it.
func (s *session) closePool(id string) {
	s.srv.mu.Lock()
	delete(s.srv.pools, id)
	idle := s.idle
	s.idle = nil
	s.srv.mu.Unlock()
	for _, c := range idle {
		c.Close()
	}
}

// keepPool keeps poolSize idle connections open until done is closed.
func (c *client) keepPool(id string, done <-chan struct{}, logConn bool) {
	for i := 0; i < poolSize; i++ {
		go func() {
			for {
				data, remote, err := c.waitIdle(id, done)
				if err == nil {
					go c.serveStream(data, remote, logConn)
					continue
				}
				select {
				case <-done:
					return
				case <-time.After(poolBackoff):
				}
			}
		}()
	}
}

// waitIdle opens an idle connection and waits for a visitor to arrive on it.
func (c *client) waitIdle(id string, done <-chan struct{}) (net.Conn, string, error) {
	select {
	case <-done:
		return nil, "", errors.New("session over")
	default:
	}
	data, err := c.dial()
	if err != nil {
		return nil, "", err
	}
	got := make(chan struct{})
	defer close(got)
	go func() {
		select {
		case <-done:
			data.Close()
		case <-got:
		}
	}()

	data.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := writeMsg(data, Msg{Type: "idle", ID: id}); err != nil {
		data.Close()
		return nil, "", err
	}
	m, err := readMsg(data) // unbuffered, so the visitor's bytes stay unread
	if err == nil && m.Type != "conn" {
		err = errors.New("unexpected " + m.Type)
	}
	if err == nil {
		err = writeMsg(data, Msg{Type: "ack"})
	}
	if err != nil {
		data.Close()
		return nil, "", err
	}
	data.SetWriteDeadline(time.Time{})
	return data, m.Remote, nil
}
