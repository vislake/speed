// Package aigateway provides a multi-vendor LLM chat gateway: a unified
// ChatProvider abstraction, logical-to-vendor model routing, platform/BYOK
// credential storage, and a Gateway facade that wires entitlement checks and
// usage metering around every call automatically.
//
// # Scope of this round
//
// This round implements only the "统一抽象层" (unified abstraction layer)
// section of docs/internal/08-ai-gateway.md: chat, not image generation.
// Image generation ("多模态扩展") -- ImageProvider, jobs-based async image
// tasks, storage integration -- is out of scope and deferred to a later
// round; nothing in this package's public API commits to a shape for it.
//
// # The pipeline
//
// Business code calls only Gateway.Chat or Gateway.ChatStream, never a
// ChatProvider directly. Internally, each call runs the design doc's fixed
// pipeline:
//
//  1. Resolve the logical model key (req.Model, e.g. "chat:default") to a
//     ModelRoute -- a (Provider, VendorModel) pair -- through the routes a
//     host declared at construction time (WithModelRoute). An unrouted key
//     is a coded error, never a silent fallback to some default provider.
//  2. If an Entitlements seam is wired, check it BEFORE the credential is
//     even resolved or the provider is called, so a refused caller is never
//     billed. This is a boolean gate, not a metered reservation -- there is
//     no refund path to run on failure.
//  3. Resolve the credential for the route's Provider: a tenant's own BYOK
//     row if one exists, the platform-wide row otherwise (CredentialService.Resolve).
//  4. Resolve the named ChatProvider from ChatProviderRegistry, built fresh
//     from the resolved credential -- never a vendor model id a caller
//     supplied.
//  5. Call the provider. For Chat, usage is reported once the call returns.
//     For ChatStream, usage is reported only once the LAST chunk (the one
//     carrying real Usage) is observed -- never speculatively at the start
//     of the stream (see ChatChunk's own doc comment for the exact channel
//     contract).
//  6. If a UsageRecorder seam is wired, report the real usage automatically.
//     Business code never calls a metering API of its own for a chat call
//     that went through this Gateway.
//
// Every step logs through obs.FromContext(ctx) with structured attributes
// (model, provider) -- tenant correlation comes from the logger's own
// context extraction, never from a tenant_id metric label.
//
// # Module boundaries: no billing or metering import
//
// ai-gateway sits at the same dependency tier as billing, sharing and
// integration (root CLAUDE.md's graph) -- none of the three may import each
// other. Entitlements and UsageRecorder are therefore declared here as
// small, structurally-typed interfaces this package owns, the same
// no-import seam pattern go/integration/seams.go uses for rbac: a host that
// has go/billing and go/metering wired satisfies both interfaces with a
// one-line adapter over the real services (see Entitlements and
// UsageRecorder's own doc comments); a host with neither wires nothing and
// the Gateway still works, just with no quota enforcement and no automatic
// metering. Shipping without Entitlements wired means this gateway enforces
// NO quota at all -- a real production deployment always wires it.
//
// # Credentials: platform key vs. tenant BYOK
//
// Credentials are stored in a single scope-tiered table modeled directly on
// go/config's own configs table: a "system" row is the platform-wide
// default, a "tenant" row is a tenant's own bring-your-own-key override, and
// resolution falls tenant-override-down-to-system-default exactly as
// config.Service.Get does (see credential.go). The table is platform data,
// not tenant data -- it deliberately does not implement dbkit.TenantScoped,
// for the identical reason config's own row type does not (see model.go's
// doc comment) -- so it is queried through a plain *gorm.DB, never a
// dbkit.Repository[T]. The API key column is encrypted at rest with dbkit's
// field-level AES-256-GCM serializer under a host-injected master key
// (CredentialAPIKeySerializerName); no blind index is needed, because a
// credential is always looked up by (provider, scope, tenant), never by its
// own value.
//
// # The default provider: OpenAI-compatible chat completions
//
// OpenAICompatibleProvider implements the chat-completions wire schema
// shared by OpenAI itself and every OpenAI-compatible host (Azure OpenAI,
// DeepSeek, many self-hosted gateways) directly against stdlib net/http and
// encoding/json -- no vendor SDK, zero third-party dependencies for this
// module's default path, the same posture go/pki's LocalSigner and
// pkgcore's in-process EventBus keep for their own defaults. It registers
// itself into the package-level ChatProviderRegistry under the name
// ProviderOpenAICompatible from this package's own init(), mirroring
// go/pki/signer_registry.go's SeamRegistry precedent; a future vendor-SDK
// provider (a hypothetical go/ai-gateway/provider/anthropic subpackage)
// would self-register the same way, without touching this round's code.
package aigateway
