package pkgcore

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- in-process fake SMTP relay -------------------------------------------

// fakeSMTPOptions shapes the scripted relay a test talks to.
type fakeSMTPOptions struct {
	// caps are the EHLO capabilities advertised in order, one per reply line,
	// e.g. "AUTH PLAIN". STARTTLS is appended automatically when cert is set.
	caps []string

	// cert, when set, makes the relay advertise STARTTLS and upgrade with this
	// certificate when the client asks.
	cert *tls.Certificate

	// implicit wraps the listener in TLS from the first byte, for relays that
	// speak TLS-only SMTP.
	implicit bool

	// hang makes the relay answer the greeting but never reply to any command,
	// which is what a dead relay looks like mid-transaction.
	hang bool

	// reject, when set, answers 550 to every RCPT it reports true for.
	reject func(rcpt string) bool
}

// smtpExchange is everything the relay saw in one MAIL..DATA transaction.
type smtpExchange struct {
	auth    string   // the whole "AUTH PLAIN ..." line, empty when none came
	from    string   // the whole "MAIL FROM:<...>" line
	rcpts   []string // whole "RCPT TO:<...>" lines
	msg     string   // dot-unstuffed message body as received
	connTLS bool     // whether the connection was TLS by the time DATA ran
}

// fakeSMTPServer is a scripted SMTP relay for exercising the mailer without a
// real one. It answers each command with a fixed success reply, records every
// exchange, and plays the failure scripts its options describe.
type fakeSMTPServer struct {
	t    *testing.T
	ln   net.Listener
	opts fakeSMTPOptions

	mu        sync.Mutex
	exchanges []smtpExchange
}

// startFakeSMTPServer listens on an ephemeral localhost port and cleans up at
// the end of the test.
func startFakeSMTPServer(t *testing.T, opts fakeSMTPOptions) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake SMTP relay: listen: %v", err)
	}
	if opts.implicit {
		if opts.cert == nil {
			ln.Close()
			t.Fatal("fake SMTP relay: implicit TLS requires a certificate")
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*opts.cert}})
	}
	s := &fakeSMTPServer{t: t, ln: ln, opts: opts}
	go s.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return s
}

// addr returns the relay's 127.0.0.1:port address.
func (s *fakeSMTPServer) addr() string { return s.ln.Addr().String() }

// take returns the recorded exchanges, emptying the record.
func (s *fakeSMTPServer) take() []smtpExchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	exchanges := s.exchanges
	s.exchanges = nil
	return exchanges
}

func (s *fakeSMTPServer) record(ex smtpExchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = append(s.exchanges, ex)
}

func (s *fakeSMTPServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// handle runs one SMTP session: greet, then answer commands until the client
// quits or the connection dies. The greeting is the relay's first word, and on
// an implicit-TLS listener it also triggers the lazy server-side handshake.
func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	fmt.Fprint(conn, "220 fake ESMTP ready\r\n")

	br := bufio.NewReader(conn)
	connTLS := s.opts.implicit
	ex := smtpExchange{}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(cmd, " ", 2)
		switch strings.ToUpper(parts[0]) {
		case "EHLO", "HELO":
			if s.opts.hang {
				continue // leave the client waiting forever
			}
			s.writeCapabilities(conn)
		case "STARTTLS":
			fmt.Fprint(conn, "220 2.0.0 Ready to start TLS\r\n")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*s.opts.cert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			br = bufio.NewReader(conn)
			connTLS = true
		case "AUTH":
			ex.auth = cmd
			fmt.Fprint(conn, "235 2.7.0 Authentication successful\r\n")
		case "MAIL":
			ex.from = cmd
			fmt.Fprint(conn, "250 2.1.0 Ok\r\n")
		case "RCPT":
			ex.rcpts = append(ex.rcpts, cmd)
			if s.opts.reject != nil && s.opts.reject(cmd) {
				fmt.Fprint(conn, "550 5.1.1 No such user\r\n")
				continue
			}
			fmt.Fprint(conn, "250 2.1.5 Ok\r\n")
		case "DATA":
			fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
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
			ex.msg = strings.Join(body, "\r\n")
			ex.connTLS = connTLS
			s.record(ex)
			ex = smtpExchange{}
			fmt.Fprint(conn, "250 2.0.0 Ok: queued\r\n")
		case "QUIT":
			fmt.Fprint(conn, "221 2.0.0 Bye\r\n")
			return
		default:
			s.t.Errorf("fake SMTP relay: unexpected command %q", cmd)
			fmt.Fprint(conn, "500 5.5.1 Command unrecognized\r\n")
		}
	}
}

// writeCapabilities answers EHLO the way a real relay does: a first line
// carrying the server name, then one line per capability, the last one
// single-spaced so the client knows the reply is complete. The first line
// must carry a dash continuation: net/smtp only parses capabilities out of a
// multiline reply, and discards the reply's first line along with any
// single-line reply, so a "250 STARTTLS" one-liner would never be seen.
func (s *fakeSMTPServer) writeCapabilities(w io.Writer) {
	caps := append([]string{"fake ESMTP"}, s.opts.caps...)
	if s.opts.cert != nil {
		caps = append(caps, "STARTTLS")
	}
	for i, capability := range caps {
		prefix := "250-"
		if i == len(caps)-1 {
			prefix = "250 "
		}
		fmt.Fprintf(w, "%s%s\r\n", prefix, capability)
	}
}

// newSelfSignedCert returns a certificate the fake relay can present. The
// mailer under test dials with InsecureSkipVerify, so the certificate only
// has to make the server-side handshake succeed.
func newSelfSignedCert(t *testing.T) tls.Certificate {
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

// mailerFor builds an SMTP mailer pointed at the fake relay. The tlsConfig
// knob exists because certificate validation is the TLS stack's business, not
// the mailer's: the tests here script the protocol, so they all skip it.
func mailerFor(t *testing.T, s *fakeSMTPServer, mode SMTPTLSMode, username, password string) Mailer {
	t.Helper()
	host, port, err := net.SplitHostPort(s.addr())
	if err != nil {
		t.Fatalf("split relay address %q: %v", s.addr(), err)
	}
	var portNumber int
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil {
		t.Fatalf("parse relay port %q: %v", port, err)
	}
	return NewSMTPMailer(SMTPConfig{
		Host:               host,
		Port:               portNumber,
		Username:           username,
		Password:           password,
		TLSMode:            mode,
		InsecureSkipVerify: true,
	})
}

// ---- SMTP wire tests -------------------------------------------------------

// TestSMTPMailer_Send_DeliversOverAPlaintextRelay runs one complete
// transaction against a relay with no TLS at all: EHLO, MAIL, two RCPTs, DATA
// with a multipart message, QUIT. Everything the relay saw is asserted,
// message bytes included, so a regression in any step of the conversation
// fails this test.
func TestSMTPMailer_Send_DeliversOverAPlaintextRelay(t *testing.T) {
	t.Parallel()

	server := startFakeSMTPServer(t, fakeSMTPOptions{})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	mail := Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com", "grace@example.com"},
		Subject: "Your invoice #1042 is ready",
		Text:    "Hello Ada,\n\nyour invoice is ready.",
		HTML:    "<p>Hello Ada,</p>",
	}
	if err := mailer.Send(context.Background(), mail); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	ex := exchanges[0]
	if ex.from != "MAIL FROM:<ops@example.com>" {
		t.Errorf("MAIL line = %q, want MAIL FROM:<ops@example.com>", ex.from)
	}
	wantRcpts := []string{"RCPT TO:<ada@example.com>", "RCPT TO:<grace@example.com>"}
	if fmt.Sprint(ex.rcpts) != fmt.Sprint(wantRcpts) {
		t.Errorf("RCPT lines = %v, want %v", ex.rcpts, wantRcpts)
	}
	if ex.auth != "" {
		t.Errorf("AUTH line = %q, want none without credentials", ex.auth)
	}
	if ex.connTLS {
		t.Error("DATA ran over TLS, want a plaintext relay to stay plaintext")
	}

	headerBlock, body, found := strings.Cut(ex.msg, "\r\n\r\n")
	if !found {
		t.Fatalf("relay message has no header/body separator: %q", ex.msg)
	}
	headers := strings.Split(headerBlock, "\r\n")
	expectHeader := func(index int, want string) {
		if len(headers) <= index {
			t.Fatalf("headers = %v, want a header at index %d", headers, index)
		}
		if headers[index] != want {
			t.Errorf("header[%d] = %q, want %q", index, headers[index], want)
		}
	}
	expectHeader(0, "From: ops@example.com")
	expectHeader(1, "To: ada@example.com, grace@example.com")
	expectHeader(2, "Subject: Your invoice #1042 is ready")
	if _, err := time.Parse(time.RFC1123Z, strings.TrimPrefix(headers[3], "Date: ")); err != nil {
		t.Errorf("Date header %q does not parse as RFC1123Z: %v", headers[3], err)
	}
	expectHeader(4, "MIME-Version: 1.0")

	contentType := strings.TrimPrefix(headers[5], "Content-Type: ")
	mediatype, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", contentType, err)
	}
	if mediatype != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediatype)
	}

	reader := multipart.NewReader(strings.NewReader(body), params["boundary"])
	assertPart := func(wantType, wantBody string) {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		if got := part.Header.Get("Content-Type"); got != wantType {
			t.Errorf("part Content-Type = %q, want %q", got, wantType)
		}
		content, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read multipart body: %v", err)
		}
		if got := string(content); got != wantBody {
			t.Errorf("part body = %q, want %q", got, wantBody)
		}
	}
	assertPart("text/plain; charset=utf-8", "Hello Ada,\r\n\r\nyour invoice is ready.")
	assertPart("text/html; charset=utf-8", "<p>Hello Ada,</p>")
	if _, err := reader.NextPart(); err != io.EOF {
		t.Errorf("multipart body carries a third part, want exactly two: %v", err)
	}
}

// TestSMTPMailer_Send_UpgradesWithSTARTTLS pins the SMTPTLSModeAuto path on a
// non-465 port: the relay advertises STARTTLS, the mailer upgrades, and the
// message goes out over the TLS connection.
func TestSMTPMailer_Send_UpgradesWithSTARTTLS(t *testing.T) {
	t.Parallel()

	cert := newSelfSignedCert(t)
	server := startFakeSMTPServer(t, fakeSMTPOptions{cert: &cert})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "over TLS", Text: "upgraded",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if !exchanges[0].connTLS {
		t.Error("DATA did not run over TLS, want the STARTTLS upgrade to have happened")
	}
	wantPrefix := "From: ops@example.com\r\nTo: ada@example.com\r\nSubject: over TLS\r\nDate: "
	if !strings.HasPrefix(exchanges[0].msg, wantPrefix) {
		t.Errorf("relay message does not start with the expected headers: %q", exchanges[0].msg)
	}
	if !strings.HasSuffix(exchanges[0].msg, "\r\n\r\nupgraded") {
		t.Errorf("relay message does not end with the expected body: %q", exchanges[0].msg)
	}
}

// TestSMTPMailer_Send_SpeaksTLSFromTheFirstByte pins the implicit-TLS path,
// which SMTPTLSModeAuto also takes on port 465: the connection is TLS before
// the greeting, so the relay never sees a plaintext command.
func TestSMTPMailer_Send_SpeaksTLSFromTheFirstByte(t *testing.T) {
	t.Parallel()

	cert := newSelfSignedCert(t)
	server := startFakeSMTPServer(t, fakeSMTPOptions{implicit: true, cert: &cert})
	mailer := mailerFor(t, server, SMTPTLSModeImplicitTLS, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "implicit", Text: "tls from the first byte",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.take()
	if len(exchanges) != 1 || !exchanges[0].connTLS {
		t.Fatalf("relay recorded %d exchanges with connTLS=%t, want one over TLS", len(exchanges), exchanges[0].connTLS)
	}
}

// TestSMTPMailer_Send_AuthenticatesOverTLS pins the AUTH step: with
// credentials configured, the mailer authenticates after the TLS upgrade and
// the relay sees exactly the PLAIN credentials the configuration carried.
func TestSMTPMailer_Send_AuthenticatesOverTLS(t *testing.T) {
	t.Parallel()

	cert := newSelfSignedCert(t)
	server := startFakeSMTPServer(t, fakeSMTPOptions{cert: &cert, caps: []string{"AUTH PLAIN"}})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "relay@example.com", "s3cret")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "authenticated", Text: "with credentials",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00relay@example.com\x00s3cret"))
	exchanges := server.take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if exchanges[0].auth != wantAuth {
		t.Errorf("AUTH line = %q, want %q", exchanges[0].auth, wantAuth)
	}
	if !exchanges[0].connTLS {
		t.Error("DATA did not run over TLS, want AUTH to have happened on the TLS connection")
	}
}

// TestSMTPMailer_RefusesAuthOverAPlaintextConnection pins the guarantee that
// credentials are never sent in the clear: PlainAuth itself refuses a
// non-localhost relay whose connection never became TLS. The relay hostname
// here is deliberately not localhost, because net/smtp trusts localhost the
// way it trusts TLS.
func TestSMTPMailer_RefusesAuthOverAPlaintextConnection(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	go func() {
		br := bufio.NewReader(serverConn)
		fmt.Fprint(serverConn, "220 smtp.example.com ESMTP\r\n")
		if _, err := br.ReadString('\n'); err != nil { // EHLO
			return
		}
		fmt.Fprint(serverConn, "250-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		// The client must close without sending AUTH; any further line read
		// then fails.
		if _, err := br.ReadString('\n'); err == nil {
			fmt.Fprint(serverConn, "250 2.0.0 Ok\r\n")
		}
	}()

	client, err := smtp.NewClient(clientConn, "smtp.example.com")
	if err != nil {
		t.Fatalf("smtp.NewClient: %v", err)
	}
	defer client.Close()
	if err := client.Hello("smtp.example.com"); err != nil {
		t.Fatalf("client.Hello: %v", err)
	}

	m := &smtpMailer{cfg: SMTPConfig{
		Host: "smtp.example.com", Port: 25,
		Username: "relay@example.com", Password: "s3cret",
	}}
	err = m.authenticate(client)
	if err == nil {
		t.Fatal("authenticate() error = nil, want the unencrypted-connection refusal")
	}
	if !strings.Contains(err.Error(), "unencrypted connection") {
		t.Errorf("authenticate() error = %v, want it to name the unencrypted connection", err)
	}
}

// TestSMTPMailer_Send_FailsWhenTheRelayRejectsARecipient pins the failure
// path of an RCPT 550: the send fails, and the relay never saw a DATA, so no
// exchange is recorded.
func TestSMTPMailer_Send_FailsWhenTheRelayRejectsARecipient(t *testing.T) {
	t.Parallel()

	server := startFakeSMTPServer(t, fakeSMTPOptions{
		reject: func(rcpt string) bool { return strings.Contains(rcpt, "grace@") },
	})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	err := mailer.Send(context.Background(), Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com", "grace@example.com"},
		Subject: "partial delivery", Text: "must not be accepted",
	})
	if err == nil {
		t.Fatal("Send() error = nil, want the relay's 550 to fail the send")
	}
	if !strings.Contains(err.Error(), "send mail via smtp") {
		t.Errorf("Send() error = %v, want it to carry the relay address context", err)
	}
	if exchanges := server.take(); len(exchanges) != 0 {
		t.Errorf("relay recorded %d exchanges, want none: the message must not be sent when a recipient is refused", len(exchanges))
	}
}

// TestSMTPMailer_Send_DeadlineBreaksAHungRelay pins the context handling on
// the wire: a deadline bounds the whole transaction, so a relay that stops
// answering cannot hang the caller forever.
func TestSMTPMailer_Send_DeadlineBreaksAHungRelay(t *testing.T) {
	t.Parallel()

	server := startFakeSMTPServer(t, fakeSMTPOptions{hang: true})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- mailer.Send(ctx, Mail{
			From: "ops@example.com", To: []string{"ada@example.com"},
			Subject: "never delivered", Text: "the relay never answers",
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send() error = nil, want the deadline to fail the hung transaction")
		}
		if !strings.Contains(err.Error(), "send mail via smtp") {
			t.Errorf("Send() error = %v, want it to carry the relay address context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send() did not return within 5s of a 100ms deadline: the deadline did not reach the connection")
	}
}

// TestSMTPMailer_Send_CancellationBreaksAHungRelay pins the cancellation
// path a deadline cannot cover: the relay hangs, the Send's context is
// cancelled instead of carrying a deadline, and the watcher's close of the
// connection is what interrupts the hung transaction. Without the watcher,
// net/smtp's blocking reads would never notice the cancellation and the Send
// would hang on the relay forever.
func TestSMTPMailer_Send_CancellationBreaksAHungRelay(t *testing.T) {
	t.Parallel()

	server := startFakeSMTPServer(t, fakeSMTPOptions{hang: true})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- mailer.Send(ctx, Mail{
			From: "ops@example.com", To: []string{"ada@example.com"},
			Subject: "never delivered", Text: "the relay never answers",
		})
	}()

	// Let the Send reach the hung relay first, so the cancellation lands
	// mid-transaction where only the watcher can act on it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send() error = nil, want the cancellation to fail the hung transaction")
		}
		if !strings.Contains(err.Error(), "send mail via smtp") {
			t.Errorf("Send() error = %v, want it to carry the relay address context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send() did not return within 5s of a cancellation: the watcher did not close the connection")
	}
}

// TestSMTPMailer_Send_RejectsInvalidMailWithoutTouchingTheWire pins the rule
// that validation runs before any dial: the invalid message fails with
// ErrInvalidMail against an address nothing listens on, where a mailer that
// dialed first would have returned the refused connection instead.
func TestSMTPMailer_Send_RejectsInvalidMailWithoutTouchingTheWire(t *testing.T) {
	t.Parallel()

	// Reserve an address, then free it, so a dial would be refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	host, port, err := net.SplitHostPort(deadAddr)
	if err != nil {
		t.Fatalf("split %q: %v", deadAddr, err)
	}
	var portNumber int
	fmt.Sscanf(port, "%d", &portNumber)

	mailer := NewSMTPMailer(SMTPConfig{Host: host, Port: portNumber})
	err = mailer.Send(context.Background(), Mail{
		From: "ops@example.com", Subject: "no recipients", Text: "body",
	})
	if !errors.Is(err, ErrInvalidMail) {
		t.Fatalf("Send() error = %v, want ErrInvalidMail from validation before any dial", err)
	}
}

// TestSMTPMailer_Send_ReportsDialFailures pins the error a valid send gets
// when the relay is unreachable: a wrapped error naming the relay address.
func TestSMTPMailer_Send_ReportsDialFailures(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	host, port, err := net.SplitHostPort(deadAddr)
	if err != nil {
		t.Fatalf("split %q: %v", deadAddr, err)
	}
	var portNumber int
	fmt.Sscanf(port, "%d", &portNumber)

	mailer := NewSMTPMailer(SMTPConfig{Host: host, Port: portNumber})
	err = mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "never delivered", Text: "no relay here",
	})
	if err == nil {
		t.Fatal("Send() error = nil, want the refused dial to fail the send")
	}
	if !strings.Contains(err.Error(), deadAddr) || !strings.Contains(err.Error(), "send mail via smtp") {
		t.Errorf("Send() error = %v, want it to wrap the relay address %q", err, deadAddr)
	}
}

// TestSMTPMailer_Send_CancelledContextFailsBeforeDialing pins that a
// cancelled context fails Send without attempting the transaction.
func TestSMTPMailer_Send_CancelledContextFailsBeforeDialing(t *testing.T) {
	t.Parallel()

	server := startFakeSMTPServer(t, fakeSMTPOptions{})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mailer.Send(ctx, Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "never sent", Text: "cancelled",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want context.Canceled", err)
	}
	if exchanges := server.take(); len(exchanges) != 0 {
		t.Errorf("relay recorded %d exchanges, want none after cancellation", len(exchanges))
	}
}

// ---- constructor and TLS-mode selection ------------------------------------

// TestNewSMTPMailer_PanicsOnAnUnusableConfig pins which configurations are
// wiring errors: they fail at construction, before any Send could run.
func TestNewSMTPMailer_PanicsOnAnUnusableConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  SMTPConfig
		want string
	}{
		{"an empty host", SMTPConfig{Port: 25}, "non-empty SMTPConfig.Host"},
		{"a zero port", SMTPConfig{Host: "smtp.example.com"}, "Port in 1..65535"},
		{"a negative port", SMTPConfig{Host: "smtp.example.com", Port: -1}, "Port in 1..65535"},
		{"an out-of-range port", SMTPConfig{Host: "smtp.example.com", Port: 65536}, "Port in 1..65535"},
		{"an unknown TLS mode", SMTPConfig{Host: "smtp.example.com", Port: 25, TLSMode: SMTPTLSMode(99)}, "unknown SMTPTLSMode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("NewSMTPMailer(%+v) did not panic, want it to", tt.cfg)
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, tt.want) {
					t.Errorf("panic = %v, want a message containing %q", r, tt.want)
				}
			}()
			NewSMTPMailer(tt.cfg)
		})
	}
}

// TestSMTPMailer_TLSFromFirstByte pins the port-and-mode convention that
// decides whether the connection is TLS before the greeting.
func TestSMTPMailer_TLSFromFirstByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  SMTPConfig
		want bool
	}{
		{"auto on 465", SMTPConfig{Host: "h", Port: 465, TLSMode: SMTPTLSModeAuto}, true},
		{"auto on 587", SMTPConfig{Host: "h", Port: 587, TLSMode: SMTPTLSModeAuto}, false},
		{"starttls on 465", SMTPConfig{Host: "h", Port: 465, TLSMode: SMTPTLSModeStartTLS}, false},
		{"implicit on any port", SMTPConfig{Host: "h", Port: 2525, TLSMode: SMTPTLSModeImplicitTLS}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &smtpMailer{cfg: tt.cfg}
			if got := m.tlsFromFirstByte(); got != tt.want {
				t.Errorf("tlsFromFirstByte() = %t, want %t", got, tt.want)
			}
		})
	}
}

// ---- message rendering -----------------------------------------------------

// TestBuildMessage_PinsTheHeaderShape asserts the exact header block a
// message carries on the wire, one line per header in a fixed order, all with
// CRLF endings and a blank line before the body.
func TestBuildMessage_PinsTheHeaderShape(t *testing.T) {
	t.Parallel()

	msg := buildMessage(Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com", "grace@example.com"},
		Subject: "plain ASCII subject",
		Text:    "line one\nline two",
	})

	headerBlock, body, found := strings.Cut(string(msg), "\r\n\r\n")
	if !found {
		t.Fatalf("message has no header/body separator: %q", msg)
	}
	headers := strings.Split(headerBlock, "\r\n")
	if len(headers) != 6 {
		t.Fatalf("headers = %v, want six lines", headers)
	}
	want := []string{
		"From: ops@example.com",
		"To: ada@example.com, grace@example.com",
		"Subject: plain ASCII subject",
		"", // Date, filled in below
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	for i, line := range want {
		if line == "" {
			continue
		}
		if headers[i] != line {
			t.Errorf("header[%d] = %q, want %q", i, headers[i], line)
		}
	}
	if _, err := time.Parse(time.RFC1123Z, strings.TrimPrefix(headers[3], "Date: ")); err != nil {
		t.Errorf("Date header %q does not parse as RFC1123Z: %v", headers[3], err)
	}
	if strings.Contains(strings.ReplaceAll(headerBlock, "\r\n", ""), "\n") {
		t.Errorf("header block contains a lone LF: %q", headerBlock)
	}
	if body != "line one\r\nline two" {
		t.Errorf("body = %q, want the CRLF-normalized text body", body)
	}
}

// TestRenderBody_NormalizesLineEndingsToCRLF pins the body normalization: any
// line ending style in, CRLF out, and a body that already used CRLF survives
// unchanged.
func TestRenderBody_NormalizesLineEndingsToCRLF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{"lone LF", "a\nb\nc", "a\r\nb\r\nc"},
		{"existing CRLF", "a\r\nb\r\nc", "a\r\nb\r\nc"},
		{"mixed endings", "a\r\nb\nc\rd", "a\r\nb\r\nc\rd"}, // a stray \r is data, not a line ending
		{"no trailing newline", "single line", "single line"},
		{"trailing newline kept", "a\n", "a\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contentType, body := renderBody(Mail{From: "f", To: []string{"t"}, Text: tt.text})
			if contentType != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", contentType)
			}
			if body != tt.want {
				t.Errorf("body = %q, want %q", body, tt.want)
			}
		})
	}
}

// TestRenderBody_SingleHTMLBody checks the html-only rendering.
func TestRenderBody_SingleHTMLBody(t *testing.T) {
	t.Parallel()

	contentType, body := renderBody(Mail{From: "f", To: []string{"t"}, HTML: "<p>hi</p>\n<p>again</p>"})
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", contentType)
	}
	if body != "<p>hi</p>\r\n<p>again</p>" {
		t.Errorf("body = %q, want the CRLF-normalized HTML", body)
	}
}

// TestEncodeSubject_KeepsASCIIAndEncodesTheRest pins the RFC 2047 subject
// handling: an ASCII subject crosses unchanged, anything else comes back as
// an encoded word that decodes to the original.
func TestEncodeSubject_KeepsASCIIAndEncodesTheRest(t *testing.T) {
	t.Parallel()

	if got := encodeSubject("plain ASCII"); got != "plain ASCII" {
		t.Errorf("encodeSubject(ascii) = %q, want it untouched", got)
	}

	decoder := new(mime.WordDecoder)
	for _, subject := range []string{"Naïve café", "发票 #1042 已开具", "snowman ☃ and 中文"} {
		encoded := encodeSubject(subject)
		if encoded == subject {
			t.Errorf("encodeSubject(%q) left the non-ASCII subject untouched", subject)
		}
		if !strings.HasPrefix(encoded, "=?utf-8?") || !strings.HasSuffix(encoded, "?=") {
			t.Errorf("encodeSubject(%q) = %q, want an RFC 2047 encoded word", subject, encoded)
		}
		decoded, err := decoder.DecodeHeader(encoded)
		if err != nil {
			t.Errorf("decoding %q: %v", encoded, err)
			continue
		}
		if decoded != subject {
			t.Errorf("encodeSubject round-trip = %q, want %q", decoded, subject)
		}
	}
}

// TestRenderBody_BothBodies_MultipartRoundTrip renders a two-body message and
// parses the result back with the standard multipart reader, so the test
// asserts the document is well-formed, not just that its bytes match a
// golden string.
func TestRenderBody_BothBodies_MultipartRoundTrip(t *testing.T) {
	t.Parallel()

	mail := Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "both bodies",
		Text:    "plain rendering\nsecond line",
		HTML:    "<p>rich rendering</p>",
	}
	contentType, body := renderBody(mail)

	mediatype, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", contentType, err)
	}
	if mediatype != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediatype)
	}

	reader := multipart.NewReader(strings.NewReader(body), params["boundary"])
	assertPart := func(wantType, wantBody string) {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		if got := part.Header.Get("Content-Type"); got != wantType {
			t.Errorf("part Content-Type = %q, want %q", got, wantType)
		}
		content, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read multipart body: %v", err)
		}
		if got := string(content); got != wantBody {
			t.Errorf("part body = %q, want %q", got, wantBody)
		}
	}
	// The decreasingly capable rendering comes first, so a recipient that
	// understands HTML sees it and the others fall back to the plain part.
	assertPart("text/plain; charset=utf-8", "plain rendering\r\nsecond line")
	assertPart("text/html; charset=utf-8", "<p>rich rendering</p>")
	if _, err := reader.NextPart(); err != io.EOF {
		t.Errorf("multipart body carries a third part, want exactly two: %v", err)
	}
}
