package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// serveHTTP routes incoming HTTP(S) connections to tunnels by Host header.
// The connection is spliced through unchanged once the first request has
// been read, so websockets and keep-alive work.
func (srv *Server) serveHTTP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go srv.handleHTTP(c)
	}
}

func (srv *Server) handleHTTP(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var seen bytes.Buffer
	req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(c, &seen)))
	if err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})

	host := strings.ToLower(req.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	sub, ok := strings.CutSuffix(host, "."+srv.cfg.Domain)
	if !ok || strings.Contains(sub, ".") {
		httpError(c, 404, "warren: no tunnel here")
		return
	}

	srv.mu.Lock()
	s := srv.claims["http/"+sub]
	srv.mu.Unlock()
	if s == nil {
		httpError(c, 404, fmt.Sprintf("warren: tunnel %q is not online", sub))
		return
	}
	st, err := s.openStream(c.RemoteAddr().String())
	if err != nil {
		httpError(c, 502, "warren: tunnel client did not respond")
		return
	}
	if _, err := st.Write(seen.Bytes()); err != nil {
		c.Close()
		st.Close()
		return
	}
	pipe(c, st)
}

func httpError(w io.WriteCloser, code int, msg string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n",
		code, http.StatusText(code), len(msg)+1, msg)
	w.Close()
}
