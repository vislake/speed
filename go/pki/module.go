package pki

import (
	"embed"
	"time"

	"gorm.io/gorm"

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

// The configuration keys pki contributes: only what round 1's own code
// paths consult. The propagation window (round 2) and the CRL distribution
// point URL (round 3) belong to the rounds that implement what they
// configure.
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
)

// Default validity periods backing the config items above. None of this
// round's issuance methods (CreateRootCA, CreateIntermediateCA,
// IssueCertificate) reads these through the config schema yet -- Register
// only declares the schema, per pkgcore.Module.Register's own "must not
// perform I/O; it only declares" contract, and pki carries no config.Service
// dependency to read a live value with. A caller of this round's Go API
// passes NotAfter directly (see RootCAParams/IntermediateCAParams/
// CertificateParams); wiring these declared config keys into that decision
// is left to the host, or to a later round.
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

// NewModule returns a Module whose tables live in db, signing through
// LocalSigner unless overridden by WithSigner. Constructing a Module
// performs no I/O: opening and migrating db, and registering
// LocalKeySerializerName against a cipher (RegisterLocalKeySerializer), are
// the host's responsibility, done before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{
		db:         db,
		signerName: "local",
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

	m.service = NewService(m.signer, m.signerName, m.signingKeys)
	m.ca = NewCAService(m.signer, m.signerName, m.authorities, m.certificates)
	return m
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
// round-1 configuration schema. It does not mount any HTTP route (no
// OpenAPI fragment exists yet), does not declare any domain event (none of
// this round's code paths publish one -- pki.signing_key.staged and its
// siblings are round 2/3), and does not register any jobs.Queue handler
// (the expiry scan and CRL regeneration jobs are round 2/3 work; pki does
// not even depend on go/jobs yet).
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
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
