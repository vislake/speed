package pkgcore

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SMTPTLSMode selects how an SMTP mailer reaches its relay's TLS. The zero
// value, SMTPTLSModeAuto, is the default and applies the convention every
// relay follows: port 465 is implicit TLS from the first byte, every other
// port starts plaintext and upgrades with STARTTLS when the relay advertises
// it.
type SMTPTLSMode int

const (
	// SMTPTLSModeAuto dials implicit TLS on port 465 and uses STARTTLS on
	// every other port when the relay advertises it.
	SMTPTLSModeAuto SMTPTLSMode = iota

	// SMTPTLSModeStartTLS starts plaintext and upgrades with STARTTLS when the
	// relay advertises it, regardless of port. A relay that does not advertise
	// STARTTLS is used in plaintext, like net/smtp.SendMail does: production
	// relays all offer it, and SMTP AUTH refuses to send credentials over a
	// connection that never became TLS, so secrets are still protected.
	SMTPTLSModeStartTLS

	// SMTPTLSModeImplicitTLS wraps the connection in TLS before the first
	// byte, regardless of port. It exists for relays that speak TLS-only SMTP
	// on a nonstandard port.
	SMTPTLSModeImplicitTLS
)

// SMTPConfig configures the distributed deployment mode's Mailer. Host and
// Port address the relay; everything else is optional.
type SMTPConfig struct {
	Host string
	Port int

	// Username and Password enable SMTP AUTH when Username is non-empty.
	// Credentials are only ever sent over TLS: PlainAuth refuses a plaintext
	// connection, so an auth-enabled mailer silently uses no credentials on a
	// relay that offers no STARTTLS rather than leaking them.
	Username string
	Password string

	// TLSMode selects how the mailer reaches the relay's TLS; the zero value
	// applies the SMTPTLSModeAuto convention.
	TLSMode SMTPTLSMode

	// InsecureSkipVerify accepts a relay certificate that does not validate
	// against the system roots, for relays on self-signed certificates. It
	// disables certificate verification entirely, so it must never be enabled
	// for a relay that is not fully trusted.
	InsecureSkipVerify bool
}

// smtpMailer is the distributed deployment mode's Mailer: an SMTP client
// speaking the protocol directly over net/smtp from the standard library.
// Every connection and command honours the context of the Send that started
// it.
type smtpMailer struct {
	cfg SMTPConfig
}

// NewSMTPMailer returns a Mailer that delivers through the SMTP relay in cfg.
// Nothing is dialed here: the relay is contacted on the first Send, so
// constructing a mailer never blocks and never fails on a relay that is down.
// An unusable configuration (empty host, out-of-range port, unknown TLSMode)
// panics instead, because it is an unrecoverable wiring error at startup.
func NewSMTPMailer(cfg SMTPConfig) Mailer {
	if cfg.Host == "" {
		panic("pkgcore: NewSMTPMailer requires a non-empty SMTPConfig.Host")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		panic(fmt.Sprintf("pkgcore: NewSMTPMailer requires SMTPConfig.Port in 1..65535, got %d", cfg.Port))
	}
	switch cfg.TLSMode {
	case SMTPTLSModeAuto, SMTPTLSModeStartTLS, SMTPTLSModeImplicitTLS:
	default:
		panic(fmt.Sprintf("pkgcore: NewSMTPMailer rejects unknown SMTPTLSMode %d", cfg.TLSMode))
	}
	return &smtpMailer{cfg: cfg}
}

// tlsConfig returns the client TLS configuration for the relay. ServerName is
// always the configured host, so a certificate issued for the relay validates
// whether the mailer dials it by hostname or by IP address.
func (m *smtpMailer) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         m.cfg.Host,
		InsecureSkipVerify: m.cfg.InsecureSkipVerify,
	}
}

// Send implements Mailer.Send by running one SMTP transaction against the
// relay: dial, hello, TLS upgrade per SMTPTLSMode, optional AUTH, then
// MAIL/RCPT/DATA with the rendered message. The relay answers each command
// before the next one is sent. Failures at any step abort the transaction and
// close the connection.
//
// ctx is honoured throughout: a cancelled context fails the Send, and a
// context with a deadline bounds the whole transaction. A context without a
// deadline leaves the transaction unbounded, so hosts that cannot tolerate a
// hung relay must carry a deadline.
func (m *smtpMailer) Send(ctx context.Context, mail Mail) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMail(mail); err != nil {
		return err
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	defer func() {
		if err != nil {
			err = fmt.Errorf("pkgcore: send mail via smtp %s: %w", addr, err)
		}
	}()

	conn, err := m.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return err
	}
	defer client.Close()

	// Hello both announces us and loads the relay's extensions, which the TLS
	// upgrade and the AUTH step below both depend on.
	if err := client.Hello(m.cfg.Host); err != nil {
		return err
	}
	if err := m.upgradeToTLS(client); err != nil {
		return err
	}
	if err := m.authenticate(client); err != nil {
		return err
	}

	if err := client.Mail(mail.From); err != nil {
		return err
	}
	for _, recipient := range mail.To {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(buildMessage(mail)); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	// Quit is the polite goodbye: it sends QUIT and waits for the relay's 221,
	// so the mailer only reports success once the relay has accepted the
	// message. The deferred Close covers every path that fails before this.
	return client.Quit()
}

// dial opens the TCP connection to the relay, wrapping it in TLS first when
// the TLS mode dials TLS from the first byte. The context of the Send that
// started it cancels the dial, and a context deadline is applied to the whole
// connection, which is what lets a cancelled or timed-out Send interrupt a
// relay that stops answering mid-transaction.
func (m *smtpMailer) dial(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
	}

	if m.tlsFromFirstByte() {
		tlsConn := tls.Client(conn, m.tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return conn, nil
}

// tlsFromFirstByte reports whether the configured TLS mode wraps the
// connection in TLS before the SMTP greeting.
func (m *smtpMailer) tlsFromFirstByte() bool {
	switch m.cfg.TLSMode {
	case SMTPTLSModeImplicitTLS:
		return true
	case SMTPTLSModeAuto:
		return m.cfg.Port == 465
	default:
		return false
	}
}

// upgradeToTLS upgrades the plaintext connection with STARTTLS, when the TLS
// mode calls for it and the relay advertises it. smtp.Client.StartTLS
// re-runs the hello on the TLS connection, so the extensions the AUTH step
// consults are the post-TLS ones.
func (m *smtpMailer) upgradeToTLS(client *smtp.Client) error {
	// A mode that dials TLS from the first byte has nothing left to upgrade.
	if m.tlsFromFirstByte() {
		return nil
	}
	advertised, _ := client.Extension("STARTTLS")
	if !advertised {
		return nil
	}
	return client.StartTLS(m.tlsConfig())
}

// authenticate performs SMTP AUTH when the configuration carries credentials.
// PlainAuth is deliberately refused by net/smtp on a connection that never
// became TLS, so a plaintext relay silently receives no credentials rather
// than a leak.
func (m *smtpMailer) authenticate(client *smtp.Client) error {
	if m.cfg.Username == "" {
		return nil
	}
	auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	return client.Auth(auth)
}

// buildMessage renders mail as one MIME message ready for SMTP DATA: RFC 5322
// headers with CRLF line endings, then the body. The subject header stays
// readable ASCII when it already is one and switches to RFC 2047 encoded-word
// form otherwise. Bodies are normalized to CRLF, and the smtp package
// dot-stuffs the payload on the wire, which needs the writer untouched by any
// hand-rolled escaping. The message carries a single Content-Type value, and
// the body it names: text/plain, text/html, or multipart/alternative with
// both bodies as parts in the order a decreasingly capable recipient should
// prefer them. The boundary in the Content-Type header and the one the parts
// use come from the same multipart.Writer, because the header must be written
// before the body but the writer's boundary is only settled once.
func buildMessage(mail Mail) []byte {
	contentType, body := renderBody(mail)

	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\n", mail.From)
	fmt.Fprintf(&message, "To: %s\r\n", strings.Join(mail.To, ", "))
	fmt.Fprintf(&message, "Subject: %s\r\n", encodeSubject(mail.Subject))
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	message.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&message, "Content-Type: %s\r\n", contentType)
	message.WriteString("\r\n")
	message.WriteString(body)
	return message.Bytes()
}

// renderBody returns the message's Content-Type value together with the body
// block that follows the headers. A single body is normalized to CRLF line
// endings; a two-body message comes back as one multipart/alternative
// document whose boundary the Content-Type value names.
func renderBody(mail Mail) (contentType, body string) {
	normalize := func(s string) string {
		// Lone \n becomes \r\n; a body that already used \r\n survives
		// unchanged because it is first collapsed to \n.
		return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	}

	switch {
	case mail.Text != "" && mail.HTML != "":
		var parts bytes.Buffer
		writer := multipart.NewWriter(&parts)
		writePart := func(partType, content string) {
			headers := make(textproto.MIMEHeader)
			headers.Set("Content-Type", partType)
			part, err := writer.CreatePart(headers)
			if err != nil {
				// A bytes.Buffer never fails a write; the error exists only to
				// satisfy the Writer interface.
				panic(fmt.Sprintf("pkgcore: multipart body write failed: %v", err))
			}
			part.Write([]byte(normalize(content)))
		}
		writePart("text/plain; charset=utf-8", mail.Text)
		writePart("text/html; charset=utf-8", mail.HTML)
		if err := writer.Close(); err != nil {
			panic(fmt.Sprintf("pkgcore: multipart body close failed: %v", err))
		}
		return "multipart/alternative; boundary=" + writer.Boundary(), parts.String()
	case mail.HTML != "":
		return "text/html; charset=utf-8", normalize(mail.HTML)
	default:
		return "text/plain; charset=utf-8", normalize(mail.Text)
	}
}

// encodeSubject leaves an ASCII subject alone and encodes a subject that
// carries non-ASCII characters as an RFC 2047 encoded word, so the subject
// survives relays that only guarantee 7-bit transport.
func encodeSubject(subject string) string {
	if isASCII(subject) {
		return subject
	}
	return mime.QEncoding.Encode("utf-8", subject)
}

// isASCII reports whether s is entirely ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
