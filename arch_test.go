package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// ---- website tunnels ----

// webApp is a local website that reports what it received.
type webApp struct {
	addr  string
	conns atomic.Int32 // TCP connections accepted
}

func startWebApp(t *testing.T) *webApp {
	t.Helper()
	app := &webApp{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s xff=%s proto=%s auth=%s",
			r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.Header.Get("Authorization"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(c, brw)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			app.conns.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	app.addr = ts.Listener.Addr().String()
	return app
}

// httpServer starts a server with website tunnels on plain HTTP and returns
// the control address and the HTTP address.
func httpServer(t *testing.T, edit func(*ServerConfig)) (string, string) {
	t.Helper()
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ctrl, _ := startServer(t, func(cfg *ServerConfig) {
		cfg.HTTPAddr = httpAddr
		if edit != nil {
			edit(cfg)
		}
	})
	return ctrl, httpAddr
}

// httpGet fetches path from the tunnel sub through addr, waiting for the tunnel
// to come online first.
func httpGet(t *testing.T, cl *http.Client, url, host string, hdr ...string) (int, string) {
	t.Helper()
	var code int
	var body string
	waitFor(t, func() bool {
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(req)
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		code, body = resp.StatusCode, string(b)
		return !strings.Contains(body, "is not online")
	})
	return code, body
}

func TestHTTPForwardedHeaders(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app"})

	code, body := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "app.tunnel.test", "X-Forwarded-For", "6.6.6.6")
	if code != 200 || body != "host=app.tunnel.test xff=127.0.0.1 proto=http auth=" {
		t.Errorf("%d %q", code, body)
	}
	req, _ := http.NewRequest("GET", "http://"+httpAddr+"/", nil)
	req.Host = "nope.tunnel.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown tunnel: %d", resp.StatusCode)
	}
}

func TestHTTPTrustedProxy(t *testing.T) {
	ctrl, httpAddr := httpServer(t, func(cfg *ServerConfig) { cfg.TrustedProxies = []string{"127.0.0.0/8"} })
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app", allow: []string{"203.0.113.9"}})

	code, body := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "app.tunnel.test",
		"X-Forwarded-For", "203.0.113.9", "X-Forwarded-Proto", "https")
	if code != 200 || body != "host=app.tunnel.test xff=203.0.113.9, 127.0.0.1 proto=https auth=" {
		t.Errorf("%d %q", code, body)
	}
	if code, _ := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "app.tunnel.test", "X-Forwarded-For", "198.51.100.1"); code != 403 {
		t.Errorf("other forwarded IP: %d", code)
	}
}

func TestHTTPBasicAuth(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app", auth: "bob:s3cret"})

	url := "http://" + httpAddr + "/"
	if code, _ := httpGet(t, http.DefaultClient, url, "app.tunnel.test"); code != 401 {
		t.Errorf("no password: %d", code)
	}
	wrong := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:nope"))
	if code, _ := httpGet(t, http.DefaultClient, url, "app.tunnel.test", "Authorization", wrong); code != 401 {
		t.Errorf("wrong password: %d", code)
	}
	right := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
	code, body := httpGet(t, http.DefaultClient, url, "app.tunnel.test", "Authorization", right)
	if code != 200 || !strings.HasSuffix(body, "auth=") {
		t.Errorf("right password: %d %q", code, body)
	}
}

func TestHTTPAllow(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "no", allow: []string{"10.0.0.0/8"}})
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "yes", allow: []string{"10.0.0.0/8", "127.0.0.1"}})

	if code, _ := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "no.tunnel.test"); code != 403 {
		t.Errorf("not allowed: %d", code)
	}
	if code, _ := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "yes.tunnel.test"); code != 200 {
		t.Errorf("allowed: %d", code)
	}
}

func TestHTTPProtectionNeedsServerSupport(t *testing.T) {
	// No capabilities on either side, i.e. a server from before they existed.
	withCaps(t)
	ctrl, _ := httpServer(t, nil)
	for _, c := range []*client{
		{server: ctrl, token: testToken, proto: "http", local: "127.0.0.1:1", auth: "a:b"},
		{server: ctrl, token: testToken, proto: "http", local: "127.0.0.1:1", allow: []string{"10.0.0.1"}},
	} {
		var fe fatalError
		if err := c.run(); !errors.As(err, &fe) || !strings.Contains(err.Error(), "too old") {
			t.Errorf("got %v", err)
		}
	}
}

func TestHTTPReuse(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	reused, single := startWebApp(t), startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: reused.addr, subdomain: "reused"})
	// PROXY headers describe one visitor, so these get a stream per request.
	runClient(t, &client{server: ctrl, proto: "http", local: single.addr, subdomain: "single", proxyProto: true})

	for _, app := range []*webApp{reused, single} {
		sub := map[*webApp]string{reused: "reused", single: "single"}[app]
		httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", sub+".tunnel.test")
		app.conns.Store(0)
		for i := 0; i < 3; i++ {
			httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", sub+".tunnel.test")
		}
	}
	if n := reused.conns.Load(); n != 0 {
		t.Errorf("reused: %d new local connections, want 0", n)
	}
	if n := single.conns.Load(); n != 3 {
		t.Errorf("single: %d new local connections, want 3", n)
	}
}

func TestHTTPOldClient(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	app := startWebApp(t)
	oldClient(t, ctrl, "old", app.addr)
	for i := 0; i < 2; i++ {
		if code, body := httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "old.tunnel.test"); code != 200 || !strings.Contains(body, "xff=127.0.0.1") {
			t.Errorf("%d %q", code, body)
		}
	}
	if n := app.conns.Load(); n < 2 {
		t.Errorf("old clients need a stream per request, got %d local connections", n)
	}
}

// oldClient speaks the protocol exactly as warren 0.3.0 did.
func oldClient(t *testing.T, ctrl, sub, local string) {
	c, err := net.Dial("tcp", ctrl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, `{"type":"hello","version":1,"token":%q,"proto":"http","subdomain":%q}`+"\n", testToken, sub)
	br := bufio.NewReader(c)
	go func() {
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(line, &m)
			switch m["type"] {
			case "ping":
				fmt.Fprint(c, `{"type":"pong"}`+"\n")
			case "conn":
				go func(id string) {
					d, err := net.Dial("tcp", ctrl)
					if err != nil {
						return
					}
					fmt.Fprintf(d, `{"type":"join","id":%q}`+"\n", id)
					l, err := net.Dial("tcp", local)
					if err != nil {
						d.Close()
						return
					}
					pipe(d, l)
				}(m["id"].(string))
			}
		}
	}()
}

func TestHTTPUpgrade(t *testing.T) {
	ctrl, httpAddr := httpServer(t, nil)
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app"})
	httpGet(t, http.DefaultClient, "http://"+httpAddr+"/", "app.tunnel.test")

	c, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET /ws HTTP/1.1\r\nHost: app.tunnel.test\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("%v %v", resp, err)
	}
	fmt.Fprint(c, "ping\n")
	if line, err := br.ReadString('\n'); line != "ping\n" {
		t.Errorf("got %q, %v", line, err)
	}
}

func TestHTTPSAndRedirect(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeTestCert(t, cert, key)
	httpsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ctrl, httpAddr := httpServer(t, func(cfg *ServerConfig) {
		cfg.HTTPSAddr, cfg.HTTPSCert, cfg.HTTPSKey = httpsAddr, cert, key
		cfg.HTTPSRedirect = true
	})
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app"})

	tlsClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}}
	req, _ := http.NewRequest("GET", "https://"+httpsAddr+"/", nil)
	req.Host = "app.tunnel.test"
	var resp *http.Response
	waitFor(t, func() bool {
		var err error
		resp, err = tlsClient.Do(req)
		return err == nil && resp.StatusCode == 200
	})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Proto != "HTTP/2.0" || !strings.Contains(string(b), "proto=https") {
		t.Errorf("%s %q", resp.Proto, b)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ = http.NewRequest("GET", "http://"+httpAddr+"/a?b=c", nil)
	req.Host = "app.tunnel.test"
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, port, _ := net.SplitHostPort(httpsAddr)
	if want := "https://app.tunnel.test:" + port + "/a?b=c"; resp.StatusCode != 308 || resp.Header.Get("Location") != want {
		t.Errorf("%d %s, want %s", resp.StatusCode, resp.Header.Get("Location"), want)
	}
}

func TestHTTPRequestLog(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ctrl, httpAddr := httpServer(t, func(cfg *ServerConfig) { cfg.LogRequests = true })
	app := startWebApp(t)
	runClient(t, &client{server: ctrl, proto: "http", local: app.addr, subdomain: "app"})
	httpGet(t, http.DefaultClient, "http://"+httpAddr+"/hello?x=1", "app.tunnel.test")

	waitFor(t, func() bool {
		s := logs.String()
		return strings.Contains(s, "[alice] app.tunnel.test GET /hello?x=1 200") && // server
			regexp.MustCompile(`GET +200 +\d+ms +127\.0\.0\.1 +/hello\?x=1`).MatchString(s) // client
	})
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeTestCert(t *testing.T, certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"*.tunnel.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}
