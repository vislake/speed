package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinMailerRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	impl, caps, err := MailerRegistry.Build("mailer.console", Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "mailer.console", err)
	}
	if impl == nil {
		t.Error("Build(\"mailer.console\") returned a nil Mailer")
	}
	if caps != Stateless {
		t.Errorf("Build(%q) capabilities = %v, want Stateless", "mailer.console", caps)
	}

	impl, caps, err = MailerRegistry.Build("mailer.smtp", Config{"host": "smtp.example.com"})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "mailer.smtp", err)
	}
	if impl == nil {
		t.Error("Build(\"mailer.smtp\") returned a nil Mailer")
	}
	// mailer.smtp declares Stateless (it holds no cross-call state -- every
	// Send dials fresh) alongside MultiReplicaSafe, which the distributed
	// mode's requirement asks of every seam; see the registration comment in
	// mailer_registry.go.
	if want := MultiReplicaSafe | Stateless; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "mailer.smtp", caps, want)
	}
}

func TestSMTPMailerFromConfig_MissingHostReturnsErrMissingSeamConfig(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{})
	if !errors.Is(err, ErrMissingSeamConfig) {
		t.Fatalf("smtpMailerFromConfig(Config{}) error = %v, want it to wrap ErrMissingSeamConfig", err)
	}
}

func TestSMTPMailerFromConfig_InvalidTLSModeReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "tls_mode": "quantum"})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with an invalid tls_mode succeeded, want an error")
	}
}

// TestSMTPMailerFromConfig_ReplyToWithALineBreakReturnsError pins the
// config-key layer of the Reply-To rule: a "reply_to" carrying a line break is
// refused with an error, never handed to the constructor whose own line break
// panic would swallow it.
func TestSMTPMailerFromConfig_ReplyToWithALineBreakReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{
		"host":     "smtp.example.com",
		"reply_to": "ops@example.com\r\nBcc: sneaky@example.com",
	})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with a line break in reply_to succeeded, want an error")
	}
}

func TestSMTPMailerFromConfig_InvalidPortReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "port": "not-a-number"})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with an invalid port succeeded, want an error")
	}
}

// TestSMTPMailerFromConfig_OutOfRangePortReturnsError pins the port range at
// the seam's own boundary: a numeric port outside 1..65535 must come back as
// an error, the same shape every other invalid Config value does, never as
// the constructor panic NewSMTPMailer raises for a hand-built SMTPConfig.
// SeamRegistry.Build is documented to return an error and never panic, so a
// value the registry path accepts must not reach a panicking constructor.
func TestSMTPMailerFromConfig_OutOfRangePortReturnsError(t *testing.T) {
	for _, raw := range []string{"0", "-1", "65536", "99999"} {
		t.Run(raw, func(t *testing.T) {
			_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "port": raw})
			if err == nil {
				t.Fatalf("smtpMailerFromConfig() with port %q succeeded, want an error", raw)
			}
			if !strings.Contains(err.Error(), "1..65535") {
				t.Errorf("smtpMailerFromConfig() error = %v, want it to name the accepted range 1..65535", err)
			}
		})
	}
}

// TestSMTPMailerFromConfig_PortBoundariesAccepted pins the inclusive ends of
// the range above: 1 and 65535 are usable ports and build a mailer.
func TestSMTPMailerFromConfig_PortBoundariesAccepted(t *testing.T) {
	for _, raw := range []string{"1", "65535"} {
		t.Run(raw, func(t *testing.T) {
			impl, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "port": raw})
			if err != nil {
				t.Fatalf("smtpMailerFromConfig() with port %q error = %v, want nil", raw, err)
			}
			if impl == nil {
				t.Fatalf("smtpMailerFromConfig() with port %q returned a nil Mailer", raw)
			}
		})
	}
}
