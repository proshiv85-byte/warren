package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Automatic certificates: with "acme" on, every website tunnel gets its own
// certificate from Let's Encrypt (or acme_ca) when it opens, so HTTPS works
// without a wildcard certificate or a DNS API. Validation uses TLS-ALPN on
// the HTTPS port, or HTTP on http_addr when that's reachable on port 80.

// certSource picks the certificate for a website tunnel's TLS handshake.
type certSource struct {
	srv  *Server
	file *certReloader     // https_cert/https_key, may be nil
	acme *autocert.Manager // may be nil
}

func newCertSource(srv *Server) (*certSource, error) {
	cfg := srv.cfg
	cs := &certSource{srv: srv}
	if cfg.HTTPSCert != "" {
		cs.file = &certReloader{cert: cfg.HTTPSCert, key: cfg.HTTPSKey}
		if _, err := cs.file.get(nil); err != nil {
			return nil, err
		}
	}
	if cfg.ACME {
		dir := cfg.ACMEDir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(srv.cfgPath), "acme")
		}
		cs.acme = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(dir),
			HostPolicy: srv.acmePolicy,
			Email:      cfg.ACMEEmail,
		}
		if cfg.ACMECA != "" {
			cs.acme.Client = &acme.Client{DirectoryURL: cfg.ACMECA}
		}
	}
	if cs.file == nil && cs.acme == nil {
		return nil, errors.New("https_addr needs https_cert and https_key, or \"acme\": true")
	}
	return cs, nil
}

func (cs *certSource) tlsConfig() *tls.Config {
	tc := &tls.Config{GetCertificate: cs.get}
	if cs.acme != nil {
		tc.NextProtos = []string{"h2", "http/1.1", acme.ALPNProto}
	}
	return tc
}

func (cs *certSource) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	challenge := len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto
	if cs.file != nil && !challenge {
		c, err := cs.file.get(hello)
		if err == nil && (cs.acme == nil || covers(c, hello.ServerName)) {
			return c, nil
		}
	}
	if cs.acme == nil {
		return nil, fmt.Errorf("no certificate for %q", hello.ServerName)
	}
	return cs.acme.GetCertificate(hello)
}

// covers reports whether c is valid for name, so a configured wildcard
// certificate is used for the names it covers and ACME for the rest.
func covers(c *tls.Certificate, name string) bool {
	leaf := c.Leaf
	if leaf == nil && len(c.Certificate) > 0 {
		leaf, _ = x509.ParseCertificate(c.Certificate[0])
	}
	return leaf != nil && name != "" && leaf.VerifyHostname(name) == nil
}

// httpHandler answers ACME HTTP challenges on the plain-HTTP listener.
func (cs *certSource) httpHandler(h http.Handler) http.Handler {
	if cs == nil || cs.acme == nil {
		return h
	}
	return cs.acme.HTTPHandler(h)
}

// acmePolicy only lets certificates be requested for tunnels that are
// online, so a stranger can't burn through the rate limits by sending
// handshakes for made-up names.
func (srv *Server) acmePolicy(_ context.Context, host string) error {
	sub, ok := strings.CutSuffix(strings.ToLower(host), "."+srv.cfg.Domain)
	if !ok || !subdomainRe.MatchString(sub) {
		return fmt.Errorf("acme: %q is not a website tunnel", host)
	}
	srv.mu.Lock()
	online := srv.claims["http/"+sub] != nil
	srv.mu.Unlock()
	if !online {
		return fmt.Errorf("acme: tunnel %q is not online", sub)
	}
	return nil
}

// warmCert gets the tunnel's certificate as soon as it opens, rather than
// making the first visitor wait for it, and tells the client if that fails.
func (srv *Server) warmCert(s *session, sub string) {
	cs := srv.certs
	if cs == nil || cs.acme == nil {
		return
	}
	host := sub + "." + srv.cfg.Domain
	if cs.file != nil {
		if c, err := cs.file.get(nil); err == nil && covers(c, host) {
			return
		}
	}
	start := time.Now()
	// Look like a modern client, so the ECDSA certificate browsers will
	// ask for is the one that gets issued.
	_, err := cs.acme.GetCertificate(&tls.ClientHelloInfo{
		ServerName:        host,
		SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:   []tls.CurveID{tls.CurveP256},
		CipherSuites:      []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	})
	if err != nil {
		log.Printf("[%s] certificate for %s: %v", s.user, host, err)
		if s.has("notice") {
			s.send(Msg{Type: "notice", Error: "could not get an HTTPS certificate for " + host + ": " + err.Error()})
		}
		return
	}
	if d := time.Since(start); d > time.Second {
		log.Printf("[%s] got a certificate for %s in %s", s.user, host, d.Round(100*time.Millisecond))
	}
}
