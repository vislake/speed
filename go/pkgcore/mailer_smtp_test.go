package pkgcore

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// mailerFor builds an SMTP mailer pointed at the fake relay server. The mode
// knob exists because certificate validation is the TLS stack's business,
// not the mailer's: the tests here script the protocol, so they all skip
// it. It stays local to this file, rather than living alongside
// testutil.FakeSMTPServer, because testutil must not import this package
// (package pkgcore) — doing so from an unexported-symbol-testing file like
// this one, which is package pkgcore itself, would be an import cycle
// (pkgcore's own test binary -> testutil -> pkgcore). See
// internal/testutil/fake_smtp_server.go's own doc comment for the fuller
// version of this note, and unittest/mailer_conformance_test.go (package
// unittest, an external test package with no such restriction) for the
// parallel helper that driver needs for the same reason.
func mailerFor(t *testing.T, server *testutil.FakeSMTPServer, mode SMTPTLSMode, username, password string) Mailer {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split relay address %q: %v", server.Addr(), err)
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

// mailerWithReplyTo is mailerFor's ReplyTo-flavored twin: the same fake-relay
// wiring, plus the implementation-level Reply-To default the config carries.
func mailerWithReplyTo(t *testing.T, server *testutil.FakeSMTPServer, replyTo string) Mailer {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split relay address %q: %v", server.Addr(), err)
	}
	var portNumber int
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil {
		t.Fatalf("parse relay port %q: %v", port, err)
	}
	return NewSMTPMailer(SMTPConfig{
		Host:               host,
		Port:               portNumber,
		InsecureSkipVerify: true,
		ReplyTo:            replyTo,
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

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
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

	exchanges := server.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	ex := exchanges[0]
	if ex.From != "MAIL FROM:<ops@example.com>" {
		t.Errorf("MAIL line = %q, want MAIL FROM:<ops@example.com>", ex.From)
	}
	wantRcpts := []string{"RCPT TO:<ada@example.com>", "RCPT TO:<grace@example.com>"}
	if fmt.Sprint(ex.Rcpts) != fmt.Sprint(wantRcpts) {
		t.Errorf("RCPT lines = %v, want %v", ex.Rcpts, wantRcpts)
	}
	if ex.Auth != "" {
		t.Errorf("AUTH line = %q, want none without credentials", ex.Auth)
	}
	if ex.ConnTLS {
		t.Error("DATA ran over TLS, want a plaintext relay to stay plaintext")
	}

	headerBlock, body, found := strings.Cut(ex.Msg, "\r\n\r\n")
	if !found {
		t.Fatalf("relay message has no header/body separator: %q", ex.Msg)
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

	cert := testutil.NewSelfSignedCert(t)
	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{Cert: &cert})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "over TLS", Text: "upgraded",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if !exchanges[0].ConnTLS {
		t.Error("DATA did not run over TLS, want the STARTTLS upgrade to have happened")
	}
	wantPrefix := "From: ops@example.com\r\nTo: ada@example.com\r\nSubject: over TLS\r\nDate: "
	if !strings.HasPrefix(exchanges[0].Msg, wantPrefix) {
		t.Errorf("relay message does not start with the expected headers: %q", exchanges[0].Msg)
	}
	if !strings.HasSuffix(exchanges[0].Msg, "\r\n\r\nupgraded") {
		t.Errorf("relay message does not end with the expected body: %q", exchanges[0].Msg)
	}
}

// TestSMTPMailer_Send_SpeaksTLSFromTheFirstByte pins the implicit-TLS path,
// which SMTPTLSModeAuto also takes on port 465: the connection is TLS before
// the greeting, so the relay never sees a plaintext command.
func TestSMTPMailer_Send_SpeaksTLSFromTheFirstByte(t *testing.T) {
	t.Parallel()

	cert := testutil.NewSelfSignedCert(t)
	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{Implicit: true, Cert: &cert})
	mailer := mailerFor(t, server, SMTPTLSModeImplicitTLS, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "implicit", Text: "tls from the first byte",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.Take()
	if len(exchanges) != 1 || !exchanges[0].ConnTLS {
		t.Fatalf("relay recorded %d exchanges with connTLS=%t, want one over TLS", len(exchanges), exchanges[0].ConnTLS)
	}
}

// TestSMTPMailer_Send_AuthenticatesOverTLS pins the AUTH step: with
// credentials configured, the mailer authenticates after the TLS upgrade and
// the relay sees exactly the PLAIN credentials the configuration carried.
func TestSMTPMailer_Send_AuthenticatesOverTLS(t *testing.T) {
	t.Parallel()

	cert := testutil.NewSelfSignedCert(t)
	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{Cert: &cert, Caps: []string{"AUTH PLAIN"}})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "relay@example.com", "s3cret")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "authenticated", Text: "with credentials",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00relay@example.com\x00s3cret"))
	exchanges := server.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if exchanges[0].Auth != wantAuth {
		t.Errorf("AUTH line = %q, want %q", exchanges[0].Auth, wantAuth)
	}
	if !exchanges[0].ConnTLS {
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
	// helloErr: the NewClient error above stays live for the authenticate
	// check below, which reassigns err.
	if helloErr := client.Hello("smtp.example.com"); helloErr != nil {
		t.Fatalf("client.Hello: %v", helloErr)
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
// path of an RCPT 550: the send fails, the failure carries
// ErrTransportPermanent -- the relay's 5xx reply is its verdict on the
// recipient address, the one SMTP failure a caller may treat as terminal
// for the destination -- and the relay never saw a DATA, so no exchange is
// recorded.
func TestSMTPMailer_Send_FailsWhenTheRelayRejectsARecipient(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{
		Reject: func(rcpt string) bool { return strings.Contains(rcpt, "grace@") },
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
	if !errors.Is(err, ErrTransportPermanent) {
		t.Errorf("Send() error = %v, want errors.Is(err, ErrTransportPermanent): a 550 reply is the relay's verdict on the recipient", err)
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("Send() error = %v, want the relay's reply to stay reachable as the cause", err)
	}
	if !strings.Contains(err.Error(), "send mail via smtp") {
		t.Errorf("Send() error = %v, want it to carry the relay address context", err)
	}
	if exchanges := server.Take(); len(exchanges) != 0 {
		t.Errorf("relay recorded %d exchanges, want none: the message must not be sent when a recipient is refused", len(exchanges))
	}
}

// TestSMTPMailer_Send_TemporaryRecipientRefusalIsNotMarkedPermanent pins the
// boundary of the permanent marking at the wire: a 4xx reply to RCPT is the
// relay asking for a retry, not the destination's verdict, so Send must
// return it unmarked. Marking it would have a caller retrying a mailbox
// that is merely full answered as a terminal refusal -- the
// over-classification the marking deliberately excludes.
func TestSMTPMailer_Send_TemporaryRecipientRefusalIsNotMarkedPermanent(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{
		Reject:      func(string) bool { return true },
		RejectReply: "452 4.2.2 Mailbox full",
	})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "retry later", Text: "the mailbox is full",
	})
	if err == nil {
		t.Fatal("Send() error = nil, want the relay's 452 to fail the send")
	}
	if errors.Is(err, ErrTransportPermanent) {
		t.Errorf("Send() error = %v, want a transient 4xx refusal NOT marked with ErrTransportPermanent", err)
	}
	if !strings.Contains(err.Error(), "452") {
		t.Errorf("Send() error = %v, want the relay's reply text", err)
	}
}

// TestSMTPMailer_Send_DeadlineBreaksAHungRelay pins the context handling on
// the wire: a deadline bounds the whole transaction, so a relay that stops
// answering cannot hang the caller forever.
func TestSMTPMailer_Send_DeadlineBreaksAHungRelay(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{Hang: true})
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

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{Hang: true})
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

// TestSMTPMailer_Send_ControlCharacterAddressesNeverReachTheWire pins the
// address rule on the wire: a message whose From, a To entry or the ReplyTo
// carries a control character fails with ErrInvalidMail before anything is
// dialed, so the relay records no exchange at all. net/smtp's own
// command-line guard rejects CR and LF only, so without the rule a value like
// the To entry below would travel verbatim into the RCPT command and the raw
// DATA headers.
func TestSMTPMailer_Send_ControlCharacterAddressesNeverReachTheWire(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	mail := Mail{
		From:    "ops@example.com\x0b",
		To:      []string{"ada@example.com\x7f"},
		ReplyTo: "ops@example.com\u0085",
		Subject: "control characters in the addresses",
		Text:    "body",
	}
	err := mailer.Send(context.Background(), mail)
	exchanges := server.Take()
	if !errors.Is(err, ErrInvalidMail) {
		t.Fatalf("Send() error = %v, want ErrInvalidMail before any dial; relay saw %#v", err, exchanges)
	}
	if len(exchanges) != 0 {
		t.Errorf("relay recorded %d exchanges, want none: %#v", len(exchanges), exchanges)
	}
}

// TestSMTPMailer_Send_CarriesADisplayNameAddressForm pins the accept side of
// the address rule on the wire: the name-and-address display form the Mail
// documentation leaves to the caller passes validation and travels through
// the transaction as given.
func TestSMTPMailer_Send_CarriesADisplayNameAddressForm(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
	mailer := mailerFor(t, server, SMTPTLSModeAuto, "", "")

	err := mailer.Send(context.Background(), Mail{
		From: "Ada Lovelace <ada@example.com>", To: []string{"Grace Hopper <grace@example.com>"},
		Subject: "display names", Text: "body",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	exchanges := server.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if want := "To: Grace Hopper <grace@example.com>\r\n"; !strings.Contains(exchanges[0].Msg, want) {
		t.Errorf("relay message = %q, want it to carry %q", exchanges[0].Msg, want)
	}
}

// TestSMTPMailer_Send_ReportsDialFailures pins the error a valid send gets
// when the relay is unreachable: a wrapped error naming the relay address,
// and deliberately NOT one carrying ErrTransportPermanent -- an unreachable
// relay says nothing about the destination, so the marking must stay off a
// connection failure.
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
	if errors.Is(err, ErrTransportPermanent) {
		t.Errorf("Send() error = %v, want a refused dial NOT marked with ErrTransportPermanent", err)
	}
}

// TestSMTPMailer_Send_CancelledContextFailsBeforeDialing pins that a
// cancelled context fails Send without attempting the transaction.
func TestSMTPMailer_Send_CancelledContextFailsBeforeDialing(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
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
	if exchanges := server.Take(); len(exchanges) != 0 {
		t.Errorf("relay recorded %d exchanges, want none after cancellation", len(exchanges))
	}
}

// TestBuildMessage_ReplyToHeaderShape asserts where the optional Reply-To
// header lands: one extra line, after To and before Subject, with the bodies
// and every other header the six-line shape TestBuildMessage_PinsTheHeaderShape
// pins left in place.
func TestBuildMessage_ReplyToHeaderShape(t *testing.T) {
	t.Parallel()

	msg := buildMessage(Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com", "grace@example.com"},
		ReplyTo: "support@example.com",
		Subject: "plain ASCII subject",
		Text:    "line one\nline two",
	})

	headerBlock, body, found := strings.Cut(string(msg), "\r\n\r\n")
	if !found {
		t.Fatalf("message has no header/body separator: %q", msg)
	}
	headers := strings.Split(headerBlock, "\r\n")
	if len(headers) != 7 {
		t.Fatalf("headers = %v, want seven lines", headers)
	}
	want := []string{
		"From: ops@example.com",
		"To: ada@example.com, grace@example.com",
		"Reply-To: support@example.com",
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
	if _, err := time.Parse(time.RFC1123Z, strings.TrimPrefix(headers[4], "Date: ")); err != nil {
		t.Errorf("Date header %q does not parse as RFC1123Z: %v", headers[4], err)
	}
	if body != "line one\r\nline two" {
		t.Errorf("body = %q, want the CRLF-normalized text body", body)
	}
}

// TestSMTPMailer_Send_ResolvesReplyTo pins the two-layer Reply-To rule on the
// wire: the Mail's own value wins over SMTPConfig.ReplyTo, an empty one falls
// back to the config, and neither set writes no Reply-To header at all.
func TestSMTPMailer_Send_ResolvesReplyTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		configReply string
		mailReplyTo string
		wantHeader  string // "" means the header must be absent
	}{
		{"the mail's own value wins", "config@example.com", "team@example.com", "Reply-To: team@example.com\r\n"},
		{"an empty mail value falls back to the config", "config@example.com", "", "Reply-To: config@example.com\r\n"},
		{"neither set writes no header", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
			mailer := mailerWithReplyTo(t, server, tt.configReply)

			mail := Mail{
				From: "ops@example.com", To: []string{"ada@example.com"},
				Subject: "reply-to resolution", Text: "body",
			}
			mail.ReplyTo = tt.mailReplyTo
			if err := mailer.Send(context.Background(), mail); err != nil {
				t.Fatalf("Send() error = %v, want nil", err)
			}

			exchanges := server.Take()
			if len(exchanges) != 1 {
				t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
			}
			headerBlock, _, found := strings.Cut(exchanges[0].Msg, "\r\n\r\n")
			if !found {
				t.Fatalf("relay message has no header/body separator: %q", exchanges[0].Msg)
			}
			if tt.wantHeader == "" {
				if strings.Contains(headerBlock, "Reply-To:") {
					t.Errorf("header block carries a Reply-To, want none: %q", headerBlock)
				}
				return
			}
			if !strings.Contains(headerBlock, tt.wantHeader) {
				t.Errorf("header block = %q, want it to carry %q", headerBlock, tt.wantHeader)
			}
		})
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
		{"a reply-to carrying a line break", SMTPConfig{Host: "smtp.example.com", Port: 25, ReplyTo: "ops@example.com\r\nBcc: sneaky@example.com"}, "line break in SMTPConfig.ReplyTo"},
		{"a reply-to carrying a control character", SMTPConfig{Host: "smtp.example.com", Port: 25, ReplyTo: "ops@example.com\x0b"}, "control character in SMTPConfig.ReplyTo"},
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

// TestNewSMTPMailerFromFields_RejectsReplyToWithControlCharacters pins the
// component configuration channel's guard: a reply_to carrying a line break
// or any other control character comes back as an error, never as a mailer
// whose Sends would carry that value into the relay's DATA headers.
func TestNewSMTPMailerFromFields_RejectsReplyToWithControlCharacters(t *testing.T) {
	t.Parallel()

	t.Run("a plain reply-to passes", func(t *testing.T) {
		t.Parallel()

		mailer, err := newSMTPMailerFromFields("smtp.example.com", 587, "", "", "ops@example.com", SMTPTLSModeAuto, false)
		if err != nil {
			t.Fatalf("newSMTPMailerFromFields() error = %v, want nil", err)
		}
		if mailer == nil {
			t.Fatal("newSMTPMailerFromFields() mailer = nil, want a mailer")
		}
	})

	for _, tt := range []struct{ name, replyTo string }{
		{"a_line_break", "ops@example.com\r\nBcc: sneaky@example.com"},
		{"a_control_character", "ops@example.com\x0b"},
	} {
		t.Run("rejects_"+tt.name, func(t *testing.T) {
			t.Parallel()

			mailer, err := newSMTPMailerFromFields("smtp.example.com", 587, "", "", tt.replyTo, SMTPTLSModeAuto, false)
			if err == nil {
				t.Fatalf("newSMTPMailerFromFields(reply_to=%q) returned a mailer and no error, want a refusal", tt.replyTo)
			}
			if mailer != nil {
				t.Errorf("newSMTPMailerFromFields returned a mailer alongside error %v, want none", err)
			}
			if !strings.Contains(err.Error(), "reply_to") {
				t.Errorf("error = %v, want it to name the reply_to field", err)
			}
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
