package pkgcore

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrMissingSeamConfig is returned by a built-in Registration's constructor
// when cfg lacks a key the implementation cannot run without and has no safe
// default for -- an SMTP relay host, an S3 bucket and its credentials (the
// latter registered by the objectstore/s3 subpackage, not here). It never
// fires for the in-process seams, which need no configuration at all.
var ErrMissingSeamConfig = errors.New("pkgcore: seam implementation is missing required configuration")

// EventBusRegistry is the package-level SeamRegistry every Kernel resolves
// the "eventbus" Preset key against, pre-populated below with pkgcore's
// in-process built-in implementation. A host registers its own
// implementation -- including pkgcore's own Redis-backed one, which
// registers "eventbus.redis" through the eventbus/redis subpackage's own
// init(), not here -- by calling EventBusRegistry.Register before
// bootstrapping a Kernel whose Preset names it.
var EventBusRegistry = newBuiltinEventBusRegistry()

// KVStoreRegistry mirrors EventBusRegistry for the "kv" seam ("kv.redis"
// registers through the kv/redis subpackage).
var KVStoreRegistry = newBuiltinKVStoreRegistry()

// MailerRegistry mirrors EventBusRegistry for the "mailer" seam. Both of its
// built-ins, "mailer.console" and "mailer.smtp", register here: net/smtp is
// standard library, so the SMTP mailer earns no dependency-isolation benefit
// from a subpackage of its own (unlike the Redis and S3 implementations).
var MailerRegistry = newBuiltinMailerRegistry()

// ObjectStoreRegistry mirrors EventBusRegistry for the "objectstore" seam
// ("objectstore.s3" registers through the objectstore/s3 subpackage).
var ObjectStoreRegistry = newBuiltinObjectStoreRegistry()

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against names this same file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from: the same unrecoverable
// startup-error convention NewLocalObjectStore and NewSMTPMailer already use
// for a wiring mistake that cannot be corrected at runtime. The three
// split-out subpackages (eventbus/redis, kv/redis, objectstore/s3) each
// carry an unexported copy of this same helper, since it cannot be exported
// from here without exposing an implementation detail no other caller needs.
func mustRegister[T any](registry *SeamRegistry[T], r Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore: builtin implementation registration failed: %v", err))
	}
}

func newBuiltinEventBusRegistry() *SeamRegistry[EventBus] {
	r := NewSeamRegistry[EventBus]()
	mustRegister(r, Registration[EventBus]{
		Name:         "eventbus.memory",
		Capabilities: 0,
		New:          func(Config) (EventBus, error) { return NewMemoryEventBus(), nil },
	})
	return r
}

func newBuiltinKVStoreRegistry() *SeamRegistry[KVStore] {
	r := NewSeamRegistry[KVStore]()
	mustRegister(r, Registration[KVStore]{
		Name:         "kv.memory",
		Capabilities: 0,
		New:          func(Config) (KVStore, error) { return NewMemoryKVStore(), nil },
	})
	return r
}

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
		Name:         "mailer.smtp",
		Capabilities: MultiReplicaSafe | SurvivesRestart,
		New:          smtpMailerFromConfig,
	})
	return r
}

func newBuiltinObjectStoreRegistry() *SeamRegistry[ObjectStore] {
	r := NewSeamRegistry[ObjectStore]()
	mustRegister(r, Registration[ObjectStore]{
		Name:         "objectstore.local",
		Capabilities: 0,
		New:          localObjectStoreFromConfig,
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

// localObjectStoreFromConfig adapts Config onto NewLocalObjectStore. Unlike
// the SMTP and S3 seams, a directory is always constructible: cfg["directory"]
// names a persistent one when the host wants objects to survive a restart,
// and an empty value falls back to a fresh private temporary directory --
// the same throwaway-by-default behaviour the pre-retrofit Kernel's
// DeploymentModeStandalone case had.
func localObjectStoreFromConfig(cfg Config) (ObjectStore, error) {
	directory := cfg["directory"]
	if directory == "" {
		created, err := os.MkdirTemp("", "pkgcore-object-store-*")
		if err != nil {
			return nil, fmt.Errorf("pkgcore: builtin objectstore.local seam: %w", err)
		}
		directory = created
	}
	return NewLocalObjectStore(directory), nil
}
