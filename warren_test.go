package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token"

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startServer runs a warren server on loopback ports and returns its
// control address. edit can adjust the config before it starts.
func startServer(t *testing.T, edit func(*ServerConfig)) (string, *ServerConfig) {
	t.Helper()
	cfg := &ServerConfig{
		Listen:  fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		Domain:  "tunnel.test",
		PortMin: 1024,
		PortMax: 65535,
		Users:   []UserConfig{{Name: "alice", Token: testToken}},
	}
	if edit != nil {
		edit(cfg)
	}
	path := filepath.Join(t.TempDir(), "server.json")
	if err := saveServerConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfgPath:  path,
		cfg:      cfg,
		users:    cfg.Users,
		sessions: map[*session]struct{}{},
		claims:   map[string]*session{},
		pending:  map[string]chan net.Conn{},
	}
	go srv.run()
	waitDial(t, "tcp", cfg.Listen)
	if cfg.HTTPAddr != "" {
		waitDial(t, "tcp", cfg.HTTPAddr)
	}
	return cfg.Listen, cfg
}

func waitDial(t *testing.T, network, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial(network, addr)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never came up: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// echoTCP starts a local TCP echo service.
func echoTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// startClient runs a tunnel client in the background, as "warren PROTO" would.
func startClient(t *testing.T, server, proto, local string, port int, sub string) {
	t.Helper()
	c := &client{server: server, token: testToken, proto: proto, local: local, port: port, subdomain: sub}
	go c.run()
}

// hello opens a raw control connection and returns it with the server's reply.
func hello(t *testing.T, server string, m Msg) (net.Conn, Msg) {
	t.Helper()
	c, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	m.Type, m.Version, m.Token = "hello", protoVersion, testToken
	if err := writeMsg(c, m); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply, err := readMsg(c)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Time{})
	return c, reply
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
}

func TestTCPTunnel(t *testing.T) {
	server, _ := startServer(t, nil)
	port := freePort(t)
	startClient(t, server, "tcp", echoTCP(t), port, "")
	pub := fmt.Sprintf("127.0.0.1:%d", port)
	waitDial(t, "tcp", pub)

	c, err := net.Dial("tcp", pub)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, "hello through warren")
	roundTrip(t, c, "and again")
}

func TestUDPTunnel(t *testing.T) {
	server, _ := startServer(t, nil)
	local, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := local.ReadFrom(buf)
			if err != nil {
				return
			}
			local.WriteTo(buf[:n], addr)
		}
	}()

	port := freePort(t)
	startClient(t, server, "udp", local.LocalAddr().String(), port, "")
	c, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The tunnel comes up asynchronously and UDP gives no signal, so retry.
	buf := make([]byte, 2048)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.Write([]byte("ping"))
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := c.Read(buf)
		if err == nil {
			if string(buf[:n]) != "ping" {
				t.Fatalf("got %q", buf[:n])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no UDP echo: %v", err)
		}
	}
}

func TestHTTPRoutingByHost(t *testing.T) {
	server, cfg := startServer(t, func(c *ServerConfig) {
		c.HTTPAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	})
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			}()
		}
	}()
	startClient(t, server, "http", backend.Addr().String(), 0, "alice")

	get := func(host string) string {
		c, err := net.Dial("tcp", cfg.HTTPAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		b, _ := io.ReadAll(c)
		return string(b)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasSuffix(get("alice.tunnel.test"), "hi") {
		if time.Now().After(deadline) {
			t.Fatal("tunnel never answered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := get("bob.tunnel.test"); !strings.HasPrefix(got, "HTTP/1.1 404") {
		t.Fatalf("unknown subdomain: got %q", got)
	}
}

func TestHTTPHeaderLimit(t *testing.T) {
	_, cfg := startServer(t, func(c *ServerConfig) {
		c.HTTPAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	})
	c, err := net.Dial("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	go func() {
		fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x.tunnel.test\r\n")
		junk := "X-Junk: " + strings.Repeat("a", 1000) + "\r\n"
		for i := 0; i < 200; i++ {
			if _, err := io.WriteString(c, junk); err != nil {
				return
			}
		}
	}()
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 431") {
		t.Fatalf("got %q, want a 431", b)
	}
}

// A client whose old control connection is still lingering on the server
// (network dropped, server hasn't noticed yet) must be able to reconnect
// even when it's at its tunnel limit.
func TestReconnectAtTunnelLimit(t *testing.T) {
	server, _ := startServer(t, func(c *ServerConfig) {
		c.Users[0].MaxTunnels = 1
	})
	port := freePort(t)
	_, first := hello(t, server, Msg{Proto: "tcp", Port: port})
	if first.Type != "ok" {
		t.Fatalf("first connect: %+v", first)
	}
	_, second := hello(t, server, Msg{Proto: "tcp", Port: port})
	if second.Type != "ok" {
		t.Fatalf("reconnect with the stale session still there: %+v", second)
	}
	// The limit itself still holds for a different tunnel.
	_, third := hello(t, server, Msg{Proto: "tcp", Port: freePort(t)})
	if third.Type != "error" || !strings.Contains(third.Error, "limit") {
		t.Fatalf("second tunnel over the limit: %+v", third)
	}
}

func TestInvalidToken(t *testing.T) {
	server, _ := startServer(t, nil)
	c, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeMsg(c, Msg{Type: "hello", Version: protoVersion, Token: "nope", Proto: "tcp"})
	m, err := readMsg(c)
	if err != nil || m.Type != "error" || !m.Fatal {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// writeSelfSigned creates a self-signed certificate like setup.sh does.
func writeSelfSigned(t *testing.T) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tunnel.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = x509.ParseCertificate(der)
	kb, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certPath, keyPath, cert
}

func TestFingerprintPinning(t *testing.T) {
	certPath, keyPath, cert := writeSelfSigned(t)
	server, _ := startServer(t, func(c *ServerConfig) {
		c.TLSCert, c.TLSKey = certPath, keyPath
	})
	pin := certFingerprint(cert)

	ok := &client{server: server, useTLS: true, pin: pin}
	conn, err := ok.dial()
	if err != nil {
		t.Fatalf("correct pin: %v", err)
	}
	conn.Close()

	wrong := &client{server: server, useTLS: true, pin: "sha256:" + strings.Repeat("0", 64)}
	if _, err := wrong.dial(); !errors.Is(err, errPinMismatch) {
		t.Fatalf("wrong pin: got %v, want errPinMismatch", err)
	}

	// Without a pin a self-signed certificate is rejected as before.
	plain := &client{server: server, useTLS: true}
	if _, err := plain.dial(); err == nil {
		t.Fatal("self-signed certificate accepted without a pin")
	}

	// A wrong pin on the first connect is fatal rather than retried forever.
	wrong.token, wrong.proto, wrong.local = testToken, "tcp", "127.0.0.1:1"
	done := make(chan error, 1)
	go func() { done <- wrong.run() }()
	select {
	case err := <-done:
		if !errors.Is(err, errPinMismatch) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client kept retrying with a wrong pin")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	want := "sha256:" + hex64
	colons := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	for _, in := range []string{want, hex64, "SHA256:" + strings.ToUpper(hex64), colons, " " + want + " "} {
		got, err := normalizeFingerprint(in)
		if err != nil || got != want {
			t.Errorf("normalizeFingerprint(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "sha256:abc", "sha256:" + strings.Repeat("zz", 32)} {
		if _, err := normalizeFingerprint(in); err == nil {
			t.Errorf("normalizeFingerprint(%q) accepted", in)
		}
	}
}

func TestProxyHeader(t *testing.T) {
	tests := []struct{ src, dst, want string }{
		{"1.2.3.4:5000", "127.0.0.1:25565", "PROXY TCP4 1.2.3.4 127.0.0.1 5000 25565\r\n"},
		{"[2001:db8::1]:5000", "[::1]:25565", "PROXY TCP6 2001:db8::1 ::1 5000 25565\r\n"},
		{"[2001:db8::1]:5000", "127.0.0.1:25565", "PROXY TCP6 2001:db8::1 :: 5000 25565\r\n"},
		{"garbage", "127.0.0.1:25565", "PROXY UNKNOWN\r\n"},
	}
	for _, tt := range tests {
		if got := proxyHeader(tt.src, tt.dst); got != tt.want {
			t.Errorf("proxyHeader(%q, %q) = %q, want %q", tt.src, tt.dst, got, tt.want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.3.0", true},
		{"0.3.0", "0.3.0", false},
		{"0.10.0", "0.9.0", false},
		{"1.0", "1.0.1", true},
	}
	for _, tt := range tests {
		if got := versionLess(tt.a, tt.b); got != tt.want {
			t.Errorf("versionLess(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestParseArgsFlagsAnywhere(t *testing.T) {
	fs := flag.NewFlagSet("tcp", flag.ContinueOnError)
	port := fs.Int("port", 0, "")
	pos, err := parseArgs(fs, []string{"25565", "--port", "1234"})
	if err != nil || *port != 1234 || len(pos) != 1 || pos[0] != "25565" {
		t.Fatalf("pos=%v port=%d err=%v", pos, *port, err)
	}
}

func TestReadLineLimit(t *testing.T) {
	if _, err := readLine(strings.NewReader(strings.Repeat("a", 100)+"\n"), 10); err == nil {
		t.Fatal("over-long line accepted")
	}
	line, err := readLine(strings.NewReader("ok\nrest"), 10)
	if err != nil || string(line) != "ok" {
		t.Fatalf("got %q, %v", line, err)
	}
}
