package pkgcore

import (
	"errors"
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
	// mailer_builtins.go.
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

func TestSMTPMailerFromConfig_InvalidPortReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "port": "not-a-number"})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with an invalid port succeeded, want an error")
	}
}
