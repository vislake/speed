package aigateway

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/ai-gateway/migrations"
)

// moduleName is ai-gateway's the module contract's Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "ai-gateway"

// SystemPurposeCredentialWrite is the audited system purpose a host
// declares (pkgcore.WithSystemContext, or tenancy.WithSystemContext's own
// registration path) when it builds the system context that authorizes a
// platform-wide credential write (CredentialService.SetPlatformCredential).
// The purpose is descriptor data: the component descriptor (component.go)
// declares it as SystemPurposes, which the assembly registers at the Init
// stage's entry and the transition bridge registers inside the module's own
// registration turn, so a host that bootstraps this module never needs to
// register it by hand.
const SystemPurposeCredentialWrite pkgcore.SystemPurpose = "ai-gateway.credential_write"

// The permissions ai-gateway contributes to the platform's permission
// catalog. Enforcement belongs to rbac, which decides which role holds
// which of these; ai-gateway only declares that they exist and what they
// are called. There is only one gated entity (the credential), so these
// follow the plain "<module>:<action>" shape -- never integration's deeper
// "<module>:<entity>:<action>" one -- a shape
// rbac.RequirePermissionFunc's splitPermission (exactly one colon) parses
// directly.
//
// PermissionWrite and PermissionManagePlatform are DELIBERATELY DISTINCT:
// setting the caller's own tenant's BYOK credential
// (aiGateway_setTenantCredential) is an ordinary tenant-scoped write,
// while setting the platform-wide default every tenant without its own
// BYOK row falls back to (aiGateway_setPlatformCredential) is a
// materially more privileged operation -- it can be granted, in a real
// deployment's role design, to a much narrower set of principals than
// PermissionWrite ever is, and no code path in this module or its host
// wiring ever checks one in place of the other.
const (
	// PermissionRead covers reading which scope currently answers for a
	// provider (aiGateway_getCredential) -- never the api key itself.
	PermissionRead = "ai-gateway:read"
	// PermissionWrite covers setting the CALLER'S OWN tenant's BYOK
	// credential (aiGateway_setTenantCredential).
	PermissionWrite = "ai-gateway:write"
	// PermissionManagePlatform covers setting the PLATFORM-WIDE default
	// credential (aiGateway_setPlatformCredential) -- see this const
	// block's own doc comment for why it is never merged with
	// PermissionWrite.
	PermissionManagePlatform = "ai-gateway:manage_platform"
)

// apiPath is the common prefix ai-gateway's HTTP routes are mounted at
// (see Register below). It must agree with the "paths:" keys of this
// module's api/openapi.yaml fragment: api.HandlerFromMux registers the
// fragment's full method+path patterns on Handler's inner mux, and
// mounting at this prefix here only tells the host's outer mux which
// requests to hand to Handler at all -- exactly as storage's and org's
// identical constants do for their own fragments.
const apiPath = "/api/v1/ai-gateway"

// openAPISpecYAML is ai-gateway's OpenAPI fragment, embedded from api/ so
// the spec -- and the generated ServerInterface and types derived from it
// -- travels inside the module binary.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// Module implements the module contract for go/ai-gateway.
//
// Module declares no notification type, no domain event and no dynamic
// config item of its own: model routing is a construction-time Go option
// rather than a tenant-tunable config value (route.go's own doc comment).
// Register contributes the module's two credential-write permissions and
// one read permission, the HTTP surface they gate (handler.go), the
// image-generation job handler when the Gateway was built with
// WithImageGeneration, and the audited system purpose
// SystemPurposeCredentialWrite. The module's migrations carry the
// ai_gateway_credentials table, shared by chat and image credentials
// alike; its Gateway/CredentialService accessors are what a host wires
// directly into whatever business code needs AI chat or image generation.
//
// The zero value is not ready to use; construct one with NewModule.
type Module struct {
	credentials *CredentialService
	gateway     *Gateway

	// handler serves the module's HTTP surface: the three credential
	// operations the api/ fragment defines, implemented in handler.go
	// behind the generated api.ServerInterface, mounted by Register at
	// apiPath on the host's router. Built by Register, not NewModule,
	// because Register runs after every construction-time option, so the
	// handler always serves the same m.credentials instance those options
	// configured.
	handler *Handler
}

// NewModule returns a Module whose ai_gateway_credentials table lives in
// db, and whose Gateway is built with opts (WithModelRoute,
// WithEntitlements, WithUsageRecorder, and so on). Constructing a Module
// performs no I/O -- opening and migrating db is the host's responsibility,
// done before Register is ever called, exactly like every other module in
// this codebase.
func NewModule(db *gorm.DB, opts ...GatewayOption) *Module {
	credentials := NewCredentialService(db)
	return &Module{
		credentials: credentials,
		gateway:     NewGateway(credentials, opts...),
	}
}

// Gateway returns the module's Gateway -- the facade business code calls
// (gateway.Chat / gateway.ChatStream).
func (m *Module) Gateway() *Gateway { return m.gateway }

// Credentials returns the module's CredentialService, for a host or an
// admin surface that needs to write a platform or tenant BYOK credential
// (SetPlatformCredential / SetTenantCredential).
func (m *Module) Credentials() *CredentialService { return m.credentials }

// Name implements the module contract.
func (m *Module) Name() string { return moduleName }

// DependsOn implements the module contract. ai-gateway depends on nothing else
// in the module graph: its Entitlements and UsageRecorder seams are
// structurally-typed interfaces a host wires directly (see seams.go), never
// an import of go/billing or go/metering, so there is no module dependency
// to declare for either.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements the module contract: ai-gateway renders no user-facing
// content of its own (it returns structured error codes, like every
// backend module, never localized text), so it contributes an empty file
// set -- a module with no locale files contributes nothing to the merged
// catalog, which is not an error (go/pkgcore/i18n's AddModule doc
// comment).
func (m *Module) Locales() embed.FS { return embed.FS{} }

// OpenAPISpec implements the module contract: ai-gateway's own OpenAPI fragment,
// embedded from api/openapi.yaml. The fragment is the single source of
// this module's HTTP surface -- the api package's generated types and
// ServerInterface (api/ai-gateway-server.gen.go, regenerated by task
// api:gen) derive from it, and Handler implements that interface (see
// handler.go).
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements the module contract. Per the interface's own contract it
// performs no I/O. SystemPurposeCredentialWrite is not declared here: it is
// descriptor data, carried by the component descriptor (component.go) and
// registered by the assembly's Init close (or, on the host path, by the
// transition bridge inside the module's own registration turn), so a host's
// system context can name it. Register claims the image-generation job
// handler on reg.Jobs whenever the Module's Gateway was built with
// WithImageGeneration (image_gateway.go's imageJobHandler):
// reg.Jobs.Handle is itself a plain catalog insertion, no I/O, so
// Register's no-I/O contract stands either way. A Gateway built for
// chat-only use (no WithImageGeneration) registers no job handler at all.
//
// The module's two credential-write permissions and one read permission
// are declared on reg.Permissions, and the module's HTTP surface --
// Handler, built here so it always serves the m.credentials instance
// NewModule's options configured -- is mounted at apiPath.
// reg.Routes.Mount is a plain registration, no I/O, so Register's no-I/O
// contract stands.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.PermissionsSeat().Add(PermissionRead, PermissionWrite, PermissionManagePlatform); err != nil {
		return err
	}
	// Attaches the registry as Gateway's hostSeams so checkRateLimit
	// (ratelimit.go) can build a go/ratelimit.Limiter over the deployment
	// mode's resolved KVStore.
	m.gateway.host = reg
	// Attaches the registry as the Gateway's component face: a route's
	// provider name resolves as a selected member of the "chat"/"image"
	// directories through it from here on (gateway.go's buildChat/
	// buildImage), the same names the seam registrations carry.
	m.gateway.components = reg
	if handler, ok := m.gateway.imageJobHandler(); ok {
		if err := reg.JobsSeat().Handle(TaskTypeImageGenerate, handler); err != nil {
			return err
		}
	}
	// The handler is built here with the registry's resolved event bus
	// (reg.EventBus()) as well as m.credentials: the assembly resolves every
	// seam before Register runs, so Register is the earliest point at which
	// the platform-credential operation's audited system-context publish
	// (handler.go's tenancy.WithSystemContext call) has a bus to publish
	// on.
	m.handler = NewHandler(m.credentials, reg.EventBus())
	reg.RoutesSeat().Mount(apiPath, m.handler)
	return nil
}
