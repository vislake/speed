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
// they are called.
//
// PermissionRevoke and PermissionRotate are round 3's additions -- round
// 1's AGENTS.md reserved both names ahead of time, deliberately undeclared
// while the module could not yet perform either operation. Both now have a
// real operation behind them: PermissionRevoke gates the HTTP revoke
// operations (handler.go), and PermissionRotate names the rotation this
// module has performed automatically since round 2 (lifecycle.go's
// ScanExpiry) and can now also perform on demand (Service.PromoteNow) --
// neither permission is enforced by anything in this module (rbac's job,
// per this file's own doc comment), so declaring PermissionRotate now, with
// no HTTP operation gated by it yet, is not the same "prepare for later"
// round 1 refused: the capability itself is real and already running, only
// a manual, permission-gated trigger for it is still future work.
const (
	// PermissionRead covers reading signing keys, authorities and
	// certificates.
	PermissionRead = "pki:read"
	// PermissionIssue covers creating CAs and issuing certificates.
	PermissionIssue = "pki:issue"
	// PermissionRevoke covers revoking a signing key or a certificate.
	PermissionRevoke = "pki:revoke"
	// PermissionRotate covers manually triggering key rotation
	// (Service.PromoteNow) -- automatic rotation (the expiry scan) needs no
	// permission check, since nothing external calls it.
	PermissionRotate = "pki:rotate"
)

// The audit actions pki contributes. pki.key.rotate and
// pki.private_key.deliver stay undeclared -- rotation is a system-driven
// background process with no single human "who did this" the audit trail's
// Actor/Resource shape is built to answer (round 2 deliberately left it
// out for the identical reason, and PromoteNow's manual trigger above is
// still an operator overriding a system process, not a new kind of
// action), and key delivery is not this round's scope. pki.key.revoke and
// pki.certificate.revoke ARE round 3's job -- see AuditActionKeyRevoke/
// AuditActionCertificateRevoke below.
const (
	// AuditActionAuthorityCreate covers CreateRootCA and CreateIntermediateCA.
	AuditActionAuthorityCreate = "pki.authority.create"
	// AuditActionCertificateIssue covers IssueCertificate.
	AuditActionCertificateIssue = "pki.certificate.issue"
	// AuditActionKeyRevoke covers Service.RevokeSigningKey, recorded by
	// handler.go's PkiRevokeSigningKey -- the same "record at the HTTP
	// boundary, after the write has committed" placement
	// examples/reference-app/internal/notes/handler.go's
	// recordNoteCreatedAudit documents, so this module's own Go API stays
	// free of an audit.Emit dependency it does not otherwise need.
	AuditActionKeyRevoke = "pki.key.revoke"
	// AuditActionCertificateRevoke covers CAService.RevokeCertificate,
	// recorded the identical way by handler.go's PkiRevokeCertificate.
	AuditActionCertificateRevoke = "pki.certificate.revoke"
)

// The configuration keys pki contributes.
//
// ConfigCRLDistributionPoint and ConfigCRLValidity are round 3's additions,
// completing docs/internal/22-pki.md's module-contract configuration list
// (this file's own doc comment already covered every other item on it).
// Neither is read by this round's own code, the identical declare-but-do-
// not-read discipline every config item in this file already follows (see
// this const block's own doc comment on that pattern) --
// ConfigCRLDistributionPoint's own value is never consulted anywhere: a
// caller passes CRLDistributionPoint directly on RootCAParams/
// IntermediateCAParams (ca.go), so this item exists purely as the
// declared, admin-visible schema entry docs/internal/22-pki.md's
// configuration-item list requires, for a host that wants to show or
// validate the value before passing it through its own wiring.
// ConfigCRLValidity's real default lives as DefaultCRLValidity (crl.go),
// which GenerateCRL actually falls back to; a caller wanting the config
// value honored passes it through GenerateCRL's own validity parameter,
// exactly as ConfigPropagationWindow/ConfigRenewalLeadTime's callers pass
// through WithPropagationWindow/WithRenewalLeadTime rather than pki reading
// config itself.
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
	// ConfigCRLDistributionPoint is the default CRL distribution point URL
	// a host may want to show or validate before passing it through
	// RootCAParams/IntermediateCAParams.CRLDistributionPoint -- see this
	// const block's own doc comment for why this round's code never reads
	// it directly.
	ConfigCRLDistributionPoint = "pki.crl_distribution_point"
	// ConfigCRLValidity is how long a generated CRL claims to be current --
	// see DefaultCRLValidity.
	ConfigCRLValidity = "pki.crl_validity"
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
	{
		Key:         ConfigCRLDistributionPoint,
		Type:        "string",
		Default:     "",
		Description: "Default CRL distribution point URL for newly created authorities. Empty means no CRLDistributionPoints extension is written into certificates by default.",
		Group:       "pki",
	},
	{
		Key:         ConfigCRLValidity,
		Type:        "duration",
		Default:     DefaultCRLValidity,
		Description: "How long a generated CRL claims to be current (NextUpdate minus ThisUpdate) before it should be regenerated.",
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
	// revocations is round 3's revocation-ledger repository -- see
	// CertificateRevocation's own model.go doc comment.
	revocations *CertificateRevocationRepository

	service *Service
	ca      *CAService

	// handler is round 3's HTTP surface, built and mounted in Register (not
	// NewModule) so it serves the service/repository instances every
	// Option has already configured by the time Register runs -- the same
	// "build the handler in Register" reasoning storage.Module.Register's
	// own doc comment gives.
	handler *Handler
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
	m.revocations = NewCertificateRevocationRepository(db)

	m.service = NewService(m.signer, m.signerName, m.signingKeys, m.cacheTTL, m.propagationWindow, m.renewalLeadTime)
	m.ca = NewCAService(m.signer, m.signerName, m.authorities, m.certificates, m.revocations)
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

// apiPath is the common prefix pki's HTTP routes are mounted at (see
// Register below). It must agree with the "paths:" keys of
// api/openapi.yaml: api.HandlerFromMux registers the fragment's full
// method+path patterns on Handler's inner mux, and mounting at this prefix
// here only tells the host's outer mux which requests to hand to Handler at
// all -- exactly as every other module fragment's identical constant does.
const apiPath = "/api/v1/pki"

// openAPISpecYAML is pki's OpenAPI fragment, embedded from api/ so the spec
// -- and the generated ServerInterface and types derived from it -- travels
// inside the module binary. Round 3's addition: the sixth module fragment
// overall, after notes, org, authn, storage and notification.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// OpenAPISpec implements pkgcore.Module: pki's own OpenAPI fragment,
// embedded from api/openapi.yaml. Round 3's addition -- the fragment is the
// single source of this module's HTTP surface, and Handler implements the
// api package's generated ServerInterface (see handler.go), per
// docs/internal/21-api-contract.md's spec-first decision.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db.
//
// It declares pki's permissions (round 3 adding PermissionRevoke and
// PermissionRotate to round 1's PermissionRead/PermissionIssue), its audit
// vocabulary (round 3 adding AuditActionKeyRevoke/
// AuditActionCertificateRevoke) and its configuration schema, and declares
// the five signing-key/certificate lifecycle events (events.go). It hands
// the registry's EventBus to both Service and CAService (attachBus) so the
// key-set cache can invalidate itself on its own published events and
// CAService can publish pki.certificate.* ones, and, when the host wired a
// queue via WithQueue, hands both the same queue too and claims BOTH the
// expiry-scan (job.go) and, round 3's addition, the CRL-regenerate (crl.go)
// task handlers, so a host draining reg.Jobs.Handlers() onto its
// jobs.Queue gets workers for both. It also builds and mounts pki's HTTP
// surface, round 3's addition: Handler is built here, not in NewModule, so
// it serves the service and repository instances every Option has already
// configured by the time Register runs (Bootstrap calls Register only
// after NewModule has returned) -- the same reasoning storage.Module's
// identical placement documents. Routes.Mount is a plain registration, no
// I/O, so Register's no-I/O contract stands.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(
		PermissionRead,
		PermissionIssue,
		PermissionRevoke,
		PermissionRotate,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionAuthorityCreate,
		AuditActionCertificateIssue,
		AuditActionKeyRevoke,
		AuditActionCertificateRevoke,
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
	m.ca.attachBus(reg)
	if m.queue != nil {
		m.service.attachQueue(m.queue)
		m.ca.attachQueue(m.queue)
		if err := reg.Jobs.Handle(taskTypeExpiryScan, expiryScanHandler{svc: m.service}); err != nil {
			return err
		}
		if err := reg.Jobs.Handle(taskTypeCRLRegenerate, crlRegenerateHandler{ca: m.ca}); err != nil {
			return err
		}
	}

	m.handler = NewHandler(m.service, m.ca, reg.Events.Bus(), reg.AuditActions)
	reg.Routes.Mount(apiPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
