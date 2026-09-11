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
// Enforcement belongs to rbac and to the host's router gate; pki only
// declares that these exist and what they are called.
//
// # The evaluation domain is part of each permission's meaning
//
// rbac evaluates every permission inside SOME tenant's scope
// (Subject{TenantID, UserID}), and pki's tables span two data domains:
// pki_signing_keys and pki_authorities are platform data with no tenant at
// all, while pki_certificates is tenant data. A host's gate must therefore
// evaluate the signing-key permission under the PLATFORM domain
// (rbac.SystemDomain -- the domain go/admin's own admin:* permissions are
// evaluated in, per that module's wiring), NEVER under the request's
// tenant domain: a single tenant's grant of a platform-level permission
// would otherwise stop token issuance for the whole deployment at once.
// The certificate permission is evaluated in the request's own tenant
// domain, like every tenant-scoped permission. This module itself performs
// no permission check (rbac's job, per this file's own doc comment); the
// obligation is recorded here so a host gate cannot improvise a wrong
// domain.
//
// PermissionRevokeSigningKey and PermissionRevokeCertificate exist as two
// names, not one: a single name spanning the module's two data domains
// would be wrong in whichever single domain it were evaluated in, and the
// permission catalog freezes on the names declared, so no spanning name
// survives to half mean something. PermissionRead and PermissionIssue's
// declared coverage similarly spans the module's two domains where no HTTP
// operation exists yet: pki:read's HTTP surface is platform material only
// (see its own comment), and pki:issue has NO HTTP operation at all -- an
// HTTP issuance surface must split it into a platform and a tenant
// permission before it lands, never add an operation on one domain or the
// other under this name.
const (
	// PermissionRead covers the read operations of this module's HTTP
	// surface: the key-lifecycle JWKS export, an authority-chain JWKS
	// export and an authority's CRL fetch (handler.go). All three serve
	// platform material -- pki_signing_keys / pki_authorities public keys
	// and CRL documents, the material the deployment intends external
	// verifiers to fetch -- and none of them reads pki_certificates:
	// certificate reads have no HTTP operation, and this permission does
	// not cover one that might be added -- a tenant-certificate read
	// surface must declare its own tenant-domain permission.
	PermissionRead = "pki:read"
	// PermissionIssue covers creating CAs and issuing certificates. It has
	// no HTTP operation behind it (issuance stays Go-only), so no gate
	// evaluates it yet -- see this const block's own doc comment for why
	// an HTTP issuance surface must split it into a platform CA permission
	// and a tenant certificate permission before it lands, rather than
	// mounting either operation under this two-domain name.
	PermissionIssue = "pki:issue"
	// PermissionRevokeSigningKey gates PkiRevokeSigningKey (handler.go):
	// revoking one row of pki_signing_keys, platform data -- a platform
	// operation whose permission MUST be evaluated in the platform domain
	// (rbac.SystemDomain), never in the request tenant's domain: a key
	// this deployment signs every tenant's tokens with must not be
	// revocable by a grant a single tenant's administrator holds.
	PermissionRevokeSigningKey = "pki:revoke_signing_key"
	// PermissionRevokeCertificate gates PkiRevokeCertificate (handler.go):
	// revoking one row of pki_certificates, tenant data -- a tenant-level
	// operation whose permission is evaluated in the request's own tenant
	// domain, like every tenant-scoped permission. A tenant's certificate
	// administrator holding this can revoke that tenant's certificates,
	// and nothing else.
	PermissionRevokeCertificate = "pki:revoke_certificate"
	// PermissionRotate covers manually triggering key rotation
	// (Service.PromoteNow) -- automatic rotation (the expiry scan) needs no
	// permission check, since nothing external calls it.
	PermissionRotate = "pki:rotate"
)

// The audit actions pki contributes. pki.key.rotate and
// pki.private_key.deliver stay undeclared: rotation is a system-driven
// background process with no single human "who did this" for the audit
// trail's Actor/Resource shape to answer -- Service.PromoteNow's manual
// trigger is an operator overriding a system process, not a new kind of
// action -- and no private-key delivery path exists to record. The two
// revoke actions ARE declared -- see AuditActionKeyRevoke/
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
// ConfigCRLDistributionPoint and ConfigCRLValidity follow the identical
// declare-but-do-not-read discipline every config item in this file
// follows: ConfigCRLDistributionPoint's own value is never consulted
// anywhere -- a caller passes CRLDistributionPoint directly on
// CAParams (ca.go), so this item exists purely as
// a declared, admin-visible schema entry, for a host that wants to show or
// validate the value before passing it through its own wiring.
// ConfigCRLValidity's real default lives as DefaultCRLValidity (crl.go),
// which GenerateCRL actually falls back to; a caller wanting the config
// value honored passes it through GenerateCRL's own validity parameter,
// exactly as ConfigPropagationWindow/ConfigRenewalLeadTime's callers pass
// through WithPropagationWindow/WithRenewalLeadTime rather than pki
// reading config itself.
//
// ConfigPropagationWindow and ConfigRenewalLeadTime duplicate neither the
// retiring overlap period: the module does not know a credential's maximum
// lifetime -- the consumer declares it, once, through EnsurePurpose's
// maxCredentialLifetime parameter, and it is recorded per-key
// (SigningKey.RetiringOverlap), never as a global setting. The propagation
// window and the rotation lead time are the opposite case: the rotation
// cadence is something the consumer has no way to know, that belongs to
// pki's own configuration -- genuinely pki's own settings, with no
// consumer that could supply them.
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
	// CAParams.CRLDistributionPoint -- see this
	// const block's own doc comment for why the module's code never reads
	// it directly.
	ConfigCRLDistributionPoint = "pki.crl_distribution_point"
	// ConfigCRLValidity is how long a generated CRL claims to be current --
	// see DefaultCRLValidity.
	ConfigCRLValidity = "pki.crl_validity"
)

// Default validity periods backing the CA/certificate config items above.
// No issuance method (CreateRootCA, CreateIntermediateCA, IssueCertificate)
// reads these through the config schema: Register only declares the schema,
// per pkgcore.Module.Register's own "must not perform I/O; it only
// declares" contract, and pki carries no config.Service dependency to read
// a live value with. A caller passes NotAfter directly (see
// CAParams/CertificateParams); wiring the declared
// config keys into that decision is the host's job.
//
// ConfigPropagationWindow and ConfigRenewalLeadTime follow the identical
// declare-but-do-not-read discipline: their defaults are
// DefaultPropagationWindow and DefaultRenewalLeadTime (lifecycle.go), read
// by NewModule (via WithPropagationWindow/WithRenewalLeadTime, or those
// package defaults when the host passes neither) rather than through a live
// config lookup, for the same reason -- no config.Service dependency exists
// to read one with.
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
// Without WithSigner, NewModule wires LocalSigner over db -- the
// zero-external-dependency signer, which is also what "task dev" runs.
// KMS-backed implementations (vault/kmsaws) reach the module either through
// the same WithSigner option or through pki.SignerRegistry's registered
// names (signer_registry.go).
type Module struct {
	db *gorm.DB

	signer     Signer
	signerName string

	// queue is the jobs.Queue the expiry scan's task handler registers
	// against (WithQueue). Nil is a legitimate configuration -- see
	// Service.queue's own doc comment.
	queue jobs.Queue

	// cacheTTL, propagationWindow, renewalLeadTime and expiryScanWindow are
	// Service's constructor arguments, defaulted to DefaultCacheTTL/
	// DefaultPropagationWindow/DefaultRenewalLeadTime/DefaultExpiryScanWindow
	// and overridable via WithCacheTTL/WithPropagationWindow/
	// WithRenewalLeadTime/WithExpiryScanWindow.
	cacheTTL          time.Duration
	propagationWindow time.Duration
	renewalLeadTime   time.Duration
	expiryScanWindow  time.Duration

	signingKeys  *SigningKeyRepository
	authorities  *AuthorityRepository
	certificates *CertificateRepository
	localKeys    *LocalKeyRepository
	// revocations is the revocation-ledger repository -- see
	// CertificateRevocation's own model.go doc comment.
	revocations *CertificateRevocationRepository

	service *Service
	ca      *CAService

	// handler is the HTTP surface, built and mounted in Register (not
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
// uses this to swap it in without touching any other part of the module's
// wiring.
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

// WithExpiryScanWindow overrides DefaultExpiryScanWindow: the period one
// expiry-scan idempotency key covers (job.go -- EnqueueExpiryScan places
// each enqueue in the window its clock read falls in, so same-window
// enqueues collapse into one job and later windows run the scan again).
// The override exists for two kinds of host: one whose scheduler interval
// approaches or exceeds the default hour (the dedup is void when every
// tick lands in a fresh window, so such a host must widen the window), and
// a test compressing the window below its own tick cadence to drive
// per-tick scans within test time (examples/reference-app's
// periodic_pki_scan_flow_test does exactly that).
func WithExpiryScanWindow(d time.Duration) Option {
	return func(m *Module) { m.expiryScanWindow = d }
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
		expiryScanWindow:  DefaultExpiryScanWindow,
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

	m.service = NewService(m.signer, m.signerName, m.signingKeys, m.cacheTTL, m.propagationWindow, m.renewalLeadTime, m.expiryScanWindow)
	m.ca = NewCAService(m.signer, m.signerName, m.authorities, m.certificates, m.revocations)
	return m
}

// Close releases the module's background resources -- exactly the key-set
// cache's janitor goroutine (Service.Close). Idempotent, and the module
// stays correct, if slower to reclaim memory, if a host never calls it.
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
// tenancy in the module dependency graph, but neither is a pkgcore.Module
// -- they are libraries the host wires, and DependsOn enumerates only
// modules in the bootstrap set.
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

// bootstrapKeyDecl is the process-start key material this module consumes: the
// AES key that seals the LocalSigner private-key column (pki_local_keys, via
// RegisterLocalKeySerializer).
//
// It is a separate secret from every other module's key material, because
// dbkit's key-separation rule applies across modules and not only within one.
// The keys it seals are the ones authn's access tokens are ultimately signed
// with, so a host that leaves it at the development default ships with signing
// keys sealed under a key committed to this repository's own source.
var bootstrapKeyDecl = pkgcore.BootstrapKey{
	Key:         "pki.local_key_cipher_key",
	Format:      "hexkey",
	Default:     "documented non-secret development default",
	Sensitive:   true,
	Description: "AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with; separate from every other key, since dbkit's key-separation rule spans modules, not only one.",
	Group:       moduleName,
}

// openAPISpecYAML is pki's OpenAPI fragment, embedded from api/ so the spec
// -- and the generated ServerInterface and types derived from it -- travels
// inside the module binary.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// OpenAPISpec implements pkgcore.Module: pki's own OpenAPI fragment,
// embedded from api/openapi.yaml -- the single source of this module's HTTP
// surface, with Handler implementing the api package's generated
// ServerInterface (see handler.go) so the spec and its implementation
// cannot drift.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db.
//
// It declares pki's permissions (PermissionRevokeSigningKey,
// PermissionRevokeCertificate and PermissionRotate alongside
// PermissionRead and PermissionIssue -- see that const block's own doc
// comment for why no one permission may span the module's two data
// domains), its audit vocabulary (the create/issue pair and the two revoke
// actions) and its configuration schema, and declares the five
// signing-key/certificate lifecycle events (events.go). It hands the
// registry's EventBus to both Service and CAService (attachBus) so the
// key-set cache can invalidate itself on its own published events and
// CAService can publish pki.certificate.* ones, and, when the host wired a
// queue via WithQueue, hands both the same queue too and claims BOTH the
// expiry-scan (job.go) and the CRL-regenerate (crl.go) task handlers, so a
// host draining reg.Jobs.Handlers() onto its jobs.Queue gets workers for
// both. It also builds and mounts pki's HTTP surface: Handler is built
// here, not in NewModule, so it serves the service and repository
// instances every Option has already configured by the time Register runs
// (Bootstrap calls Register only after NewModule has returned) -- the same
// reasoning storage.Module's identical placement documents. Routes.Mount
// is a plain registration, no I/O, so Register's no-I/O contract stands.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.PermissionsSeat().Add(
		PermissionRead,
		PermissionIssue,
		PermissionRevokeSigningKey,
		PermissionRevokeCertificate,
		PermissionRotate,
	); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(
		AuditActionAuthorityCreate,
		AuditActionCertificateIssue,
		AuditActionKeyRevoke,
		AuditActionCertificateRevoke,
	); err != nil {
		return err
	}
	if err := reg.ConfigSeat().Add(configItemDecls...); err != nil {
		return err
	}
	// The process-start key material (bootstrapKeyDecl) is descriptor data:
	// the component descriptor carries it as BootstrapKeys, which the loader
	// resolves before anything is constructed.
	if err := reg.EventsSeat().Publishes(eventDecls...); err != nil {
		return err
	}
	m.service.attachBus(reg)
	m.ca.attachBus(reg)
	if m.queue != nil {
		m.service.attachQueue(m.queue)
		m.ca.attachQueue(m.queue)
		if err := reg.JobsSeat().Handle(taskTypeExpiryScan, jobs.NewEmptyPayloadHandler(taskTypeExpiryScan, m.service.runScheduledExpiryScan)); err != nil {
			return err
		}
		if err := reg.JobsSeat().Handle(taskTypeCRLRegenerate, jobs.NewEmptyPayloadHandler(taskTypeCRLRegenerate, m.ca.runScheduledCRLRegenerate)); err != nil {
			return err
		}
		// Declare both periodic schedules alongside their handlers:
		// declaring means scheduled, so a host that runs a
		// jobs.Scheduler over the registry's declarations scans for
		// expiry and regenerates CRLs at the tasks' own windows without
		// writing schedule points of its own. They are declared exactly
		// where their handlers are registered -- a composition that wires
		// no queue registers neither, so no declaration can outlive its
		// executor.
		if err := reg.SchedulesSeat().Add(m.service.expiryScanSchedule(), m.ca.crlRegenerateSchedule()); err != nil {
			return err
		}
	}

	m.handler = NewHandler(m.service, m.ca, reg.EventBus(), reg.AuditActionsSeat())
	reg.RoutesSeat().Mount(apiPath, m.handler)
	return nil
}
