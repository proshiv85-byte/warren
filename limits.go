package main

import (
	"log"
	"net"
	"sync"
	"time"
)

// Limits that keep one flood from exhausting the server. They're generous
// enough that normal use never gets near them.
const (
	defaultMaxConnsPerTunnel = 1000 // visitors connected through one tunnel
	defaultMaxPendingStreams = 64   // visitors waiting for the client to answer
	defaultMaxHandshakesIP   = 256  // control-port connections per IP that haven't said hello/join yet
	defaultMaxHandshakes     = 4096 // the same, across all IPs
)

type limits struct {
	connsPerTunnel int
	pendingStreams int
	handshakesIP   int
	handshakes     int
}

// handshakes counts control-port connections that haven't sent their first
// message yet, in total and per IP.
type handshakes struct {
	mu       sync.Mutex
	total    int
	perIP    map[string]int
	lastWarn time.Time
}

// start reserves a handshake slot for addr and reports whether it got one.
// Each successful start must be paired with done.
func (h *handshakes) start(addr net.Addr, l limits) bool {
	ip := hostOf(addr)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= l.handshakes || h.perIP[ip] >= l.handshakesIP {
		if time.Since(h.lastWarn) > time.Minute {
			h.lastWarn = time.Now()
			log.Printf("too many connections to the control port that haven't logged in (%d from %s, %d in total); refusing new ones",
				h.perIP[ip], ip, h.total)
		}
		return false
	}
	if h.perIP == nil {
		h.perIP = map[string]int{}
	}
	h.total++
	h.perIP[ip]++
	return true
}

func (h *handshakes) done(addr net.Addr) {
	ip := hostOf(addr)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total--
	if h.perIP[ip]--; h.perIP[ip] <= 0 {
		delete(h.perIP, ip)
	}
}

func hostOf(addr net.Addr) string {
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

// reserveConn counts a new visitor against the tunnel's connection limit.
// If it returns true, releaseConn must be called once the visitor is gone.
func (s *session) reserveConn() bool {
	if s.conns.Add(1) > int32(s.srv.limits.connsPerTunnel) {
		s.conns.Add(-1)
		s.warnLimit("too many visitors at once (%d); turning new ones away", s.srv.limits.connsPerTunnel)
		return false
	}
	return true
}

func (s *session) releaseConn() { s.conns.Add(-1) }

// warnLimit logs that a limit was hit, at most once a minute per tunnel so
// a flood doesn't flood the log too.
func (s *session) warnLimit(format string, a ...any) {
	now := time.Now().UnixNano()
	last := s.lastWarn.Load()
	if now-last < int64(time.Minute) || !s.lastWarn.CompareAndSwap(last, now) {
		return
	}
	log.Printf("[%s] "+format, append([]any{s.user}, a...)...)
}
