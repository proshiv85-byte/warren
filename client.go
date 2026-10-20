package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const defaultControlPort = "7000"

type clientConfig struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	TLS      bool   `json:"tls,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	// Pinned server key ("sha256:..."), for self-signed certificates.
	Fingerprint string `json:"fingerprint,omitempty"`
}

func clientConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "warren", "client.json"), nil
}

func loadClientConfig() clientConfig {
	var cfg clientConfig
	if p, err := clientConfigPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &cfg)
		}
	}
	if v := os.Getenv("WARREN_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("WARREN_TOKEN"); v != "" {
		cfg.Token = v
	}
	return cfg
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "server address, host[:port]")
	token := fs.String("token", "", "your token")
	useTLS := fs.Bool("tls", false, "connect to the server using TLS")
	insecure := fs.Bool("insecure", false, "with --tls, skip certificate verification (prefer --fingerprint)")
	fingerprint := fs.String("fingerprint", "", "with --tls, trust only the server key with this fingerprint (for self-signed certificates)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *server == "" || *token == "" {
		return errors.New("usage: warren login --server HOST[:PORT] --token TOKEN [--tls] [--fingerprint sha256:...]")
	}
	if *fingerprint != "" {
		fp, err := normalizeFingerprint(*fingerprint)
		if err != nil {
			return err
		}
		*fingerprint, *useTLS = fp, true
	}
	p, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(clientConfig{Server: *server, Token: *token, TLS: *useTLS,
		Insecure: *insecure, Fingerprint: *fingerprint}, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("Saved to %s\n", p)
	switch {
	case !*useTLS:
		fmt.Println("Warning: without --tls your token is sent unencrypted.")
	case *insecure && *fingerprint == "":
		fmt.Println("Warning: --insecure accepts any certificate, so your token can be intercepted.\n" +
			"Ask the server owner for the fingerprint (\"warren fingerprint\" on the server) and use --fingerprint instead.")
	}
	fmt.Println("Now try: warren tcp 25565")
	return nil
}

type client struct {
	server     string
	token      string
	useTLS     bool
	insecure   bool
	pin        string
	proto      string
	local      string
	port       int
	subdomain  string
	proxyProto bool
	auth       string   // http: "user:password"
	allow      []string // http: IPs and CIDR ranges
	online     bool
	caps       []string      // negotiated with the server for the current session
	stop       chan struct{} // closing it ends run; nil runs forever
}

// listFlag collects a flag that may be repeated or comma-separated.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

type fatalError struct{ msg string }

func (e fatalError) Error() string { return e.msg }

func cmdTunnel(proto string, args []string) error {
	cfg := loadClientConfig()
	fs := flag.NewFlagSet(proto, flag.ExitOnError)
	server := fs.String("server", cfg.Server, "server address, host[:port] (default: from warren login)")
	token := fs.String("token", cfg.Token, "auth token (default: from warren login)")
	useTLS := fs.Bool("tls", cfg.TLS, "connect to the server using TLS")
	insecure := fs.Bool("insecure", cfg.Insecure, "with --tls, skip certificate verification (prefer --fingerprint)")
	fingerprint := fs.String("fingerprint", cfg.Fingerprint, "with --tls, trust only the server key with this fingerprint")
	c := &client{proto: proto}
	if proto == "http" {
		fs.StringVar(&c.subdomain, "subdomain", "", "subdomain to request (default: random)")
		fs.StringVar(&c.auth, "auth", "", "require this `user:password` from visitors (basic auth)")
		fs.Var((*listFlag)(&c.allow), "allow", "only let in these `IPs` or ranges, e.g. 203.0.113.7,198.51.100.0/24 (repeatable)")
	} else {
		fs.IntVar(&c.port, "port", 0, "public port to request on the server (default: random)")
	}
	if proto != "udp" {
		fs.BoolVar(&c.proxyProto, "proxy-protocol", false, "send a PROXY protocol v1 header so the local server sees real visitor IPs")
	}
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: warren %s LOCAL [flags]\n\nLOCAL is a port (25565) or host:port.\n\n", proto)
		fs.PrintDefaults()
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if *server == "" || *token == "" {
		return errors.New("no server/token configured: run \"warren login --server HOST --token TOKEN\" first")
	}
	if u, p, ok := strings.Cut(c.auth, ":"); c.auth != "" && (!ok || u == "" || p == "") {
		return errors.New("--auth must be user:password")
	}
	if _, err := parsePrefixes(c.allow); err != nil {
		return fmt.Errorf("--allow: %w", err)
	}

	c.server, c.token, c.useTLS, c.insecure = *server, *token, *useTLS, *insecure
	if *fingerprint != "" {
		if c.pin, err = normalizeFingerprint(*fingerprint); err != nil {
			return err
		}
		c.useTLS = true
	}
	if _, _, err := net.SplitHostPort(c.server); err != nil {
		c.server = net.JoinHostPort(c.server, defaultControlPort)
	}
	c.local = pos[0]
	if _, err := strconv.Atoi(c.local); err == nil {
		c.local = "127.0.0.1:" + c.local
	}
	return c.run()
}

func (c *client) run() error {
	backoff := time.Second
	for {
		start := time.Now()
		err := c.session()
		var fe fatalError
		if errors.As(err, &fe) {
			return err
		}
		if !c.online && errors.Is(err, errPinMismatch) {
			// A different key won't change by retrying: either the server
			// got a new certificate or someone is intercepting.
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if !c.online && errors.Is(err, io.EOF) && !c.useTLS {
			err = fmt.Errorf("%w (if the server uses TLS, add --tls)", err)
		}
		select {
		case <-c.stop:
			return nil
		default:
		}
		log.Printf("disconnected (%v), reconnecting in %s", err, backoff)
		select {
		case <-c.stop:
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *client) dial() (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !c.useTLS {
		return d.Dial("tcp", c.server)
	}
	host, _, _ := net.SplitHostPort(c.server)
	cfg := &tls.Config{ServerName: host, InsecureSkipVerify: c.insecure}
	if c.pin != "" {
		// The pin replaces the usual CA check (self-signed certificates
		// have no CA), so a mismatch is the only thing that fails.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errPinMismatch
			}
			if got := certFingerprint(cs.PeerCertificates[0]); got != c.pin {
				return fmt.Errorf("%w: server has %s, expected %s", errPinMismatch, got, c.pin)
			}
			return nil
		}
	}
	return tls.DialWithDialer(d, "tcp", c.server, cfg)
}

var errPinMismatch = errors.New("the server's certificate doesn't match --fingerprint " +
	"(if the server owner made a new certificate, ask them for the new fingerprint)")

func (c *client) session() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-c.stop:
			conn.Close()
		case <-ended:
		}
	}()

	conn.SetDeadline(time.Now().Add(15 * time.Second))
	err = writeMsg(conn, Msg{Type: "hello", Version: protoVersion, Token: c.token,
		Proto: c.proto, Port: c.port, Subdomain: c.subdomain, Caps: c.offerCaps(),
		Auth: c.auth, Allow: c.allow})
	if err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	m, err := readMsg(br)
	if err != nil {
		return err
	}
	if m.Type == "error" {
		if m.Fatal || !c.online {
			// Errors on the very first connect (port taken, bad
			// subdomain...) won't fix themselves, so don't retry forever.
			return fatalError{"server: " + m.Error}
		}
		return errors.New("server: " + m.Error)
	}
	if m.Type != "ok" {
		return fmt.Errorf("unexpected reply %q", m.Type)
	}
	conn.SetDeadline(time.Time{})
	// Only what the server echoed back counts; an older server sends nothing.
	c.caps = negotiateCaps(m.Caps)
	// An older server ignores options it doesn't know, which would leave the
	// site open; hang up before anyone can visit.
	if c.auth != "" && !c.has("http-auth") {
		return fatalError{"this server is too old to protect tunnels with --auth: ask the server admin to run \"warren update\""}
	}
	if len(c.allow) > 0 && !c.has("http-allow") {
		return fatalError{"this server is too old to restrict tunnels with --allow: ask the server admin to run \"warren update\""}
	}

	// Ask for the same port/subdomain again if we have to reconnect.
	c.port, c.subdomain = m.Port, m.Subdomain
	if !c.online {
		fmt.Printf("\n  warren is online\n\n  %s  ->  %s\n\n", m.URL, c.local)
		if c.proto != "http" {
			fmt.Printf("  people connect to: %s\n\n", strings.TrimPrefix(m.URL, c.proto+"://"))
		}
		if c.auth != "" {
			fmt.Printf("  visitors need the password for user %q\n", strings.SplitN(c.auth, ":", 2)[0])
		}
		if len(c.allow) > 0 {
			fmt.Printf("  only open to: %s\n", strings.Join(c.allow, ", "))
		}
		if c.auth != "" || len(c.allow) > 0 {
			fmt.Println()
		}
		c.online = true
	} else {
		log.Printf("reconnected: %s", m.URL)
	}
	logConn := !c.has("http-log")
	if c.has("pool") && m.ID != "" {
		done := make(chan struct{})
		defer close(done)
		c.keepPool(m.ID, done, logConn)
	}

	for {
		conn.SetReadDeadline(time.Now().Add(controlTimeout))
		m, err := readMsg(br)
		if err != nil {
			return err
		}
		switch m.Type {
		case "ping":
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := writeMsg(conn, Msg{Type: "pong"}); err != nil {
				return err
			}
		case "conn":
			go c.handleStream(m.ID, m.Remote, logConn)
		case "req":
			if r := m.Req; r != nil {
				log.Printf("%-7s %3d %6dms  %-15s %s", r.Method, r.Status, r.Ms, r.Remote, r.Path)
			}
		case "error":
			if m.Fatal {
				return fatalError{"server: " + m.Error}
			}
			return errors.New("server: " + m.Error)
		}
	}
}

// offerCaps lists the capabilities to ask the server for.
func (c *client) offerCaps() []string {
	var caps []string
	for _, cp := range supportedCaps {
		// A PROXY header describes one visitor, so each one needs its own stream.
		if cp == "http-reuse" && c.proxyProto {
			continue
		}
		caps = append(caps, cp)
	}
	return caps
}

func (c *client) has(cp string) bool { return slices.Contains(c.caps, cp) }

// handleStream opens a data connection for a visitor the server announced.
func (c *client) handleStream(id, remote string, logConn bool) {
	data, err := c.dial()
	if err != nil {
		log.Printf("opening stream: %v", err)
		return
	}
	data.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := writeMsg(data, Msg{Type: "join", ID: id}); err != nil {
		data.Close()
		return
	}
	data.SetWriteDeadline(time.Time{})
	c.serveStream(data, remote, logConn)
}

// serveStream connects a visitor's data connection to the local service.
// logConn says whether to log it (website tunnels with "http-log" log each
// request instead).
func (c *client) serveStream(data net.Conn, remote string, logConn bool) {
	if c.proto == "udp" {
		clientUDP(data, c.local, remote)
		return
	}
	local, err := net.DialTimeout("tcp", c.local, 5*time.Second)
	if err != nil {
		log.Printf("connection from %s, but nothing is listening on %s: %v", remote, c.local, err)
		if c.proto == "http" {
			rawHTTPError(data, 502, "warren: the local service at "+c.local+" is not running")
		} else {
			data.Close()
		}
		return
	}
	if logConn {
		log.Printf("connection from %s", remote)
	}
	if c.proxyProto {
		local.Write([]byte(proxyHeader(remote, local.RemoteAddr().String())))
	}
	pipe(data, local)
}

// proxyHeader builds a PROXY protocol v1 line describing the visitor.
func proxyHeader(src, dst string) string {
	sh, sp, err1 := net.SplitHostPort(src)
	dh, dp, err2 := net.SplitHostPort(dst)
	sip, dip := net.ParseIP(sh), net.ParseIP(dh)
	if err1 != nil || err2 != nil || sip == nil || dip == nil {
		return "PROXY UNKNOWN\r\n"
	}
	if s4 := sip.To4(); s4 != nil {
		d4 := dip.To4()
		if d4 == nil {
			d4 = net.IPv4zero
		}
		return fmt.Sprintf("PROXY TCP4 %s %s %s %s\r\n", s4, d4, sp, dp)
	}
	if dip.To4() != nil {
		dip = net.IPv6unspecified
	}
	return fmt.Sprintf("PROXY TCP6 %s %s %s %s\r\n", sip, dip, sp, dp)
}
