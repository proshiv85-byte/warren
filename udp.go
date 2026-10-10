package main

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// UDP is tunneled by giving every remote address ("flow") its own data
// connection, over which datagrams travel as length-prefixed frames.

const udpIdleTimeout = 2 * time.Minute

type udpFlow struct {
	ch   chan []byte
	last atomic.Int64
}

func (f *udpFlow) touch() { f.last.Store(time.Now().UnixNano()) }

func (f *udpFlow) idle() bool {
	return time.Since(time.Unix(0, f.last.Load())) > udpIdleTimeout
}

func (srv *Server) serveUDP(pc *net.UDPConn, s *session) {
	var mu sync.Mutex
	flows := map[string]*udpFlow{}
	buf := make([]byte, 65535)
	for {
		n, raddr, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		key := raddr.String()
		mu.Lock()
		f := flows[key]
		if f == nil {
			f = &udpFlow{ch: make(chan []byte, 256)}
			f.touch()
			flows[key] = f
			go func() {
				s.runUDPFlow(pc, raddr, f)
				mu.Lock()
				delete(flows, key)
				mu.Unlock()
			}()
		}
		mu.Unlock()
		f.touch()
		select {
		case f.ch <- append([]byte(nil), buf[:n]...):
		default: // client is too slow; drop like a real network would
		}
	}
}

func (s *session) runUDPFlow(pc *net.UDPConn, raddr *net.UDPAddr, f *udpFlow) {
	st, err := s.openStream(raddr.String())
	if err != nil {
		return
	}
	defer st.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			p, err := readFrame(st)
			if err != nil {
				return
			}
			f.touch()
			pc.WriteToUDP(p, raddr)
		}
	}()

	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case p := <-f.ch:
			if writeFrame(st, p) != nil {
				return
			}
		case <-t.C:
			if f.idle() {
				return
			}
		case <-done:
			return
		case <-s.closed:
			return
		}
	}
}

// clientUDP relays frames from the server's data connection to the local
// UDP service and back.
func clientUDP(data net.Conn, local, remote string) {
	defer data.Close()
	u, err := net.Dial("udp", local)
	if err != nil {
		log.Printf("cannot reach local %s: %v", local, err)
		return
	}
	defer u.Close()
	log.Printf("udp flow from %s", remote)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := u.Read(buf)
			if err != nil {
				data.Close()
				return
			}
			if writeFrame(data, buf[:n]) != nil {
				return
			}
		}
	}()
	for {
		p, err := readFrame(data)
		if err != nil {
			return
		}
		u.Write(p)
	}
}
