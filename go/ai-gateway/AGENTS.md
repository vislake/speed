# ai-gateway

Round 1 of `docs/internal/08-ai-gateway.md`'s unified-abstraction-layer
section: a chat-only LLM gateway. Image generation -- the design doc's
multi-modal-expansion section -- is explicitly out of scope for this round
and deferred to a later one -- there is no `ImageProvider`, no jobs-based
async image task, and no storage integration anywhere in this module yet.

## What this round ships

- `ChatProvider`: the vendor-agnostic `Chat`/`ChatStream` interface every
  integration implements (`types.go`). `ChatRequest.Model` is a caller-facing
  LOGICAL model key at the `Gateway` boundary and the resolved concrete
  vendor model id once a provider receives it -- see `ChatRequest`'s own doc
  comment for the exact translation point. `ChatChunk`'s doc comment pins
  the streaming channel contract: ordinary chunks, then exactly one terminal
  chunk (real `Usage` on success, `Err` on failure), then the channel
  closes -- no separate error channel, no error surfaced only through the
  function's own return.
- Model routing (`route.go`): `WithModelRoute(logicalKey, provider,
  vendorModel)` is a construction-time `GatewayOption`, not a dynamic
  `go/config` item -- routing is an infrastructure-composition decision the
  host assembler makes once, not a tenant-tunable runtime value in this
  round. An unrouted logical key is `ErrUnroutedModel`, never a silent
  fallback.
- Credential storage (`model.go`, `store.go`, `credential.go`): a single
  scope-tiered `ai_gateway_credentials` table modeled directly on
  `go/config`'s own `configs` table -- `CredentialScopeSystem` /
  `CredentialScopeTenant`, the same empty-string tenant_id sentinel
  convention, the same tenant-override-down-to-system-default resolution
  order. Platform data, not tenant data (see `model.go`'s doc comment):
  queried through a plain `*gorm.DB`, never `dbkit.Repository[T]`, tested
  with `tenancytest.AssertNotTenantScoped`. The API key column is encrypted
  at rest through `CredentialAPIKeySerializerName`, a host-registered
  `dbkit.RegisterEncryptedSerializer` call before `dbkit.Open` -- no blind
  index, since a credential is only ever looked up by (provider, scope,
  tenant), never by its own value.
- `OpenAICompatibleProvider` (`openai_compatible.go`): the default,
  zero-external-dependency `ChatProvider`, implemented directly against the
  OpenAI-compatible chat-completions wire schema (stdlib `net/http` +
  `encoding/json` only). Supports both the JSON response and
  Server-Sent-Events streaming (`data: {...}` lines, `data: [DONE]`
  terminator, `stream_options.include_usage` requested automatically so the
  final chunk carries real token usage -- the design doc's explicit rule
  that streaming handles real usage at the last chunk).
- `ChatProviderRegistry` (`registry.go`): a package-level
  `pkgcore.SeamRegistry[ChatProvider]`, mirroring `go/pki/signer_registry.go`'s
  precedent, with `OpenAICompatibleProvider` self-registered under
  `ProviderOpenAICompatible` ("chat.openai-compatible") from this package's
  own `init()`. Unlike pkgcore's four kernel seams, `Gateway` calls `Build`
  fresh on every request -- the resolved credential (base_url, api_key) is
  the `pkgcore.Config` -- since constructing an `OpenAICompatibleProvider`
  performs no I/O.
- `Gateway` (`gateway.go`): the facade -- `Chat`/`ChatStream` are the only
  entry points business code calls. Pipeline: validate the request -> check
  `Entitlements` (if wired) BEFORE any credential/provider resolution, so a
  refused caller is never billed -> resolve the credential -> resolve the
  route -> resolve the provider -> call it -> report usage to
  `UsageRecorder` (if wired) -- for `ChatStream`, only once the terminal
  success chunk (real `Usage`) is observed, never speculatively at stream
  start.
- `Entitlements` and `UsageRecorder` (`seams.go`): structurally-typed,
  no-import seams, the identical pattern `go/integration/seams.go`'s
  `PermissionLister` documents for the identical reason -- `ai-gateway` sits
  at the same dependency tier as `billing`, `sharing` and `integration`
  (root `CLAUDE.md`'s graph), so none of the three may import each other.
  Both interfaces mirror the real `billing.EntitlementsService.Check` and
  `metering.Recorder.Record` shapes without importing either package; a host
  with both wired satisfies them with a one-line assignment or a short
  closure (see each interface's own doc comment and the `*Func` adapters).
  Both seams are OPTIONAL: a `Gateway` built with neither wired still works,
  it just enforces no quota and reports no usage anywhere -- a real
  production deployment always wires both.
- `Module` (`module.go`): implements `pkgcore.Module`. This round declares
  nothing on the registry (`Register` is a no-op) -- no HTTP surface, no
  permission, no config item, no notification type, no domain event. What it
  contributes is the migrations (the `ai_gateway_credentials` table) and the
  `Gateway`/`CredentialService` accessors a host wires directly.

## Reference-app consumer

`examples/reference-app/internal/consult` is the mandatory first consumer:
a small, non-HTTP-generated Go service that, given a note's text, asks
`gateway.Chat()` for a short AI-generated consultation-suggestion summary
under the logical model key `"chat:default"`. It is mounted as a
hand-written route (`POST /api/v1/consult/suggest`), outside the OpenAPI
machinery -- the same pattern the demo module's own hand-written patient-
message route already establishes in this app, since `ai-gateway` itself
ships no HTTP surface for the reference app's notes-spec fragment to grow
into. `cmd/server/consult_flow_test.go` drives it through the composed HTTP
stack against an `httptest.Server` standing in for the OpenAI-compatible
endpoint -- scripted, deterministic responses, no live API key required or
used.

## Known limitations / deferred

- No dynamic (`go/config`-backed) model routing -- construction-time
  `WithModelRoute` options only, a deliberate round-1 choice (see
  `route.go`'s doc comment); a later round may add a config-driven layer on
  top without changing `ModelRoute`'s shape.
- No admin/HTTP surface for writing platform or tenant BYOK credentials --
  `CredentialService.SetPlatformCredential` / `SetTenantCredential` are Go
  APIs a host or a future admin-console round wires into its own surface.
- `UsageEvent.IdempotencyKey` is a fresh random value per call, not derived
  from any caller business-operation id (there is none to derive it from at
  this layer) -- a `UsageRecorder` wanting exactly-once billing-grade
  semantics cannot rely on it for deduplication, mirroring
  `go/metering`'s own `AnalyticsRecorder` fail-open, undeduped stance.
- No local/self-hosted inference provider (Ollama/vLLM) yet -- `ChatProvider`
  is deliberately unaware of whether an implementation is local or remote,
  so this is purely a future additional registration, not an interface
  change.
- No `ImageProvider`, no image generation, no `jobs`/`storage` integration --
  out of scope for this round by the task's own instruction; see
  `docs/internal/08-ai-gateway.md`'s multi-modal-expansion section for the
  target shape a later round implements.
