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
		// The declaration-audit trichotomy (holds state and outlives a
		// restart -> SurvivesRestart; holds state and does not -> neither,
		// Bootstrap warns; holds NO state -> Stateless) classifies
		// mailer.smtp as Stateless the same way it classifies
		// mailer.console: every Send dials a fresh connection to the relay
		// (net/smtp's smtp.NewClient per Send; see smtp_mailer.go) and the
		// struct holds only its config, so there is no cross-call state a
		// restart could drop. The pre-audit declaration carried
		// SurvivesRestart over that empty claim -- nothing this
		// implementation holds survives or fails to survive, the relay's
		// own durability being the relay's business -- and this round drops
		// it. MultiReplicaSafe stays: DeploymentModeDistributed requires it
		// of every seam, and any number of replicas sharing one relay is
		// exactly the bit's promise, vacuously satisfied by a
		// connection-per-Send shape. warnIfNotDurable skips a Stateless
		// implementation, so the banner behaviour is unchanged by the
		// audit.
		Name:         "mailer.smtp",
		Capabilities: MultiReplicaSafe | Stateless,
		New:          smtpMailerFromConfig,
	})
	return r
}

func newBuiltinObjectStoreRegistry() *SeamRegistry[ObjectStore] {
	r := NewSeamRegistry[ObjectStore]()
	mustRegister(r, Registration[ObjectStore]{
		// Known limitation of the one-Registration-one-capability-set shape
		// versus a capability that depends on Config, recorded rather than
		// fixed: Capabilities is unconditionally 0 while
		// localObjectStoreFromConfig has two durability modes -- a
		// throwaway MkdirTemp root (honestly 0: the random root dies with
		// the process that created it, since nothing after a restart can
		// find it again, and it is the shape every config-less preset build
		// takes) and a host-supplied persistent cfg["directory"] whose
		// objects genuinely outlive a process restart (the directory
		// outlasts the process), over which the warnIfNotDurable startup
		// banner names a loss that does not exist. The registration is
		// deliberately NOT split into two names this round: under-declaring
		// is the safe direction (warnIfNotDurable treats SurvivesRestart
		// and Stateless equivalently, no deployment mode requires the bit,
		// and a host with a persistent directory can inject the store
		// directly with WithObjectStore(store, SurvivesRestart) when it
		// wants the banner gone), and splitting would change the name space
		// hosts and Presets already pin.
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
		// The temp directory was created by this registration and is owned
		// by it: the returned value's Close() error removes the directory
		// again, so a Kernel.Shutdown (or a failed Bootstrap, which closes
		// what it resolved) does not leak the throwaway tree. A store over
		// a host-supplied cfg["directory"] never carries a closer: that
		// directory is the host's data, which nothing here may delete.
		store := NewLocalObjectStore(created)
		return &closableObjectStore{ObjectStore: store, removeRoot: func() error {
			return os.RemoveAll(created)
		}}, nil
	}
	return NewLocalObjectStore(directory), nil
}

// closableObjectStore is the value "objectstore.local"'s registration
// returns when it created the store's directory itself: the store itself
// (whose promoted methods satisfy ObjectStore) plus the Close() error method
// that removes the temporary directory the registration created, per the
// Registration-level resource-ownership contract. A host that calls
// NewLocalObjectStore itself keeps owning its directory, exactly as that
// constructor's own doc comment promises.
type closableObjectStore struct {
	ObjectStore
	removeRoot func() error
}

// Close removes the temporary directory the registration created. Nothing
// may use the store after Close; a host shuts its seams down last.
func (s *closableObjectStore) Close() error {
	if s.removeRoot != nil {
		return s.removeRoot()
	}
	return nil
}
