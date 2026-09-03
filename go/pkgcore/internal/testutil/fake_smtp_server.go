// Package testutil holds shared test helpers used by more than one of
// go/pkgcore's own test files, per the backend coding standard's
// "shared test helpers go in a dedicated internal/testutil package" rule.
// Being under go/pkgcore/internal, it is importable only from within the
// go/pkgcore module tree — package pkgcore's own test files (both the
// internal _test.go files and the external pkgcore_test ones) and its
// exported contract-test packages (eventbustest, kvstoretest, mailertest,
// objectstoretest), never from outside the module.
//
// FakeSMTPServer originated as unexported helpers inside smtp_mailer_test.go
// (package pkgcore) and moved here once a second consumer needed it:
// mailer_conformance_test.go (package pkgcore_test, see that file's own
// doc comment) proves pkgcore.NewSMTPMailer against mailertest.AssertConforms
// using the same scripted relay smtp_mailer_test.go's own wire-level tests
// already use, rather than a second, drifting copy of the same fake.
//
// This package deliberately knows nothing about pkgcore.Mailer or
// pkgcore.SMTPConfig — it scripts the SMTP wire protocol only. Package
// pkgcore's own smtp_mailer_test.go is package pkgcore itself (it needs
// direct access to unexported symbols like smtpMailer and buildMessage), so
// if this package imported pkgcore, that file's use of it would be an
// import cycle: pkgcore's own test binary -> testutil -> pkgcore. Each
// consumer therefore builds its own thin pkgcore.Mailer wrapper around
// FakeSMTPServer.Addr() — see smtp_mailer_test.go's mailerFor and
// mailer_conformance_test.go's equivalent.
package testutil

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeSMTPOptions shapes the scripted relay a test talks to.
type FakeSMTPOptions struct {
	// Caps are the EHLO capabilities advertised in order, one per reply
	// line, e.g. "AUTH PLAIN". STARTTLS is appended automatically when Cert
	// is set.
	Caps []string

	// Cert, when set, makes the relay advertise STARTTLS and upgrade with
	// this certificate when the client asks.
	Cert *tls.Certificate

	// Implicit wraps the listener in TLS from the first byte, for relays
	// that speak TLS-only SMTP.
	Implicit bool

	// Hang makes the relay answer the greeting but never reply to any
	// command, which is what a dead relay looks like mid-transaction.
	Hang bool

	// Reject, when set, answers 550 to every RCPT it reports true for.
	Reject func(rcpt string) bool
}

// SMTPExchange is everything the relay saw in one MAIL..DATA transaction.
type SMTPExchange struct {
	Auth    string   // the whole "AUTH PLAIN ..." line, empty when none came
	From    string   // the whole "MAIL FROM:<...>" line
	Rcpts   []string // whole "RCPT TO:<...>" lines
	Msg     string   // dot-unstuffed message body as received
	ConnTLS bool     // whether the connection was TLS by the time DATA ran
}

// FakeSMTPServer is a scripted SMTP relay for exercising a Mailer without a
// real one. It answers each command with a fixed success reply, records
// every exchange, and plays the failure scripts its options describe.
type FakeSMTPServer struct {
	t    *testing.T
	ln   net.Listener
	opts FakeSMTPOptions

	mu        sync.Mutex
	exchanges []SMTPExchange
}

// StartFakeSMTPServer listens on an ephemeral localhost port and cleans up
// at the end of the test.
func StartFakeSMTPServer(t *testing.T, opts FakeSMTPOptions) *FakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake SMTP relay: listen: %v", err)
	}
	if opts.Implicit {
		if opts.Cert == nil {
			_ = ln.Close()
			t.Fatal("fake SMTP relay: implicit TLS requires a certificate")
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*opts.Cert}})
	}
	s := &FakeSMTPServer{t: t, ln: ln, opts: opts}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// Addr returns the relay's 127.0.0.1:port address.
func (s *FakeSMTPServer) Addr() string { return s.ln.Addr().String() }

// Take returns the recorded exchanges, emptying the record.
func (s *FakeSMTPServer) Take() []SMTPExchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	exchanges := s.exchanges
	s.exchanges = nil
	return exchanges
}

func (s *FakeSMTPServer) record(ex SMTPExchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = append(s.exchanges, ex)
}

func (s *FakeSMTPServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// handle runs one SMTP session: greet, then answer commands until the
// client quits or the connection dies. The greeting is the relay's first
// word, and on an implicit-TLS listener it also triggers the lazy
// server-side handshake.
func (s *FakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "220 fake ESMTP ready\r\n")

	br := bufio.NewReader(conn)
	connTLS := s.opts.Implicit
	ex := SMTPExchange{}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(cmd, " ", 2)
		switch strings.ToUpper(parts[0]) {
		case "EHLO", "HELO":
			if s.opts.Hang {
				continue // leave the client waiting forever
			}
			s.writeCapabilities(conn)
		case "STARTTLS":
			_, _ = fmt.Fprint(conn, "220 2.0.0 Ready to start TLS\r\n")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*s.opts.Cert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			br = bufio.NewReader(conn)
			connTLS = true
		case "AUTH":
			ex.Auth = cmd
			_, _ = fmt.Fprint(conn, "235 2.7.0 Authentication successful\r\n")
		case "MAIL":
			ex.From = cmd
			_, _ = fmt.Fprint(conn, "250 2.1.0 Ok\r\n")
		case "RCPT":
			ex.Rcpts = append(ex.Rcpts, cmd)
			if s.opts.Reject != nil && s.opts.Reject(cmd) {
				_, _ = fmt.Fprint(conn, "550 5.1.1 No such user\r\n")
				continue
			}
			_, _ = fmt.Fprint(conn, "250 2.1.5 Ok\r\n")
		case "DATA":
			_, _ = fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
			var body []string
			for {
				dataLine, err := br.ReadString('\n')
				if err != nil {
					return
				}
				trimmed := strings.TrimRight(dataLine, "\r\n")
				if trimmed == "." {
					break
				}
				if strings.HasPrefix(trimmed, "..") {
					trimmed = trimmed[1:] // dot-unstuff
				}
				body = append(body, trimmed)
			}
			ex.Msg = strings.Join(body, "\r\n")
			ex.ConnTLS = connTLS
			s.record(ex)
			ex = SMTPExchange{}
			_, _ = fmt.Fprint(conn, "250 2.0.0 Ok: queued\r\n")
		case "QUIT":
			_, _ = fmt.Fprint(conn, "221 2.0.0 Bye\r\n")
			return
		default:
			s.t.Errorf("fake SMTP relay: unexpected command %q", cmd)
			_, _ = fmt.Fprint(conn, "500 5.5.1 Command unrecognized\r\n")
		}
	}
}

// writeCapabilities answers EHLO the way a real relay does: a first line
// carrying the server name, then one line per capability, the last one
// single-spaced so the client knows the reply is complete. The first line
// must carry a dash continuation: net/smtp only parses capabilities out of
// a multiline reply, and discards the reply's first line along with any
// single-line reply, so a "250 STARTTLS" one-liner would never be seen.
func (s *FakeSMTPServer) writeCapabilities(w io.Writer) {
	caps := append([]string{"fake ESMTP"}, s.opts.Caps...)
	if s.opts.Cert != nil {
		caps = append(caps, "STARTTLS")
	}
	for i, capability := range caps {
		prefix := "250-"
		if i == len(caps)-1 {
			prefix = "250 "
		}
		_, _ = fmt.Fprintf(w, "%s%s\r\n", prefix, capability)
	}
}

// NewSelfSignedCert returns a certificate the fake relay can present. The
// mailer under test dials with InsecureSkipVerify, so the certificate only
// has to make the server-side handshake succeed.
func NewSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create relay certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
