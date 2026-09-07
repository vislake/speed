package aigateway

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/ai-gateway/migrations"
)

// moduleName is ai-gateway's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "ai-gateway"

// SystemPurposeCredentialWrite is the audited system purpose a host
// declares (pkgcore.WithSystemContext, or tenancy.WithSystemContext's own
// registration path) when it builds the system context that authorizes a
// platform-wide credential write (CredentialService.SetPlatformCredential).
// Register calls pkgcore.RegisterSystemPurpose with it, so a host that
// bootstraps this module never needs to register it by hand -- mirroring
// go/config's identical SystemPurposeSystemWrite constant and registration
// call.
const SystemPurposeCredentialWrite pkgcore.SystemPurpose = "ai-gateway.credential_write"

// The permissions ai-gateway contributes to the platform's permission
// catalog. Enforcement belongs to rbac, which decides which role holds
// which of these; ai-gateway only declares that they exist and what they
// are called. There is only one gated entity (the credential), so these
// follow the plain "<module>:<action>" shape storage's and notes' own
// permissions do, never integration's deeper "<module>:<entity>:<action>"
// one -- a shape rbac.RequirePermissionFunc's splitPermission (exactly one
// colon) parses directly, unlike integration's.
//
// PermissionWrite and PermissionManagePlatform are DELIBERATELY DISTINCT,
// per this round's own brief: setting the caller's own tenant's BYOK
// credential (aiGateway_setTenantCredential) is an ordinary tenant-scoped
// write, while setting the platform-wide default every tenant without its
// own BYOK row falls back to (aiGateway_setPlatformCredential) is a
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

// Module implements pkgcore.Module for go/ai-gateway.
//
// Round 1 and round 2 declared almost nothing on the registry: no HTTP
// surface (the design doc framed the module entirely as an in-process
// library business code calls directly), no permission, no notification
// type, no domain event, and -- per route.go's own doc comment -- no
// dynamic config item either, since model routing is a construction-time
// Go option rather than a tenant-tunable config value. This round (round
// 3) adds the module's first HTTP surface -- the credential-write admin
// surface go/ai-gateway/AGENTS.md's Known limitations section named as a
// gap -- and with it two permissions and a mounted route, but changes
// nothing else: model routing is still construction-time only, and the
// image-generation job handler claim from round 2 is unchanged. What
// Module otherwise contributes is its migrations (the
// ai_gateway_credentials table, shared by chat and image credentials
// alike) and its Gateway/CredentialService accessors, which a host wires
// directly into whatever business code needs AI chat or image generation.
//
// The zero value is not ready to use; construct one with NewModule.
type Module struct {
	credentials *CredentialService
	gateway     *Gateway

	// handler serves the module's HTTP surface: the three credential
	// operations the api/ fragment defines, implemented in handler.go
	// behind the generated api.ServerInterface, mounted by Register at
	// apiPath on the host's router. Built by Register, not NewModule, for
	// the same reason storage's identical field is: Register runs after
	// every construction-time option, so the handler always serves the
	// same m.credentials instance those options configured.
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

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. ai-gateway depends on nothing else
// in the module graph: its Entitlements and UsageRecorder seams are
// structurally-typed interfaces a host wires directly (see seams.go), never
// an import of go/billing or go/metering, so there is no module dependency
// to declare for either.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: ai-gateway renders no user-facing
// content of its own this round (it returns structured error codes, like
// every backend module, never localized text), so it contributes an empty
// file set -- a module with no locale files contributes nothing to the
// merged catalog, which is not an error (go/pkgcore/i18n's AddModule doc
// comment).
func (m *Module) Locales() embed.FS { return embed.FS{} }

// OpenAPISpec implements pkgcore.Module: ai-gateway's own OpenAPI fragment,
// embedded from api/openapi.yaml. The fragment is the single source of
// this module's HTTP surface -- the api package's generated types and
// ServerInterface (api/ai-gateway-server.gen.go, regenerated by task
// api:gen) derive from it, and Handler implements that interface (see
// handler.go) -- per docs/internal/21-api-contract.md's spec-first
// decision. New this round: rounds 1-2 returned nil here, since this
// module shipped no HTTP surface at all until now.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own contract it
// performs no I/O; it declares SystemPurposeCredentialWrite
// (pkgcore.RegisterSystemPurpose is a pure in-memory registration, not I/O)
// so a host's system context can name it, claims the image-generation job
// handler on reg.Jobs whenever the Module's Gateway was built with
// WithImageGeneration (image_gateway.go's imageJobHandler, round 2's
// addition, unchanged this round): reg.Jobs.Handle is itself a plain
// catalog insertion, no I/O, so Register's no-I/O contract stands either
// way. A Gateway built for chat-only use (no WithImageGeneration)
// registers no job handler at all.
//
// New this round: the module's two credential-write permissions and one
// read permission are declared on reg.Permissions, and the module's HTTP
// surface -- Handler, built here so it always serves the m.credentials
// instance NewModule's options configured, exactly mirroring storage's own
// Register doc comment's reasoning -- is mounted at apiPath.
// reg.Routes.Mount is a plain registration, no I/O, so Register's no-I/O
// contract stands.
func (m *Module) Register(reg *pkgcore.Registry) error {
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)
	if err := reg.Permissions.Add(PermissionRead, PermissionWrite, PermissionManagePlatform); err != nil {
		return err
	}
	// Attaches the registry as Gateway's hostSeams so checkRateLimit
	// (ratelimit.go) can build a go/ratelimit.Limiter over the deployment
	// mode's resolved KVStore -- mirroring go/sharing's identical
	// s.host = reg wiring in its own Module.Register.
	m.gateway.host = reg
	if handler, ok := m.gateway.imageJobHandler(); ok {
		if err := reg.Jobs.Handle(TaskTypeImageGenerate, handler); err != nil {
			return err
		}
	}
	// The handler is built here with the registry's resolved event bus
	// (reg.EventBus()) as well as m.credentials: Bootstrap resolves every
	// seam before Register runs, so Register is the earliest point at which
	// the platform-credential operation's audited system-context publish
	// (handler.go's tenancy.WithSystemContext call) has a bus to publish
	// on.
	m.handler = NewHandler(m.credentials, reg.EventBus())
	reg.Routes.Mount(apiPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
