package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

// Wire protocol
//
// Every connection to the server's control port starts with one JSON line.
//
//   Control connection (one per tunnel):
//     client -> {"type":"hello","version":1,"token":..,"proto":"tcp","port":25565}
//     server -> {"type":"ok","url":..,"port":25565}  or  {"type":"error",..}
//     then server sends {"type":"conn","id":..,"remote":..} for every visitor
//     and {"type":"ping"} periodically; the client answers {"type":"pong"}.
//
//   Data connection (one per visitor):
//     client -> {"type":"join","id":..}
//     after that the connection carries raw visitor bytes. For UDP tunnels it
//     carries datagrams framed as a 2-byte big-endian length + payload.

const protoVersion = 1

type Msg struct {
	Type      string `json:"type"`
	Version   int    `json:"version,omitempty"`
	Token     string `json:"token,omitempty"`
	Proto     string `json:"proto,omitempty"`
	Port      int    `json:"port,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	ID        string `json:"id,omitempty"`
	Remote    string `json:"remote,omitempty"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
	Fatal     bool   `json:"fatal,omitempty"`
}

// readLine reads one '\n'-terminated line a byte at a time, so nothing past
// the line is consumed from r. Wrap r in a bufio.Reader when that's not needed.
func readLine(r io.Reader, max int) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < max {
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
	return nil, errors.New("line too long")
}

func readMsg(r io.Reader) (Msg, error) {
	var m Msg
	line, err := readLine(r, 8192)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(line, &m)
	return m, err
}

func writeMsg(w io.Writer, m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// certFingerprint identifies a certificate by the SHA-256 of its public key,
// so it stays the same when the certificate is re-issued with the same key.
func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizeFingerprint accepts "sha256:ab12...", plain hex, or hex with
// colons, and returns the "sha256:<64 lowercase hex>" form.
func normalizeFingerprint(fp string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(fp))
	h = strings.TrimPrefix(h, "sha256:")
	h = strings.ReplaceAll(h, ":", "")
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("invalid fingerprint %q (expected sha256: followed by 64 hex digits)", fp)
	}
	return "sha256:" + h, nil
}

type closeWriter interface{ CloseWrite() error }

// pipe copies data both ways between a and b, propagating half-closes, and
// closes both once both directions are done or either one fails.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

func writeFrame(w io.Writer, p []byte) error {
	if len(p) > 0xffff {
		return errors.New("datagram too large")
	}
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	_, err := io.ReadFull(r, p)
	return p, err
}

// parseArgs parses flags that may appear before or after positional
// arguments ("warren tcp 25565 --port 25565") and returns the positionals.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
