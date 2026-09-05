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

// Module implements pkgcore.Module for go/ai-gateway.
//
// Unlike most modules in this codebase, Module.Register declares almost
// nothing: this module ships no HTTP surface (the design doc frames it
// entirely as an in-process library business code calls directly), no
// permission (nothing to gate over HTTP), no notification type, no domain
// event, and -- per route.go's own doc comment -- no dynamic config item
// either, since model routing is a construction-time Go option rather than
// a tenant-tunable config value. The one thing Register does declare,
// added in round 2, is the image-generation job handler on reg.Jobs, and
// only when the Module's Gateway was built with WithImageGeneration (see
// image_gateway.go) -- a chat-only Gateway registers no job handler at
// all. What Module otherwise contributes is its migrations (the
// ai_gateway_credentials table, shared by chat and image credentials
// alike) and its Gateway/CredentialService accessors, which a host wires
// directly into whatever business code needs AI chat or image generation.
//
// The zero value is not ready to use; construct one with NewModule.
type Module struct {
	credentials *CredentialService
	gateway     *Gateway
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

// OpenAPISpec implements pkgcore.Module: nil. This round ships no HTTP
// surface -- see this type's own doc comment.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// performs no I/O; it declares SystemPurposeCredentialWrite
// (pkgcore.RegisterSystemPurpose is a pure in-memory registration, not I/O)
// so a host's system context can name it, and -- new this round -- claims
// the image-generation job handler on reg.Jobs whenever the Module's
// Gateway was built with WithImageGeneration (image_gateway.go's
// imageJobHandler): reg.Jobs.Handle is itself a plain catalog insertion, no
// I/O, so Register's no-I/O contract stands either way. A Gateway built for
// chat-only use (no WithImageGeneration) registers no job handler at all --
// nothing else changes from round 1; see this type's own doc comment for
// what else remains deliberately absent.
func (m *Module) Register(reg *pkgcore.Registry) error {
	pkgcore.RegisterSystemPurpose(SystemPurposeCredentialWrite)
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
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
