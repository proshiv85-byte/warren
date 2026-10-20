package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for protocol capabilities, website tunnels through the reverse
// proxy, the idle connection pool and automatic certificates.

// startServerSrv is startServer, also returning the *Server.
func startServerSrv(t *testing.T, edit func(*ServerConfig)) (*Server, string, *ServerConfig) {
	t.Helper()
	var srv *Server
	ctrl, cfg := startServerWith(t, edit, func(s *Server) { srv = s })
	return srv, ctrl, cfg
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// rawHello does a handshake by hand, the way an older or foreign client would.
func rawHello(t *testing.T, ctrl string, hello any) Msg {
	t.Helper()
	c, err := net.Dial("tcp", ctrl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	b, _ := json.Marshal(hello)
	c.Write(append(b, '\n'))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), `"caps"`) && len(m.Caps) == 0 {
		t.Errorf("empty caps should be omitted: %s", line)
	}
	return m
}

func withCaps(t *testing.T, caps ...string) {
	old := supportedCaps
	supportedCaps = caps
	t.Cleanup(func() { supportedCaps = old })
}

func echoOnce(t *testing.T, addr, msg string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(conn, msg+"\n")
	if line, err := bufio.NewReader(conn).ReadString('\n'); line != msg+"\n" {
		t.Errorf("got %q, %v", line, err)
	}
}

// ---- capabilities ----

func TestNegotiateCaps(t *testing.T) {
	withCaps(t, "a", "b")
	if got := negotiateCaps([]string{"b", "x", "b", "a"}); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("got %v", got)
	}
	if got := negotiateCaps(nil); got != nil {
		t.Errorf("got %v", got)
	}
}

func TestCapsHandshake(t *testing.T) {
	withCaps(t, "a", "b")
	ctrl, _ := startServer(t, nil)

	m := rawHello(t, ctrl, Msg{Type: "hello", Version: protoVersion, Token: testToken, Proto: "tcp", Caps: []string{"b", "future"}})
	if m.Type != "ok" || !slices.Equal(m.Caps, []string{"b"}) {
		t.Errorf("new client: %+v", m)
	}

	// A client from before capabilities existed sends no "caps" at all.
	m = rawHello(t, ctrl, map[string]any{"type": "hello", "version": 1, "token": testToken, "proto": "tcp"})
	if m.Type != "ok" || len(m.Caps) != 0 {
		t.Errorf("old client: %+v", m)
	}
}

func TestVersionMismatch(t *testing.T) {
	ctrl, _ := startServer(t, nil)
	for v, want := range map[int]string{protoVersion + 1: "ask the server admin", protoVersion - 1: "your warren is too old"} {
		m := rawHello(t, ctrl, Msg{Type: "hello", Version: v, Token: testToken, Proto: "tcp"})
		if m.Type != "error" || !m.Fatal || !strings.Contains(m.Error, want) {
			t.Errorf("version %d: %+v", v, m)
		}
	}
}

func TestClientStop(t *testing.T) {
	ctrl, _ := startServer(t, nil)
	port := freePort(t)
	c := &client{server: ctrl, token: testToken, proto: "tcp", local: echoTCP(t), port: port, stop: make(chan struct{})}
	done := make(chan error)
	go func() { done <- c.run() }()
	echoOnceRetry(t, fmt.Sprintf("127.0.0.1:%d", port))
	close(c.stop)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client didn't stop")
	}
}

// echoOnceRetry waits for a TCP tunnel to come online, then echoes through it.
func echoOnceRetry(t *testing.T, addr string) {
	t.Helper()
	waitDial(t, "tcp", addr)
	echoOnce(t, addr, "hello warren")
}
