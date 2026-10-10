package main

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pingInterval   = 20 * time.Second
	controlTimeout = 60 * time.Second
	streamTimeout  = 10 * time.Second
)

type UserConfig struct {
	Name       string `json:"name"`
	Token      string `json:"token"`
	MaxTunnels int    `json:"max_tunnels,omitempty"` // 0 = unlimited
}

type ServerConfig struct {
	Listen    string `json:"listen"`              // control port, e.g. ":7000"
	Domain    string `json:"domain"`              // e.g. "tunnel.example.com"
	TLSCert   string `json:"tls_cert,omitempty"`  // optional TLS for the control port
	TLSKey    string `json:"tls_key,omitempty"`   //
	HTTPAddr  string `json:"http_addr,omitempty"` // e.g. ":80"; empty disables http tunnels
	HTTPSAddr string `json:"https_addr,omitempty"`
	// Port shown in printed website URLs, when a reverse proxy in front of
	// warren serves them on a different port than http_addr/https_addr.
	PublicHTTPPort int          `json:"public_http_port,omitempty"`
	HTTPSCert      string       `json:"https_cert,omitempty"` // must cover *.domain
	HTTPSKey       string       `json:"https_key,omitempty"`
	PortMin        int          `json:"port_min"`
	PortMax        int          `json:"port_max"`
	Users          []UserConfig `json:"users"`
}

func loadServerConfig(path string) (*ServerConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &ServerConfig{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":7000"
	}
	if cfg.PortMin == 0 {
		cfg.PortMin = 1024
	}
	if cfg.PortMax == 0 {
		cfg.PortMax = 65535
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("%s: \"domain\" is required", path)
	}
	if cfg.PortMin < 1 || cfg.PortMax > 65535 || cfg.PortMin > cfg.PortMax {
		return nil, fmt.Errorf("%s: invalid port_min/port_max", path)
	}
	return cfg, nil
}

func defaultConfigPath() string {
	if p := os.Getenv("WARREN_CONFIG"); p != "" {
		return p
	}
	return "/etc/warren/server.json"
}

func saveServerConfig(path string, cfg *ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	// Keep the original owner, so running adduser as root doesn't leave a
	// file the server's own user can no longer read.
	if st, err := os.Stat(path); err == nil {
		copyOwner(st, tmp)
	}
	return os.Rename(tmp, path)
}

type Server struct {
	cfgPath string
	cfg     *ServerConfig
	httpOn  bool

	mu       sync.Mutex
	users    []UserConfig
	sessions map[*session]struct{}
	claims   map[string]*session      // "tcp/25565", "udp/19132", "http/name"
	pending  map[string]chan net.Conn // stream id -> waiting visitor
}

type session struct {
	srv   *Server
	user  string
	token string
	ctrl  net.Conn

	wmu       sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

func cmdServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to server config (or set WARREN_CONFIG)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := loadServerConfig(*cfgPath)
	if err != nil {
		return err
	}
	srv := &Server{
		cfgPath:  *cfgPath,
		cfg:      cfg,
		users:    cfg.Users,
		sessions: map[*session]struct{}{},
		claims:   map[string]*session{},
		pending:  map[string]chan net.Conn{},
	}
	return srv.run()
}

func (srv *Server) run() error {
	cfg := srv.cfg
	ln, err := listen(cfg.Listen, cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return err
	}
	log.Printf("warren %s: control port %s (tls=%v), domain %s, ports %d-%d, %d users",
		version, cfg.Listen, cfg.TLSCert != "", cfg.Domain, cfg.PortMin, cfg.PortMax, len(cfg.Users))
	if cfg.TLSCert == "" {
		log.Printf("WARNING: tls_cert is not set, so tokens travel unencrypted; anyone on the path can steal them")
	}

	if cfg.HTTPAddr != "" {
		hl, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return err
		}
		srv.httpOn = true
		log.Printf("http tunnels on %s", cfg.HTTPAddr)
		go srv.serveHTTP(hl)
	}
	if cfg.HTTPSAddr != "" {
		hl, err := listen(cfg.HTTPSAddr, cfg.HTTPSCert, cfg.HTTPSKey)
		if err != nil {
			return err
		}
		srv.httpOn = true
		log.Printf("https tunnels on %s", cfg.HTTPSAddr)
		go srv.serveHTTP(hl)
	}
	go srv.watchUsers()

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go srv.handleConn(c)
	}
}

// listen opens a TCP listener, wrapped in TLS when a cert is given.
func listen(addr, certFile, keyFile string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil || certFile == "" {
		return ln, err
	}
	cr := &certReloader{cert: certFile, key: keyFile}
	if _, err := cr.get(nil); err != nil {
		ln.Close()
		return nil, err
	}
	return tls.NewListener(ln, &tls.Config{GetCertificate: cr.get}), nil
}

// certReloader re-reads the certificate when the file changes on disk, so
// renewed Let's Encrypt certs are picked up without a restart.
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	mtime     time.Time
	cached    *tls.Certificate
}

func (cr *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	st, err := os.Stat(cr.cert)
	if err == nil && cr.cached != nil && st.ModTime().Equal(cr.mtime) {
		return cr.cached, nil
	}
	c, err := tls.LoadX509KeyPair(cr.cert, cr.key)
	if err != nil {
		if cr.cached != nil {
			log.Printf("reloading certificate: %v (keeping old one)", err)
			return cr.cached, nil
		}
		return nil, err
	}
	cr.cached = &c
	if st != nil {
		cr.mtime = st.ModTime()
	}
	return cr.cached, nil
}

func (srv *Server) handleConn(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := readMsg(c)
	if err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})

	switch m.Type {
	case "join":
		srv.mu.Lock()
		ch := srv.pending[m.ID]
		delete(srv.pending, m.ID)
		if ch != nil {
			ch <- c // buffered, and we're the only sender
		}
		srv.mu.Unlock()
		if ch == nil {
			c.Close()
		}
	case "hello":
		srv.handleSession(c, m)
	default:
		c.Close()
	}
}

// authenticate re-reads the config so newly added users work immediately.
func (srv *Server) authenticate(token string) *UserConfig {
	if cfg, err := loadServerConfig(srv.cfgPath); err == nil {
		srv.mu.Lock()
		srv.users = cfg.Users
		srv.mu.Unlock()
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i := range srv.users {
		u := srv.users[i]
		if u.Token != "" && subtle.ConstantTimeCompare([]byte(u.Token), []byte(token)) == 1 {
			return &u
		}
	}
	return nil
}

// watchUsers periodically reloads the user list and disconnects sessions
// whose user was removed.
func (srv *Server) watchUsers() {
	for range time.Tick(30 * time.Second) {
		cfg, err := loadServerConfig(srv.cfgPath)
		if err != nil {
			log.Printf("reloading config: %v", err)
			continue
		}
		valid := map[string]bool{}
		for _, u := range cfg.Users {
			valid[u.Token] = true
		}
		var kick []*session
		srv.mu.Lock()
		srv.users = cfg.Users
		for s := range srv.sessions {
			if !valid[s.token] {
				kick = append(kick, s)
			}
		}
		srv.mu.Unlock()
		for _, s := range kick {
			log.Printf("[%s] user removed, disconnecting", s.user)
			s.kick("your access was revoked")
		}
	}
}

func (srv *Server) handleSession(c net.Conn, hello Msg) {
	s := &session{srv: srv, ctrl: c, closed: make(chan struct{})}
	defer s.close()
	fail := func(fatal bool, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		log.Printf("[%s] rejected %s: %s", c.RemoteAddr(), s.user, msg)
		s.send(Msg{Type: "error", Error: msg, Fatal: fatal})
	}

	if hello.Version != protoVersion {
		fail(true, "protocol version mismatch (server %d, client %d): update warren", protoVersion, hello.Version)
		return
	}
	user := srv.authenticate(hello.Token)
	if user == nil {
		fail(true, "invalid token")
		return
	}
	s.user, s.token = user.Name, user.Token

	// Claim first: a reconnecting client asks for its old port/subdomain,
	// which kicks its own stale session, so that one isn't counted below.
	ok, cleanup, err := srv.setupTunnel(s, hello)
	if err != nil {
		fail(false, "%v", err)
		return
	}
	defer cleanup()

	srv.mu.Lock()
	n := 0
	for o := range srv.sessions {
		if o.user == s.user {
			n++
		}
	}
	if user.MaxTunnels > 0 && n >= user.MaxTunnels {
		srv.mu.Unlock()
		fail(false, "tunnel limit reached (%d)", user.MaxTunnels)
		return
	}
	srv.sessions[s] = struct{}{}
	srv.mu.Unlock()
	// Runs before cleanup releases the claim, so whoever claims it next
	// never counts this session.
	defer func() {
		srv.mu.Lock()
		delete(srv.sessions, s)
		srv.mu.Unlock()
	}()

	if err := s.send(ok); err != nil {
		return
	}
	log.Printf("[%s] opened %s (from %s)", s.user, ok.URL, c.RemoteAddr())

	go s.pinger()
	br := bufio.NewReader(c)
	for {
		c.SetReadDeadline(time.Now().Add(controlTimeout))
		m, err := readMsg(br)
		if err != nil {
			break
		}
		if m.Type == "ping" {
			s.send(Msg{Type: "pong"})
		}
	}
	s.close()
	log.Printf("[%s] closed %s", s.user, ok.URL)
}

var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// setupTunnel claims and binds the public side of a tunnel. cleanup unbinds
// it; it must run before the session is forgotten.
func (srv *Server) setupTunnel(s *session, hello Msg) (Msg, func(), error) {
	switch hello.Proto {
	case "tcp", "udp":
		port, cl, err := srv.bindPort(s, hello.Proto, hello.Port)
		if err != nil {
			return Msg{}, nil, err
		}
		key := fmt.Sprintf("%s/%d", hello.Proto, port)
		cleanup := func() { cl.Close(); srv.release(key, s) }
		url := fmt.Sprintf("%s://%s:%d", hello.Proto, srv.cfg.Domain, port)
		return Msg{Type: "ok", URL: url, Port: port}, cleanup, nil

	case "http":
		if !srv.httpOn {
			return Msg{}, nil, errors.New("http tunnels are not enabled on this server")
		}
		sub := strings.ToLower(hello.Subdomain)
		if sub == "" {
			for {
				sub = randHex(4)
				if srv.tryClaim("http/"+sub, s) {
					break
				}
			}
		} else {
			if !subdomainRe.MatchString(sub) {
				return Msg{}, nil, fmt.Errorf("invalid subdomain %q (use a-z, 0-9 and -)", sub)
			}
			if err := srv.claim("http/"+sub, s); err != nil {
				return Msg{}, nil, err
			}
		}
		cleanup := func() { srv.release("http/"+sub, s) }
		return Msg{Type: "ok", URL: srv.httpURL(sub), Subdomain: sub}, cleanup, nil
	}
	return Msg{}, nil, fmt.Errorf("unknown tunnel type %q", hello.Proto)
}

func (srv *Server) httpURL(sub string) string {
	host := sub + "." + srv.cfg.Domain
	scheme, addr := "http", srv.cfg.HTTPAddr
	if srv.cfg.HTTPSAddr != "" {
		scheme, addr = "https", srv.cfg.HTTPSAddr
	}
	_, port, _ := net.SplitHostPort(addr)
	if srv.cfg.PublicHTTPPort != 0 {
		port = strconv.Itoa(srv.cfg.PublicHTTPPort)
	}
	if (scheme == "http" && port != "80") || (scheme == "https" && port != "443") {
		host += ":" + port
	}
	return scheme + "://" + host
}

func (srv *Server) tryClaim(key string, s *session) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.claims[key] != nil {
		return false
	}
	srv.claims[key] = s
	return true
}

// claim reserves key for s. If the same user already holds it (typically a
// stale connection from before a reconnect) the old session is replaced.
func (srv *Server) claim(key string, s *session) error {
	deadline := time.Now().Add(3 * time.Second)
	var kicked *session
	for {
		if srv.tryClaim(key, s) {
			return nil
		}
		srv.mu.Lock()
		old := srv.claims[key]
		srv.mu.Unlock()
		if old != nil && old.user != s.user {
			return fmt.Errorf("%s is already in use by someone else", key)
		}
		if old != nil && old != kicked {
			kicked = old
			old.kick("replaced by a newer connection with the same port/subdomain")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is already in use", key)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (srv *Server) release(key string, s *session) {
	srv.mu.Lock()
	if srv.claims[key] == s {
		delete(srv.claims, key)
	}
	srv.mu.Unlock()
}

func (srv *Server) bindPort(s *session, proto string, want int) (int, io.Closer, error) {
	bind := func(port int) (io.Closer, error) {
		if proto == "tcp" {
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				return nil, err
			}
			go srv.serveTCP(ln, s)
			return ln, nil
		}
		pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, err
		}
		go srv.serveUDP(pc, s)
		return pc, nil
	}

	if want != 0 {
		if want < srv.cfg.PortMin || want > srv.cfg.PortMax {
			return 0, nil, fmt.Errorf("port %d is outside the allowed range %d-%d", want, srv.cfg.PortMin, srv.cfg.PortMax)
		}
		key := proto + "/" + strconv.Itoa(want)
		if err := srv.claim(key, s); err != nil {
			return 0, nil, err
		}
		cl, err := bind(want)
		if err != nil {
			srv.release(key, s)
			return 0, nil, fmt.Errorf("port %d is not available", want)
		}
		return want, cl, nil
	}

	for i := 0; i < 100; i++ {
		port := srv.cfg.PortMin + rand.Intn(srv.cfg.PortMax-srv.cfg.PortMin+1)
		key := proto + "/" + strconv.Itoa(port)
		if !srv.tryClaim(key, s) {
			continue
		}
		if cl, err := bind(port); err == nil {
			return port, cl, nil
		}
		srv.release(key, s)
	}
	return 0, nil, errors.New("no free port found")
}

func (srv *Server) serveTCP(ln net.Listener, s *session) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			st, err := s.openStream(c.RemoteAddr().String())
			if err != nil {
				c.Close()
				return
			}
			pipe(c, st)
		}()
	}
}

func (s *session) send(m Msg) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return writeMsg(s.ctrl, m)
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.ctrl.Close()
	})
}

// kick tells the client why it's being disconnected (best effort, the
// connection may already be dead) and closes the session.
func (s *session) kick(reason string) {
	s.wmu.Lock()
	s.ctrl.SetWriteDeadline(time.Now().Add(time.Second))
	writeMsg(s.ctrl, Msg{Type: "error", Error: reason, Fatal: true})
	s.wmu.Unlock()
	s.close()
}

func (s *session) pinger() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			if err := s.send(Msg{Type: "ping"}); err != nil {
				s.close()
				return
			}
		}
	}
}

// openStream asks the client to open a data connection for a new visitor
// and waits for it to arrive.
func (s *session) openStream(remote string) (net.Conn, error) {
	srv := s.srv
	id := randHex(16)
	ch := make(chan net.Conn, 1)
	srv.mu.Lock()
	srv.pending[id] = ch
	srv.mu.Unlock()
	defer func() {
		srv.mu.Lock()
		delete(srv.pending, id)
		srv.mu.Unlock()
		select {
		case c := <-ch: // arrived after we gave up
			c.Close()
		default:
		}
	}()

	if err := s.send(Msg{Type: "conn", ID: id, Remote: remote}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(streamTimeout)
	defer timer.Stop()
	select {
	case c := <-ch:
		return c, nil
	case <-timer.C:
		return nil, errors.New("client did not open a stream in time")
	case <-s.closed:
		return nil, errors.New("session closed")
	}
}

// ---- user management commands ----

func cmdAddUser(args []string) error {
	fs := flag.NewFlagSet("adduser", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to server config (or set WARREN_CONFIG)")
	maxTunnels := fs.Int("max-tunnels", 0, "max simultaneous tunnels (0 = unlimited)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: warren adduser [--config server.json] NAME")
	}
	cfg, err := loadServerConfig(*cfgPath)
	if err != nil {
		return err
	}
	for _, u := range cfg.Users {
		if u.Name == pos[0] {
			return fmt.Errorf("user %q already exists", pos[0])
		}
	}
	token := randHex(24)
	cfg.Users = append(cfg.Users, UserConfig{Name: pos[0], Token: token, MaxTunnels: *maxTunnels})
	if err := saveServerConfig(*cfgPath, cfg); err != nil {
		return err
	}

	_, port, _ := net.SplitHostPort(cfg.Listen)
	login := fmt.Sprintf("warren login --server %s:%s --token %s", cfg.Domain, port, token)
	if cfg.TLSCert != "" {
		login += " --tls"
		if cert, err := readCert(cfg.TLSCert); err == nil && cert.CheckSignatureFrom(cert) == nil {
			// Self-signed: clients can't verify it the usual way, so they
			// pin its key instead.
			login += " --fingerprint " + certFingerprint(cert)
		}
	}
	fmt.Printf("Added user %q. Send them this command:\n\n  %s\n\n", pos[0], login)
	return nil
}

// readCert parses the first certificate in a PEM file.
func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM certificate found", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// cmdFingerprint prints the fingerprint clients pin with --fingerprint.
func cmdFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to server config (or set WARREN_CONFIG)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := loadServerConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.TLSCert == "" {
		return errors.New("the control port doesn't use TLS (no tls_cert in the config)")
	}
	cert, err := readCert(cfg.TLSCert)
	if err != nil {
		return err
	}
	fmt.Println(certFingerprint(cert))
	return nil
}

func cmdDelUser(args []string) error {
	fs := flag.NewFlagSet("deluser", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to server config (or set WARREN_CONFIG)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: warren deluser [--config server.json] NAME")
	}
	cfg, err := loadServerConfig(*cfgPath)
	if err != nil {
		return err
	}
	kept := cfg.Users[:0]
	for _, u := range cfg.Users {
		if u.Name != pos[0] {
			kept = append(kept, u)
		}
	}
	if len(kept) == len(cfg.Users) {
		return fmt.Errorf("no user %q", pos[0])
	}
	cfg.Users = kept
	if err := saveServerConfig(*cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Removed %q. Their tunnels will be closed within 30 seconds.\n", pos[0])
	return nil
}

func cmdUsers(args []string) error {
	fs := flag.NewFlagSet("users", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to server config (or set WARREN_CONFIG)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := loadServerConfig(*cfgPath)
	if err != nil {
		return err
	}
	for _, u := range cfg.Users {
		limit := "unlimited"
		if u.MaxTunnels > 0 {
			limit = strconv.Itoa(u.MaxTunnels)
		}
		fmt.Printf("%-20s tunnels: %s\n", u.Name, limit)
	}
	return nil
}
