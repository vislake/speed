package testutil

// Tests for the scripted SMTP relay itself, driven from inside this package
// so the relay's own protocol behaviour is exercised where its statements
// count. The driving client is the standard library's net/smtp -- a genuine,
// independent SMTP client implementation -- so these tests are real
// client/server conversations: the same relay pkgcore's own mailer tests
// script against, here verified to behave like the relay those tests assume.

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

// dial opens a raw TCP connection to the relay and wraps it in an smtp
// client speaking for the given local name.
func smtpClientFor(t *testing.T, srv *FakeSMTPServer, tlsConfig *tls.Config) *smtp.Client {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial(%q) error = %v, want nil", srv.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if tlsConfig != nil {
		conn = tls.Client(conn, tlsConfig)
	}
	client, err := smtp.NewClient(conn, "localhost")
	if err != nil {
		t.Fatalf("smtp.NewClient() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// sendOneMail runs one MAIL..DATA transaction through client against srv
// and returns the recorded exchange.
func sendOneMail(t *testing.T, srv *FakeSMTPServer, client *smtp.Client) SMTPExchange {
	t.Helper()
	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("client.Mail() error = %v, want nil", err)
	}
	if err := client.Rcpt("recipient@example.com"); err != nil {
		t.Fatalf("client.Rcpt() error = %v, want nil", err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatalf("client.Data() error = %v, want nil", err)
	}
	if _, err := w.Write([]byte("subject: test\r\n\r\nhello relay")); err != nil {
		t.Fatalf("data write error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("data close error = %v, want nil", err)
	}
	exchanges := srv.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	return exchanges[0]
}

// TestFakeSMTPServer_PlainSession_RecordsTheWholeExchange pins the plain
// (no auth, no TLS) transaction shape: one MAIL line, one RCPT line, the
// dot-unstuffed body, no auth, and no TLS.
func TestFakeSMTPServer_PlainSession_RecordsTheWholeExchange(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{})
	client := smtpClientFor(t, srv, nil)

	ex := sendOneMail(t, srv, client)
	if ex.From != "MAIL FROM:<sender@example.com>" {
		t.Errorf("exchange From = %q, want the whole MAIL line", ex.From)
	}
	if len(ex.Rcpts) != 1 || ex.Rcpts[0] != "RCPT TO:<recipient@example.com>" {
		t.Errorf("exchange Rcpts = %v, want the whole RCPT line", ex.Rcpts)
	}
	if ex.Msg != "subject: test\r\n\r\nhello relay" {
		t.Errorf("exchange Msg = %q, want the body verbatim", ex.Msg)
	}
	if ex.Auth != "" {
		t.Errorf("exchange Auth = %q, want none on a plain session", ex.Auth)
	}
	if ex.ConnTLS {
		t.Error("exchange ConnTLS = true, want false on a plain session")
	}
	if err := client.Quit(); err != nil {
		t.Fatalf("client.Quit() error = %v, want nil", err)
	}
}

// TestFakeSMTPServer_AuthExchange_RecordsTheAuthLine pins the AUTH leg: a
// relay advertising AUTH PLAIN sees the client's AUTH PLAIN exchange and
// records the whole line on the exchange.
func TestFakeSMTPServer_AuthExchange_RecordsTheAuthLine(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{Caps: []string{"AUTH PLAIN"}})
	client := smtpClientFor(t, srv, nil)

	auth := smtp.PlainAuth("", "user", "secret", "localhost")
	if err := client.Auth(auth); err != nil {
		t.Fatalf("client.Auth() error = %v, want nil", err)
	}
	ex := sendOneMail(t, srv, client)
	if !strings.HasPrefix(ex.Auth, "AUTH PLAIN ") {
		t.Errorf("exchange Auth = %q, want a recorded AUTH PLAIN line", ex.Auth)
	}
}

// TestFakeSMTPServer_StartTLS_MarksTheExchangeAndCarriesTheMail pins the
// STARTTLS leg: a relay advertising STARTTLS upgrades the connection when
// asked, and the mail that follows is recorded with ConnTLS true.
func TestFakeSMTPServer_StartTLS_MarksTheExchangeAndCarriesTheMail(t *testing.T) {
	cert := NewSelfSignedCert(t)
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{Caps: []string{"AUTH PLAIN"}, Cert: &cert})
	client := smtpClientFor(t, srv, nil)

	if err := client.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatalf("client.StartTLS() error = %v, want nil", err)
	}
	// STARTTLS keeps the same TCP connection: net/smtp re-runs EHLO over the
	// upgraded channel, and the relay's session loop must survive both.
	ex := sendOneMail(t, srv, client)
	if !ex.ConnTLS {
		t.Error("exchange ConnTLS = false, want true: the mail ran over TLS")
	}
	if ex.Msg != "subject: test\r\n\r\nhello relay" {
		t.Errorf("exchange Msg = %q, want the body verbatim over TLS", ex.Msg)
	}
}

// TestFakeSMTPServer_ImplicitTLS_AnswersFromTheFirstByte pins the implicit
// mode: the listener speaks TLS from the first byte, and a TLS-dialing
// client gets a normal session.
func TestFakeSMTPServer_ImplicitTLS_AnswersFromTheFirstByte(t *testing.T) {
	cert := NewSelfSignedCert(t)
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{Implicit: true, Cert: &cert})
	client := smtpClientFor(t, srv, &tls.Config{InsecureSkipVerify: true})

	ex := sendOneMail(t, srv, client)
	if !ex.ConnTLS {
		t.Error("exchange ConnTLS = false, want true on an implicit-TLS relay")
	}
}

// TestFakeSMTPServer_Reject_Answers550AndStillRecordsTheAttempt pins the
// rejection script: a RCPT the Reject function reports is answered 550
// (which surfaces to the SMTP client as an error), while the attempt is
// still recorded on the exchange, From included.
func TestFakeSMTPServer_Reject_Answers550AndStillRecordsTheAttempt(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{
		Reject: func(rcpt string) bool { return strings.Contains(rcpt, "blocked@example.com") },
	})
	client := smtpClientFor(t, srv, nil)

	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("client.Mail() error = %v, want nil", err)
	}
	if err := client.Rcpt("blocked@example.com"); err == nil {
		t.Fatal("client.Rcpt(blocked) error = nil, want the 550 refusal")
	}
	// The session continues after a refused recipient: an accepted one and a
	// data transaction still work.
	if err := client.Rcpt("allowed@example.com"); err != nil {
		t.Fatalf("client.Rcpt(allowed) error = %v, want nil", err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatalf("client.Data() error = %v, want nil", err)
	}
	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatalf("data write error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("data close error = %v, want nil", err)
	}

	exchanges := srv.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	ex := exchanges[0]
	if len(ex.Rcpts) != 2 {
		t.Errorf("exchange Rcpts = %v, want both the refused and the accepted RCPT recorded", ex.Rcpts)
	}
	if ex.From != "MAIL FROM:<sender@example.com>" {
		t.Errorf("exchange From = %q, want the whole MAIL line", ex.From)
	}
}

// TestFakeSMTPServer_DotUnstuffing_RoundTripsPins the relay's DATA handling:
// lines the client dot-stuffed (a body line beginning with ".") arrive at
// the record unstuffed, byte for byte.
func TestFakeSMTPServer_DotUnstuffing_RoundTrips(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{})
	client := smtpClientFor(t, srv, nil)

	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("client.Mail() error = %v, want nil", err)
	}
	if err := client.Rcpt("recipient@example.com"); err != nil {
		t.Fatalf("client.Rcpt() error = %v, want nil", err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatalf("client.Data() error = %v, want nil", err)
	}
	body := "first\r\n.starts-with-dot\r\n..starts-with-two\r\nlast"
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("data write error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("data close error = %v, want nil", err)
	}
	exchanges := srv.Take()
	if len(exchanges) != 1 {
		t.Fatalf("relay recorded %d exchanges, want 1", len(exchanges))
	}
	if exchanges[0].Msg != body {
		t.Errorf("exchange Msg = %q, want the body dot-unstuffed back to %q", exchanges[0].Msg, body)
	}
}

// TestFakeSMTPServer_Hang_NeverRepliesToCommands pins the hang script: the
// relay answers the greeting, then leaves a command unanswered -- what a
// dead relay looks like mid-transaction.
func TestFakeSMTPServer_Hang_NeverRepliesToCommands(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{Hang: true})

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial(%q) error = %v, want nil", srv.Addr(), err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline() error = %v, want nil", err)
	}
	br := bufio.NewReader(conn)
	greeting, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("greeting = %q (err %v), want a 220 line", greeting, err)
	}
	if _, err := conn.Write([]byte("EHLO localhost\r\n")); err != nil {
		t.Fatalf("EHLO write error = %v, want nil", err)
	}
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("relay answered an EHLO despite the hang script, want it to stay silent")
	} else {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("relay read error = %v, want a deadline timeout proving no reply came", err)
		}
	}
}

// TestFakeSMTPServer_TakeEmptiesTheRecord pins Take's emptying semantics,
// which the mailer tests rely on to assert per-mail records.
func TestFakeSMTPServer_TakeEmptiesTheRecord(t *testing.T) {
	srv := StartFakeSMTPServer(t, FakeSMTPOptions{})
	client := smtpClientFor(t, srv, nil)

	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("client.Mail() error = %v, want nil", err)
	}
	if err := client.Rcpt("recipient@example.com"); err != nil {
		t.Fatalf("client.Rcpt() error = %v, want nil", err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatalf("client.Data() error = %v, want nil", err)
	}
	if _, err := w.Write([]byte("one")); err != nil {
		t.Fatalf("data write error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("data close error = %v, want nil", err)
	}

	if got := srv.Take(); len(got) != 1 {
		t.Fatalf("first Take() = %d exchanges, want 1", len(got))
	}
	if got := srv.Take(); len(got) != 0 {
		t.Fatalf("second Take() = %d exchanges, want 0: Take empties the record", len(got))
	}
}
