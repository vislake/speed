package pki

import (
	"embed"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki/locales"
	"github.com/vislake/speed/go/pki/migrations"
)

// moduleName is pki's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "pki"

// The permissions pki contributes to the platform's permission catalog.
// Enforcement belongs to rbac; pki only declares that these exist and what
// they are called. pki:revoke and pki:rotate are round 3/round 2
// permissions respectively and are deliberately not declared here yet --
// declaring a permission for an operation the module cannot perform is
// exactly the kind of "prepare for later" the round split's own boundary
// forbids (see this file's Register doc comment).
const (
	// PermissionRead covers reading signing keys, authorities and
	// certificates.
	PermissionRead = "pki:read"
	// PermissionIssue covers creating CAs and issuing certificates.
	PermissionIssue = "pki:issue"
)

// The audit actions pki contributes. pki.key.rotate, pki.key.revoke,
// pki.certificate.revoke and pki.private_key.deliver are declared by the
// rounds that implement rotation, revocation and key delivery -- an
// audit action nothing ever emits is dead catalog weight, not forward
// compatibility.
const (
	// AuditActionAuthorityCreate covers CreateRootCA and CreateIntermediateCA.
	AuditActionAuthorityCreate = "pki.authority.create"
	// AuditActionCertificateIssue covers IssueCertificate.
	AuditActionCertificateIssue = "pki.certificate.issue"
)

// The configuration keys pki contributes. The CRL distribution point URL
// (round 3) belongs to the round that implements it.
//
// ConfigPropagationWindow and ConfigRenewalLeadTime are round 2's additions.
// Neither duplicates the retiring overlap period: docs/internal/22-pki.md's
// section on why the retiring overlap period's length is declared by the
// consumer is explicit that pki does not know a credential's maximum
// lifetime -- the consumer declares it, once,
// through EnsurePurpose's maxCredentialLifetime parameter, and it is
// recorded per-key (SigningKey.RetiringOverlap), never as a global setting.
// The propagation window and the rotation lead time are the opposite case
// -- docs/internal/22-pki.md's own words describe the rotation cadence as
// something the consumer has no way to know, that belongs to pki's own
// configuration -- genuinely pki's own settings, with no consumer
// that could supply them.
const (
	// ConfigCADefaultValidity is how long a CA certificate (root or
	// intermediate) is valid for when a caller does not specify one.
	ConfigCADefaultValidity = "pki.ca_default_validity"
	// ConfigCAMaxValidity bounds how long a CA certificate may be issued
	// for, regardless of what a caller requests.
	ConfigCAMaxValidity = "pki.ca_max_validity"
	// ConfigCertificateDefaultValidity is how long an end-entity
	// certificate is valid for when a caller does not specify one.
	ConfigCertificateDefaultValidity = "pki.certificate_default_validity"
	// ConfigCertificateMaxValidity bounds how long an end-entity
	// certificate may be issued for, regardless of what a caller requests.
	ConfigCertificateMaxValidity = "pki.certificate_max_validity"
	// ConfigPropagationWindow is how long a newly staged pending key waits
	// before the expiry scan promotes it to active -- see
	// DefaultPropagationWindow.
	ConfigPropagationWindow = "pki.propagation_window"
	// ConfigRenewalLeadTime is how far ahead of a signing key's expiry the
	// expiry scan stages its replacement -- see DefaultRenewalLeadTime.
	ConfigRenewalLeadTime = "pki.renewal_lead_time"
)

// Default validity periods backing the CA/certificate config items above.
// None of this round's issuance methods (CreateRootCA, CreateIntermediateCA,
// IssueCertificate) reads these through the config schema yet -- Register
// only declares the schema, per pkgcore.Module.Register's own "must not
// perform I/O; it only declares" contract, and pki carries no config.Service
// dependency to read a live value with. A caller of this round's Go API
// passes NotAfter directly (see RootCAParams/IntermediateCAParams/
// CertificateParams); wiring these declared config keys into that decision
// is left to the host, or to a later round.
//
// ConfigPropagationWindow and ConfigRenewalLeadTime follow the identical
// declare-but-do-not-read discipline: their defaults are
// DefaultPropagationWindow and DefaultRenewalLeadTime (lifecycle.go), read
// by NewModule (via WithPropagationWindow/WithRenewalLeadTime, or those
// package defaults when the host passes neither) rather than through a live
// config lookup, for the same reason -- no config.Service dependency exists
// this round to read one with.
const (
	defaultCADefaultValidity          = 10 * 365 * 24 * time.Hour
	defaultCAMaxValidity              = 15 * 365 * 24 * time.Hour
	defaultCertificateDefaultValidity = 365 * 24 * time.Hour
	defaultCertificateMaxValidity     = 2 * 365 * 24 * time.Hour
)

// configItemDecls is the catalog entry for each config item, declared in
// Register.
var configItemDecls = []pkgcore.ConfigItem{
	{
		Key:         ConfigCADefaultValidity,
		Type:        "duration",
		Default:     defaultCADefaultValidity,
		Description: "Default validity period for a newly issued CA certificate (root or intermediate) when the caller does not specify one.",
		Group:       "pki",
	},
	{
		Key:         ConfigCAMaxValidity,
		Type:        "duration",
		Default:     defaultCAMaxValidity,
		Description: "Maximum validity period a CA certificate may be issued for, regardless of what the caller requests.",
		Group:       "pki",
	},
	{
		Key:         ConfigCertificateDefaultValidity,
		Type:        "duration",
		Default:     defaultCertificateDefaultValidity,
		Description: "Default validity period for a newly issued end-entity certificate when the caller does not specify one.",
		Group:       "pki",
	},
	{
		Key:         ConfigCertificateMaxValidity,
		Type:        "duration",
		Default:     defaultCertificateMaxValidity,
		Description: "Maximum validity period an end-entity certificate may be issued for, regardless of what the caller requests.",
		Group:       "pki",
	},
	{
		Key:         ConfigPropagationWindow,
		Type:        "duration",
		Default:     DefaultPropagationWindow,
		Description: "How long a newly staged pending signing key waits before the expiry scan promotes it to active.",
		Group:       "pki",
	},
	{
		Key:         ConfigRenewalLeadTime,
		Type:        "duration",
		Default:     DefaultRenewalLeadTime,
		Description: "How far ahead of a signing key's expiry the expiry scan stages its replacement.",
		Group:       "pki",
	},
}

// Module implements pkgcore.Module for go/pki.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Constructing a Module performs no I/O: db is opened and migrated by the
// host before Register is ever called, exactly like every other module in
// this codebase.
//
// # Default Signer
//
// Without WithSigner, NewModule wires LocalSigner over db -- the zero-
// external-dependency signer this round ships, which is also what "task
// dev" runs. A future round's vault/kmsaws provider subpackages (round 4)
// slot in through the same WithSigner option; this round builds no
// self-registering SignerRegistry for them to register into (see AGENTS.md's
// Known limitations for why that is explicitly deferred, not an oversight).
type Module struct {
	db *gorm.DB

	signer     Signer
	signerName string

	// queue is the jobs.Queue the expiry scan's task handler registers
	// against (WithQueue). Nil is a legitimate configuration -- see
	// Service.queue's own doc comment.
	queue jobs.Queue

	// cacheTTL, propagationWindow and renewalLeadTime are Service's
	// constructor arguments, defaulted to DefaultCacheTTL/
	// DefaultPropagationWindow/DefaultRenewalLeadTime and overridable via
	// WithCacheTTL/WithPropagationWindow/WithRenewalLeadTime.
	cacheTTL          time.Duration
	propagationWindow time.Duration
	renewalLeadTime   time.Duration

	signingKeys  *SigningKeyRepository
	authorities  *AuthorityRepository
	certificates *CertificateRepository
	localKeys    *LocalKeyRepository

	service *Service
	ca      *CAService
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithSigner overrides the default LocalSigner with signer, recorded on
// every issued row under name. A host wiring a KMS-backed implementation
// (round 4) uses this to swap it in without touching any other part of the
// module's wiring.
func WithSigner(name string, signer Signer) Option {
	return func(m *Module) {
		m.signerName = name
		m.signer = signer
	}
}

// WithQueue wires the jobs.Queue the expiry-scan task (job.go) is scheduled
// on and claimed from. Without it, Module still works in full for the
// key-lifecycle layer's synchronous surface (EnsurePurpose, ActiveSigner,
// VerificationKeys) and for the X.509 layer -- only automatic rotation is
// unavailable: Register skips claiming the task handler, and
// Service.EnqueueExpiryScan reports a plain error, the same optional-queue
// shape storage.WithQueue's absence produces for its own expiry sweep.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithCacheTTL overrides DefaultCacheTTL for the key-set cache
// (Service.ActiveSigner/VerificationKeys' hot-path cache, cache.go). A
// value <=0 disables the cache outright -- useful for a test that wants to
// observe every write immediately without waiting out a TTL.
func WithCacheTTL(ttl time.Duration) Option {
	return func(m *Module) { m.cacheTTL = ttl }
}

// WithPropagationWindow overrides DefaultPropagationWindow: how long a
// newly staged pending key waits before the expiry scan promotes it to
// active. See lifecycle.go's "pending" state doc comment for why the wait
// exists at all.
func WithPropagationWindow(d time.Duration) Option {
	return func(m *Module) { m.propagationWindow = d }
}

// WithRenewalLeadTime overrides DefaultRenewalLeadTime: how far ahead of a
// signing key's expiry the expiry scan stages its replacement.
func WithRenewalLeadTime(d time.Duration) Option {
	return func(m *Module) { m.renewalLeadTime = d }
}

// NewModule returns a Module whose tables live in db, signing through
// LocalSigner unless overridden by WithSigner. Constructing a Module
// performs no I/O: opening and migrating db, and registering
// LocalKeySerializerName against a cipher (RegisterLocalKeySerializer), are
// the host's responsibility, done before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{
		db:                db,
		signerName:        "local",
		cacheTTL:          DefaultCacheTTL,
		propagationWindow: DefaultPropagationWindow,
		renewalLeadTime:   DefaultRenewalLeadTime,
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.signer == nil {
		m.signer = NewLocalSigner(db)
	}

	m.signingKeys = NewSigningKeyRepository(db)
	m.authorities = NewAuthorityRepository(db)
	m.certificates = NewCertificateRepository(db)
	m.localKeys = NewLocalKeyRepository(db)

	m.service = NewService(m.signer, m.signerName, m.signingKeys, m.cacheTTL, m.propagationWindow, m.renewalLeadTime)
	m.ca = NewCAService(m.signer, m.signerName, m.authorities, m.certificates)
	return m
}

// Close releases the module's background resources -- today, exactly the
// key-set cache's janitor goroutine (Service.Close). Idempotent, and the
// module stays correct, if slower to reclaim memory, if a host never calls
// it.
func (m *Module) Close() error {
	return m.service.Close()
}

// Service returns the module's key-lifecycle Service.
func (m *Module) Service() *Service { return m.service }

// CA returns the module's X.509 CAService.
func (m *Module) CA() *CAService { return m.ca }

// Signer returns the module's configured Signer.
func (m *Module) Signer() Signer { return m.signer }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing. pki sits above dbkit and
// tenancy in docs/internal/01-architecture.md's graph, but neither is a
// pkgcore.Module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of pki's error codes,
// in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. pki has no HTTP surface this
// round -- see AGENTS.md's Known limitations -- so this returns nil, the
// same "no fragment yet" answer go/config's Module gives.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db.
//
// It declares pki's permissions, its round-1 audit vocabulary and its
// configuration schema, and declares the three signing-key lifecycle events
// (events.go). It hands the registry's EventBus to Service so the key-set
// cache can invalidate itself on its own published events (attachBus), and,
// when the host wired a queue via WithQueue, hands Service that queue too
// and claims the expiry-scan task's handler (job.go) so a host draining
// reg.Jobs.Handlers() onto its jobs.Queue gets a worker that advances the
// lifecycle state machine. It does not mount any HTTP route -- no OpenAPI
// fragment exists yet, still round 3's job.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(
		PermissionRead,
		PermissionIssue,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionAuthorityCreate,
		AuditActionCertificateIssue,
	); err != nil {
		return err
	}
	if err := reg.Config.Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.Events.Publishes(eventDecls...); err != nil {
		return err
	}
	m.service.attachBus(reg)
	if m.queue != nil {
		m.service.attachQueue(m.queue)
		if err := reg.Jobs.Handle(taskTypeExpiryScan, expiryScanHandler{svc: m.service}); err != nil {
			return err
		}
	}
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
