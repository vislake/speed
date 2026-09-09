package pkgcore

import (
	"fmt"
	"strconv"
	"strings"
)

// MailerRegistry mirrors EventBusRegistry for the "mailer" seam. Both of its
// built-ins, "mailer.console" and "mailer.smtp", register here: net/smtp is
// standard library, so the SMTP mailer earns no dependency-isolation benefit
// from a subpackage of its own (unlike the Redis and S3 implementations).
var MailerRegistry = newBuiltinMailerRegistry()

func newBuiltinMailerRegistry() *SeamRegistry[Mailer] {
	r := NewSeamRegistry[Mailer]()
	mustRegister(r, Registration[Mailer]{
		// Stateless: each Send writes the message to its writer and returns,
		// so a restart drops nothing this implementation holds -- which is
		// why Bootstrap must not print the non-survives-restart banner over
		// it (see Stateless's own doc comment in capability.go).
		Name:         "mailer.console",
		Capabilities: Stateless,
		New:          func(Config) (Mailer, error) { return NewConsoleMailer(), nil },
	})
	mustRegister(r, Registration[Mailer]{
		// The classification trichotomy (holds state and outlives a
		// restart -> SurvivesRestart; holds state and does not -> neither,
		// Bootstrap warns; holds NO state -> Stateless) classifies
		// mailer.smtp as Stateless the same way it classifies
		// mailer.console: every Send dials a fresh connection to the relay
		// (net/smtp's smtp.NewClient per Send; see smtp_mailer.go) and the
		// struct holds only its config, so there is no cross-call state a
		// restart could drop -- nothing this implementation holds survives
		// or fails to survive, the relay's own durability being the
		// relay's business. MultiReplicaSafe stays: DeploymentModeDistributed
		// requires it of every seam, and any number of replicas sharing
		// one relay is exactly the bit's promise, vacuously satisfied by a
		// connection-per-Send shape. warnIfNotDurable skips a Stateless
		// implementation.
		Name:         "mailer.smtp",
		Capabilities: MultiReplicaSafe | Stateless,
		New:          smtpMailerFromConfig,
	})
	return r
}

// smtpMailerFromConfig adapts Config onto NewSMTPMailer. Host has no safe
// default -- there is no such thing as a generic SMTP relay -- so a Config
// missing it is rejected with ErrMissingSeamConfig before NewSMTPMailer is
// even called, rather than letting that constructor's own panic (its
// unrecoverable-wiring-error convention for a caller that built an SMTPConfig
// by hand) surface through a SeamRegistry.Build call that is documented to
// return an error, never to panic.
func smtpMailerFromConfig(cfg Config) (Mailer, error) {
	host := cfg["host"]
	if host == "" {
		return nil, fmt.Errorf("pkgcore: builtin mailer.smtp seam: %w: requires \"host\"", ErrMissingSeamConfig)
	}

	port := 587 // the submission port: plaintext first, STARTTLS when advertised
	if raw, ok := cfg["port"]; ok && raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("pkgcore: builtin mailer.smtp seam: invalid \"port\" %q: %w", raw, err)
		}
		port = parsed
	}

	tlsMode, err := parseSMTPTLSMode(cfg["tls_mode"])
	if err != nil {
		return nil, fmt.Errorf("pkgcore: builtin mailer.smtp seam: %w", err)
	}

	return NewSMTPMailer(SMTPConfig{
		Host:               host,
		Port:               port,
		Username:           cfg["username"],
		Password:           cfg["password"],
		TLSMode:            tlsMode,
		InsecureSkipVerify: cfg["insecure_skip_verify"] == "true",
	}), nil
}

// parseSMTPTLSMode maps a Config string onto an SMTPTLSMode, defaulting to
// SMTPTLSModeAuto -- the same default the zero-value SMTPConfig.TLSMode
// carries -- for an unset value.
func parseSMTPTLSMode(raw string) (SMTPTLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return SMTPTLSModeAuto, nil
	case "starttls":
		return SMTPTLSModeStartTLS, nil
	case "implicit", "implicit_tls":
		return SMTPTLSModeImplicitTLS, nil
	default:
		return 0, fmt.Errorf("invalid \"tls_mode\" %q: want one of \"auto\", \"starttls\", \"implicit\"", raw)
	}
}
