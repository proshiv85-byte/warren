package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
)

// Website tunnels go through a reverse proxy: the server parses each request,
// applies the tunnel's access rules, adds X-Forwarded-* headers and sends it
// to the client over a stream as plain HTTP/1.1. Visitors can use HTTP/2 when
// the server has HTTPS. Websockets work through the proxy's protocol upgrade.

type visitorKey struct{}

// visitor is what a proxied request carries in its context.
type visitor struct {
	s    *session
	addr string // ip:port of the visitor, after trusted proxies
}

// maxHeaderBytes caps how much of a request is read before it's routed,
// so nobody can make the server buffer endless headers.
const maxHeaderBytes = 64 << 10

// serveHTTP serves website tunnels on ln, with TLS when tc is not nil.
func (srv *Server) serveHTTP(ln net.Listener, tc *tls.Config) {
	var h http.Handler = http.HandlerFunc(srv.handleHTTP)
	if tc == nil {
		h = srv.certs.httpHandler(h)
	}
	hs := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes, // larger requests get a 431
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0), // TLS handshake noise from scanners
		TLSConfig:         tc,
	}
	var err error
	if tc != nil {
		err = hs.ServeTLS(ln, "", "") // also enables HTTP/2
	} else {
		err = hs.Serve(ln)
	}
	log.Printf("website tunnels on %s stopped: %v", ln.Addr(), err)
}

func (srv *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	sub, ok := strings.CutSuffix(host, "."+srv.cfg.Domain)
	if !ok || sub == "" || strings.Contains(sub, ".") {
		httpError(w, 404, "warren: no tunnel here")
		return
	}
	if r.TLS == nil && srv.cfg.HTTPSRedirect && !srv.forwardedHTTPS(r) {
		http.Redirect(w, r, srv.httpURL(sub)+r.URL.RequestURI(), http.StatusPermanentRedirect)
		return
	}

	srv.mu.Lock()
	s := srv.claims["http/"+sub]
	srv.mu.Unlock()
	if s == nil {
		httpError(w, 404, fmt.Sprintf("warren: tunnel %q is not online", sub))
		return
	}

	ip, addr := srv.visitorAddr(r)
	lw := &logWriter{ResponseWriter: w}
	start := time.Now()
	defer func() { s.logRequest(r, lw, ip, time.Since(start)) }()

	// Requests in flight (including open websockets) count as visitors.
	if !s.reserveConn() {
		httpError(lw, 503, "warren: this site is too busy right now, try again in a moment")
		return
	}
	defer s.releaseConn()

	if !s.allowed(ip) {
		httpError(lw, 403, "warren: your IP address is not allowed to use this tunnel")
		return
	}
	if s.auth != "" {
		user, pass, _ := r.BasicAuth()
		if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(s.auth)) != 1 {
			lw.Header().Set("WWW-Authenticate", `Basic realm="warren", charset="UTF-8"`)
			httpError(lw, 401, "warren: this tunnel needs a username and password")
			return
		}
		// The password is for the tunnel, not for the app behind it.
		r.Header.Del("Authorization")
	}
	r = r.WithContext(context.WithValue(r.Context(), visitorKey{}, visitor{s, addr}))
	srv.proxy.ServeHTTP(lw, r)
}

func newProxy(srv *Server) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = pr.In.Host // Out.Host is left alone, so the app sees the original Host
			ap, err := netip.ParseAddrPort(pr.In.RemoteAddr)
			trusted := err == nil && srv.trusted(ap.Addr().Unmap())
			if trusted {
				// Keep the chain the proxy in front of us started.
				if v := pr.In.Header.Values("X-Forwarded-For"); len(v) > 0 {
					pr.Out.Header["X-Forwarded-For"] = v
				}
			}
			pr.SetXForwarded()
			if trusted {
				if v := pr.In.Header.Get("X-Forwarded-Proto"); v != "" {
					pr.Out.Header.Set("X-Forwarded-Proto", v)
				}
			}
		},
		Transport: tunnelTransport{},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the visitor went away
			}
			httpError(w, 502, "warren: the tunnel client did not respond")
		},
	}
}

// tunnelTransport sends a request to the client of the tunnel it's for.
type tunnelTransport struct{}

func (tunnelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	v, _ := r.Context().Value(visitorKey{}).(visitor)
	if v.s == nil {
		return nil, errors.New("request without a tunnel")
	}
	return v.s.transport.RoundTrip(r)
}

// newTransport makes the session's HTTP client. Without "http-reuse" every
// request gets a fresh stream, because an older client (or one sending
// PROXY headers) treats a stream as belonging to a single visitor.
func (s *session) newTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			v, _ := ctx.Value(visitorKey{}).(visitor)
			return s.openStream(v.addr)
		},
		DisableKeepAlives:   !s.has("http-reuse"),
		DisableCompression:  true, // pass the app's encoding through untouched
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
}

// visitorAddr returns the visitor's IP and ip:port. Behind a trusted proxy
// that's the last address the proxy added to X-Forwarded-For.
func (srv *Server) visitorAddr(r *http.Request) (netip.Addr, string) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, r.RemoteAddr
	}
	ip := ap.Addr().Unmap()
	if srv.trusted(ip) {
		if v := r.Header.Values("X-Forwarded-For"); len(v) > 0 {
			hops := strings.Split(v[len(v)-1], ",")
			if fip, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
				fip = fip.Unmap()
				return fip, netip.AddrPortFrom(fip, 0).String()
			}
		}
	}
	return ip, netip.AddrPortFrom(ip, ap.Port()).String()
}

func (srv *Server) trusted(ip netip.Addr) bool {
	for _, p := range srv.trustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedHTTPS reports whether a trusted proxy in front of warren already
// received this request over HTTPS.
func (srv *Server) forwardedHTTPS(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && srv.trusted(ap.Addr().Unmap()) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *session) allowed(ip netip.Addr) bool {
	if len(s.allow) == 0 {
		return true
	}
	for _, p := range s.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// parsePrefixes parses IP addresses and CIDR ranges.
func parsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, v := range list {
		v = strings.TrimSpace(v)
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked())
		} else if ip, err := netip.ParseAddr(v); err == nil {
			out = append(out, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		} else {
			return nil, fmt.Errorf("%q is not an IP address or range (like 203.0.113.7 or 203.0.113.0/24)", v)
		}
	}
	return out, nil
}

// logRequest records a finished request in the server log (if enabled) and
// sends it to the client (if it asked for request logs).
func (s *session) logRequest(r *http.Request, lw *logWriter, ip netip.Addr, d time.Duration) {
	status := lw.status
	if status == 0 {
		status = 502 // the proxy gave up without writing anything
		if r.Context().Err() != nil {
			status = 499 // the visitor left first
		}
	}
	req := &ReqLog{Method: r.Method, Path: r.URL.RequestURI(), Status: status,
		Ms: d.Milliseconds(), Bytes: lw.bytes, Remote: ip.String()}
	if s.srv.cfg.LogRequests {
		log.Printf("[%s] %s %s %s %d %dms %s", s.user, r.Host, req.Method, req.Path, req.Status, req.Ms, req.Remote)
	}
	if s.has("http-log") {
		select {
		case s.logs <- Msg{Type: "req", Req: req}:
		default: // the client can't keep up; logs are best effort
		}
	}
}

// sendLogs forwards request logs to the client until the session ends.
func (s *session) sendLogs() {
	for {
		select {
		case m := <-s.logs:
			if s.send(m) != nil {
				return
			}
		case <-s.closed:
			return
		}
	}
}

// logWriter records the status and size of a response.
type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *logWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *logWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return c, brw, err
}

func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func httpError(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}

// rawHTTPError writes a whole response to a connection that isn't served by
// net/http, then closes it.
func rawHTTPError(w io.WriteCloser, code int, msg string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n",
		code, http.StatusText(code), len(msg)+1, msg)
	w.Close()
}
