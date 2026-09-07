# ai-gateway

Round 1 shipped `docs/internal/08-ai-gateway.md`'s unified-abstraction-layer
section: a chat-only LLM gateway. Round 2 shipped the design doc's
multi-modal-expansion section: `ImageProvider`, the async-only
`Gateway.GenerateImage` pipeline, and the go/storage + go/jobs integration
round 1 deliberately left unbuilt. Round 3 shipped the HTTP surface for
reading and writing platform and tenant BYOK credentials -- the module's
own OpenAPI fragment, its generator wiring, its handler, its reference-app
consumer and its tests -- closing the "no admin/HTTP surface" limitation
rounds 1 and 2 recorded below. Round 4 (this round) ships the SSRF guard
for the tenant-writable BYOK base URL (the reviewer-ringed P0 this module
carried) plus the explicit tenantless-call handling at Gateway's three
rate-limit call sites (the adjudicated P1). See "What round 4 adds".

## What round 1 shipped

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
  it just enforces no quota and reports no usage anywhere. The round-1
  "a real production deployment always wires both" gloss is corrected by
  this module's own mandatory first consumer: the reference app's smilesim
  service charges per generation directly through `go/billing`'s
  reserve/confirm lifecycle without ever wiring `WithUsageRecorder` -- the
  fork and its decision are recorded in this file's "Review-fix round 2"
  section below, and the seam's own role stays analytics-grade usage
  reporting, never charging.
- `Module` (`module.go`): implements `pkgcore.Module`. As of round 1,
  `Register` declared nothing on the registry -- no HTTP surface, no
  permission, no config item, no notification type, no domain event; what
  the module contributed was the migrations (the `ai_gateway_credentials`
  table) and the `Gateway`/`CredentialService` accessors a host wires
  directly. That is no longer the full picture: round 2 added a conditional
  job-handler claim and round 3 declared the permission vocabulary, the
  system purpose and the HTTP mount described below.

## What round 2 adds

- `ImageProvider` (`image_types.go`): the vendor-agnostic image abstraction,
  the multi-modal counterpart of `ChatProvider` -- three methods,
  `TextToImage`/`ImageToImage`/`Inpaint`, one per operation the design doc
  names, each taking a request struct built from `Model` (resolved vendor
  id) + `Prompt` + `Params map[string]any` (the identical vendor-passthrough
  convention `ChatRequest.Params` established) plus `ImageBytes` (raw
  content + MIME) for `Input`/`Mask` where the operation needs one.
  **`ImageProvider` itself trades in raw bytes, never a storage object
  reference** -- see the next bullet for why, and `image_gateway.go`'s own
  "object-reference / raw-bytes boundary" doc comment for the full
  reasoning this file only summarizes.
- **The object-reference boundary is drawn at the Gateway/job-handler
  layer, not inside `ImageProvider`.** The design doc states images travel
  through storage uniformly and the interface passes object references,
  never byte streams. `ImageRequest`
  (`Gateway.GenerateImage`'s own input) and `ImageJobResult` (what a caller
  polls back) satisfy that literally: `InputObjectID`/`MaskObjectID`/
  `OutputObjectID` are the only image-shaped fields either carries, never a
  byte. `ImageProvider`'s own three methods, deliberately, do not carry
  object references: `pkgcore.SeamRegistry[ImageProvider].Build`'s `Config`
  is a flat `map[string]string` (the same shape `ChatProviderRegistry.Build`
  already uses for `base_url`/`api_key`), which cannot carry a live
  go/storage handle to a provider resolved fresh per job -- and forcing
  every future third-party `ImageProvider` implementation to embed its own
  storage plumbing would needlessly couple simple vendor integrations (and
  their tests) to go/storage, the opposite of `OpenAICompatibleImageProvider`
  staying as easily `httptest`-testable as its chat sibling. The job handler
  is the SINGLE place storage I/O happens, translating a reference to bytes
  before calling the provider and bytes back to a reference afterward. This
  is a considered interpretation of the task's brief where its own wording
  admitted two readings (item 1's "`ImageProvider` ... take[s] and return[s]
  storage object references" against item 3's "the job handler ... reads
  the input object(s) from storage ... writes the output image bytes back
  into storage"); it is recorded here explicitly rather than silently
  resolved, per this codebase's own discipline that a genuine design choice
  gets written down, not just coded.
- Async-only pipeline (`image_gateway.go`): `Gateway.GenerateImage(ctx,
  ImageRequest) (jobs.JobID, error)` has NO synchronous counterpart at
  all -- stricter than `Chat`, which is synchronous by default with
  `ChatStream` as its only async-shaped variant, per the design doc's
  explicit rule that every image task is asynchronous by default, running
  through jobs. It validates the
  request, checks `Entitlements` (reused verbatim, never a second gate)
  BEFORE resolving anything, resolves the route/credential once to fail
  fast on an obviously broken configuration (the built provider is
  discarded -- see the next point), and enqueues one `TaskTypeImageGenerate`
  job (`ai-gateway.image.generate`), returning its `JobID` immediately.
  Every real provider call, storage read/write and usage report happens
  inside the job handler, which reruns route/credential resolution fresh --
  a job may execute long after, and on a different replica than, the
  enqueuing call, and a resolved credential can legitimately have changed
  in between, mirroring how `Gateway.Chat` also resolves a provider fresh
  on every call rather than caching one.
- **A caller retrieves a completed image job's result through go/jobs' own,
  already-shipped mechanism -- there is no second job-status system here.**
  `jobs.Queue.Get(ctx, jobID)` returns the `*jobs.Job`; once `Status` is
  `jobs.StatusSucceeded`, `json.Unmarshal(job.Result.Data, &ImageJobResult{})`
  yields `OutputObjectID` (the generated image's go/storage object id, a
  brand new object under the request's own tenant -- the handler never
  overwrites `InputObjectID`/`MaskObjectID`) and `Usage` (the real vendor
  billing dimensions, below). This is the same `Queue.Get` polling shape
  go/storage's own consumers already use for the thumbnail-derive task; ai-
  gateway builds nothing new on top of it.
- Usage/metering by real vendor billing dimension: `UsageEvent`/
  `UsageRecorder` (`seams.go`, round 1) needed **no shape change at all** --
  `Feature`/`Quantity`/`Metadata` were already generic, not chat-token-
  specific in any way that would not generalize. The job handler reports
  `"ai.image_count"` and `"ai.image_steps"` (new package-level Feature
  constants, `image_gateway.go`) as separate `UsageEvent`s when the vendor's
  own `ImageUsage` reports a positive count for each, folding the
  categorical resolution tier into `Metadata["resolution_tier"]` rather than
  inventing a third numeric Feature for a value that is not a quantity --
  `UsageEvent.Metadata`'s own doc comment already names it for exactly this
  kind of small, bounded call context. `"ai.chat_tokens"` is never reported
  for an image call, and vice versa: `image_gateway_test.go`'s own
  `TestImageGenerateHandler_TextToImage_WritesNewObjectAndReportsUsage`
  pins that the two dimensions never cross-contaminate.
- `ImageProviderRegistry` (`image_registry.go`): a package-level
  `pkgcore.SeamRegistry[ImageProvider]`, mirroring `ChatProviderRegistry`'s
  precedent exactly (same `Build`-fresh-per-call reasoning, same
  `pkgcore.Config{"base_url", "api_key"}` shape), with
  `OpenAICompatibleImageProvider` self-registered under
  `ProviderOpenAICompatibleImage` ("image.openai-compatible") from this
  package's own `init()`.
- `OpenAICompatibleImageProvider` (`openai_compatible_image.go`): the
  default, zero-vendor-SDK `ImageProvider`, implemented directly against
  the OpenAI-compatible images wire schema with stdlib `net/http` +
  `encoding/json` + `mime/multipart` only. `TextToImage` posts JSON to
  `/images/generations`; `ImageToImage` and `Inpaint` both post
  `multipart/form-data` to `/images/edits` (an `image` file part, an
  optional `mask` file part present only for `Inpaint`, `model`/`prompt`
  form fields, and every `Params` entry as an additional field -- a string
  value verbatim, anything else JSON-encoded). Every request asks for
  exactly the encoding it accepts: `response_format` is set to `b64_json`
  explicitly on the generation JSON body and the edits multipart alike,
  overriding any same-named `Params` entry (some vendors default
  `response_format` to `url`, and a url answer would require a second,
  unauthenticated fetch this provider never performs). Every response is
  decoded from `{"data": [{"b64_json": "..."}], "usage": {...}}`; the
  generated bytes' MIME is detected from the decoded bytes themselves via
  `http.DetectContentType`, never trusted from a vendor-supplied field --
  the same "probe, never trust a header" discipline go/storage's own
  revalidation pipeline applies. A data entry that decodes to zero bytes --
  an empty `b64_json` value, or the url-shaped answer of a vendor that
  ignored the requested `response_format` -- is refused with the coded
  `ErrProviderResponseInvalid`, never delivered as a zero-byte successful
  image with `ImageCount` 1. A response with no `usage` object falls
  back to the real delivered image count (`len(data)`) with zero steps and
  an empty resolution tier, rather than fabricating vendor billing data
  that was never reported.
- Credential and model routing reuse, unchanged in shape: image credentials
  live in the SAME `ai_gateway_credentials` table as chat credentials --
  `CredentialService.Resolve`/`SetPlatformCredential`/`SetTenantCredential`
  take a `provider` string that is just a registry name, and
  `"image.openai-compatible"` is exactly as valid a row key as
  `"chat.openai-compatible"`, so this needed no schema change. Model
  routing is the identical story: `WithModelRoute` and `Gateway.routes` are
  ONE shared `map[string]ModelRoute` namespace for both chat and image
  logical keys (a host picks non-colliding prefixes, e.g. `"chat:default"`
  vs `"image:default"`), resolved through `ChatProviderRegistry` or
  `ImageProviderRegistry` depending on which pipeline is asking.
  `route.go` and `model.go`/`store.go`/`credential.go` are therefore
  UNCHANGED files this round -- reuse, not extension.
- `Module` (`module.go`): `Register` gains exactly one new, conditional
  declaration -- claiming the image-generation job handler on `reg.Jobs`
  whenever the Module's `Gateway` was built with the new
  `WithImageGeneration(queue, objects)` `GatewayOption` (both go/storage's
  `*storage.ObjectService` and go/jobs' `jobs.Queue`, injected directly:
  both sit below ai-gateway in root `CLAUDE.md`'s dependency graph, so this
  is an ordinary downward dependency, unlike `Entitlements`/`UsageRecorder`,
  which stay structurally-typed no-import seams because ai-gateway sits at
  billing/metering's own tier). A `Gateway` built for chat-only use (no
  `WithImageGeneration`) registers no job handler at all, and
  `Gateway.GenerateImage` on such a Gateway always fails with
  `ErrImageGenerationUnavailable`.
- Runnable documentation (`example_test.go`): `Example_generateImage` walks
  the whole async pipeline this section describes -- a real
  `jobs.StandaloneQueue`, a real `storage.ObjectService` (both required by
  `WithImageGeneration`), `Gateway.GenerateImage`, draining
  `reg.Jobs.Handlers()` onto the queue exactly as `examples/reference-app`'s
  own `cmd/server/server.go` does, and polling the enqueued job to
  completion against a fake OpenAI-compatible images endpoint -- alongside
  round 1's chat-only `Example`, per root `CLAUDE.md`'s rule that a new
  public API ships with a compilable godoc `Example` in the same pull
  request.

## What round 3 adds

- The credential-write HTTP surface, the module's first
  (`api/openapi.yaml`, the seventh backend fragment after notes, org,
  authn, storage, notification and sharing): three operations under
  `/api/v1/ai-gateway` -- `GET /credentials/{provider}`,
  `PUT /credentials/{provider}/tenant`,
  `PUT /credentials/{provider}/platform` -- serving chat and image
  providers alike, since provider is just the registry name the
  credentials table's own row keys already use. Regenerated by the same
  pinned oapi-codegen v2.8.0 into a committed `api/ai-gateway-server.gen.go`
  (added to task `api:gen`'s legs and api-contract.yml's regen-and-diff
  pairs) and implemented by `Handler` (`handler.go`) behind the generated
  `api.ServerInterface` with a `var _ api.ServerInterface` compile-time
  assertion at the bottom of the file -- a spec change with no matching
  handler change cannot compile, per the spec-first flow
  (docs/internal/21-api-contract.md).
- **The read path is deliberately metadata-only.** All three operations
  answer with provider/scope/baseUrl and never the api key -- the surface
  is write-only by design, the rule this module's root-`CLAUDE.md` Security
  obligations (no secrets in API responses) and the spec fragment's own
  header both state. GET is the round's answer to "which scope currently
  answers for a provider?": it reports exactly what
  `CredentialService.Resolve` resolves under the caller's own request
  context -- the tenant's own BYOK row when one exists, the platform-wide
  row otherwise -- with an unknown provider answering 404 and the tenant
  read carrying no scope suffix at all (the resolution IS the read).
- **Two deliberately distinct write permissions, never one shared gate**
  (`module.go`'s doc comment on the pair): `PermissionWrite`
  ("ai-gateway:write") gates the tenant-scoped write (PUT .../tenant);
  `PermissionManagePlatform` ("ai-gateway:manage_platform") gates the
  platform-wide write (PUT .../platform), which is materially more
  privileged -- it rewrites the fallback every tenant WITHOUT its own BYOK
  row resolves to. `PermissionRead` ("ai-gateway:read") gates GET/HEAD.
  The Handler itself performs no permission check, exactly like every other
  module handler in this codebase: the host's router-level rbac gate
  decides. The reference app's `aiGatewayPermissionFor` (`demo_subject.go`)
  is the first concrete instance of that gate: GET/HEAD read, a
  `/platform`-suffixed path manage_platform, any other write.
- Handler translation only, no new business decision (`handler.go`'s own
  doc comment): every operation is answered entirely by the EXISTING
  `CredentialService` methods (`Resolve` / `SetTenantCredential` /
  `SetPlatformCredential`) this module's earlier rounds shipped, with the
  tenant always read from the request context, never from a request
  parameter, header or body (there is no tenant_id anywhere on this
  surface). The one translation layer: PUT .../platform builds the audited
  system context `SetPlatformCredential`'s own contract requires --
  `tenancy.WithSystemContext` (the audited wrapper, which publishes an
  `EventSystemContextEntered` audit event on the bus `NewHandler` was
  given, and fails the write closed if that publish fails) under
  `SystemPurposeCredentialWrite` ("ai-gateway.credential_write"),
  attributed to the fixed actor "ai-gateway-http", mirroring the
  fixed-actor reason the reference app's own boot-time platform writes
  already use: the audit value that matters is WHICH PATH performed the
  write, not a per-request caller identity a system context cannot carry
  here (pkgcore.SystemReason.Actor's own doc comment). This module sits
  well above tenancy in the dependency graph, so using the raw
  `pkgcore.WithSystemContext` here -- as an earlier round did -- would take
  the escape hatch on a live request path with zero audit events, the
  exact violation tenancy/AGENTS.md's System-context rule names; the
  audited wrapper is mandatory for this path.
- `Module.Register` grows accordingly: rounds 1's no-op (plus round 2's
  conditional image job claim) becomes a `Register` that also declares the
  three permissions on `reg.Permissions`, registers
  `SystemPurposeCredentialWrite` via `pkgcore.RegisterSystemPurpose`, and
  mounts the Handler at `apiPath` -- the module's single
  `Register(reg *Registry)` contract unchanged.
- Tests: `handler_test.go` pins the request/response translation in
  isolation -- an unknown provider's read is 404, an empty key and a
  malformed body are refused, a tenant write with no tenant in the context
  is refused, an omitted baseUrl is omitted from the response, and the
  apiKey is never echoed; `handler_example_test.go`'s `ExampleHandler`
  compiles AND runs a composed walk of the whole surface -- 404 with no
  credential, platform write, tenant BYOK override, and `Resolve`
  answering the tenant's row -- per root `CLAUDE.md`'s rule that a new
  public API ships a compilable godoc `Example` in the same pull request;
  and the reference app's `ai_gateway_flow_test.go` is the mandatory first
  consumer's end-to-end proof (next section).

## What round 4 adds

- **SSRF defense for the tenant BYOK base URL** (`ssrf.go`, `errors.go`,
  `credential.go`, `gateway.go`, `image_gateway.go`): a tenant admin
  writes the base URL of an OpenAI-compatible endpoint that the
  platform's own network then dials presenting the credential's API key --
  the exact outbound-dial primitive root `CLAUDE.md`'s Security rules
  protect for webhooks ("SSRF protection is mandatory, including
  DNS-rebinding protection"), which this module carried with zero
  validation. Two checks, deliberately mirroring `go/integration/ssrf.go`'s
  shape and tests case for case (this module sits on integration's own
  tier, so the small stdlib-only helpers are duplicated, not shared):
  creation-time refusal through `ValidateBaseURL` (parse, http/https
  scheme whitelist, literal-IP or real-DNS per-address check over the
  same blocked ranges integration refuses -- loopback, private,
  link-local, CGNAT, NAT64/IPv4-compatible/site-local IPv6, and the rest
  of the stdlib classification), and a dial-time re-check that PINS the
  address actually connected: a tenant-tier credential's provider is
  swapped onto `guardedProviderHTTPClient`, whose transport resolves the
  host once, refuses any blocked candidate, and dials the validated IP by
  address -- never a second re-resolution a rebinding DNS answer could
  steer. A hostname that passed validation when stored but resolves to an
  internal address by call time fails the call closed.
- **The scope boundary, stated in code** (`ssrf.go`'s file header,
  `credential.go`'s both setter doc comments): only the TENANT-tier write
  is validated (and only the tenant-tier dial is guarded), because only
  that tier is tenant-influenceable. The platform-wide row is written by
  the operator under an audited system context, and an intranet
  OpenAI-compatible LLM gateway is a legitimate platform default -- the
  reference app's boot-time platform credential and its loopback test
  harness live on that trusted side of the boundary, unchanged.
- **The no-IP-echo rule, inherited from integration's own narrowing**
  (96697dc): a blocked refusal reached through DNS resolution never
  carries the resolved address in its params (that would make the refusal
  an internal-DNS reconnaissance oracle -- submit hostnames, read back
  internal IPs); a refusal of a literal IP the caller typed still echoes
  it. The asymmetry is pinned by tests on both sides, and
  `ErrBaseURLBlocked`'s own doc comment demands the two modules' refusals
  keep agreeing.
- Three new coded errors, Invalid-classified like integration's:
  `aigateway.base_url_invalid`, `aigateway.base_url_unresolvable`,
  `aigateway.base_url_blocked`. An empty baseUrl stays legal to store
  ("no base URL configured").
- **Tenantless call sites never feed the rate limiter the empty string**
  (`gateway.go`, `image_gateway.go`, `ratelimit.go`): Chat and ChatStream
  gate their per-tenant limiter check on `pkgcore.TenantFromContext`'s
  ok -- a tenantless (system-context) call, which the module's own docs
  bless, skips the check with the reason stated at the call site rather
  than sharing an empty-string bucket with every other tenantless caller;
  GenerateImage hoists its existing tenant requirement
  (`ErrImageRequiresTenant`) into the limiter's own pipeline position, so
  the limiter is only ever reached with a real tenant dimension.
- **The response-reflux half of the same posture** (`openai_compatible.go`'s
  `errorFromResponse`, shared by the chat and image providers): the
  dialed endpoint's non-2xx response body used to be read verbatim into
  the returned error's `body` param -- the echo family DESIGN point 4
  (refusal answers must not carry what the server learned from the dial)
  has a body twin, and unlike the address rule it survives the SSRF
  guards for ALLOWED destinations, since a non-2xx answer is the common
  error path of every provider call. The params now carry the status
  code only. A later ring of the same audit narrowed the server-side log
  to match: the raw body (however bounded) is no longer logged there
  either, because observability's redaction layer masks credential
  shapes, never arbitrary echoed content -- and content-moderation-class
  refusals routinely echo the refused request input into the error
  envelope's free-text message. What reaches the log instead is the
  vendor's own error contract: `error.type` / `error.code`, the
  envelope's two enumeration fields, parsed from the JSON body (read at
  most `maxErrorBodyBytes`) into the structured attributes `error_type` /
  `error_code`; a body that is not the JSON envelope contributes no
  attributes and the line carries the status code alone. What operators
  lose is the vendor's prose; what they keep is the status and the two
  coded fields vendors triage by.
- Product decision, recorded not implemented: the stronger convergence --
  a platform-declared whitelist of base URLs only -- stays future product
  work; the legitimate capability (a tenant pointing its BYOK credential
  at any PUBLIC OpenAI-compatible vendor) is deliberately preserved.
- Tests: `ssrf_test.go` mirrors integration's suite (blocked literals,
  the no-echo/echo asymmetry, public-IP allowed, scheme/malformed/
  no-host/unresolvable refusals, the dial-time refusal of loopback and
  the non-refusal of public addresses) plus resolve/resolveImage wiring
  proofs that a tenant-tier credential's provider carries the guarded
  client and a platform-tier one does not; `credential_test.go`,
  `handler_test.go` and the reference app's `ai_gateway_flow_test.go`
  pin the refusals through the service, the HTTP envelope and the
  composed stack; `ratelimit_test.go` pins that tenantless calls never
  consult the limiter; `openai_compatible_test.go` and
  `openai_compatible_image_test.go` pin that a non-2xx answer's raw body
  reaches neither the returned error's params nor the server-side log --
  the envelope's `error_type` / `error_code` attributes are what the log
  carries, and a non-JSON body contributes nothing to it.

## Reference-app consumer

`examples/reference-app/internal/consult` remains round 1's mandatory first
consumer: a small, non-HTTP-generated Go service that, given a note's text,
asks `gateway.Chat()` for a short AI-generated consultation-suggestion
summary under the logical model key `"chat:default"`. It is mounted as a
hand-written route (`POST /api/v1/consult/suggest`), outside the OpenAPI
machinery -- the same pattern the demo module's own hand-written patient-
message route already establishes in this app. `cmd/server/consult_flow_test.go`
drives it through the composed HTTP stack against an `httptest.Server`
standing in for the OpenAI-compatible endpoint -- scripted, deterministic
responses, no live API key required or used.

`examples/reference-app/internal/smilesim` is round 2's mandatory first
consumer of `Gateway.GenerateImage`: given a patient photo the caller
already uploaded and completed through go/storage's own HTTP surface, it
asks the gateway for an async before/after AI smile simulation -- an
`ImageOperationImageToImage` request under the logical
model key `"image:smile-simulation"` -- exactly the use case root
`CLAUDE.md`'s own premise for this reference app names. Two hand-written
routes (`cmd/server/smilesim.go`, outside the OpenAPI machinery for the
identical reason consult.go's route is): `POST
/api/v1/smile-simulation/simulate` enqueues the job and answers 202 with
its id, and `GET /api/v1/smile-simulation/jobs/{id}` polls the app's own
`jobs.StandaloneQueue` (the same pool storage's thumbnail-derive task and
notification's delivery task already share) and, once succeeded, decodes
`Job.Result.Data` into `ImageJobResult` to answer with the generated
image's object id and its real usage dimensions.
`cmd/server/smilesim_flow_test.go` drives the whole chain through the
composed HTTP stack -- storage's real upload lifecycle for the input photo,
the async job, and the poll to completion -- against an `httptest.Server`
scripting the OpenAI-compatible images-edits endpoint, asserting the
genuine `multipart/form-data` request (the uploaded photo's own
storage-sanitized bytes, the routed vendor model id, no stray `mask` part
for an image-to-image request) and that the generated output lands as a
real, separate, completed go/storage object.

The reference app is also round 3's mandatory first consumer of the
credential-write surface itself. Its demo boot (`cmd/server/server.go`)
writes the chat and image platform defaults from `SPEED_AIGATEWAY_*`
configuration exactly as before -- those writes now sit side by side with
the module's own HTTP routes, mounted behind `demo_subject.go`'s real rbac
gate: `aiGatewayPermissionFor` selects among the round's three
permissions per request (see "What round 3 adds"), and the seeded demo
role `demo-aigateway-tenant-writer` (granted aigateway:read +
aigateway:write in every demo tenant, deliberately NOT
aigateway:manage_platform) proves on the composed stack that a
tenant-scoped principal really is refused the platform-wide write while
the demo owner (BuiltinRoleOwner) is not.
`cmd/server/ai_gateway_flow_test.go` then proves the round-4 guard's
write-then-resolve shape with real calls. Its two BYOK legs deliberately
REPLACE the round-3 redirect proof (which wrote a tenant credential
naming a loopback fake endpoint and asserted the next real call reached
it presenting the tenant key -- the exact primitive the round-4 P0 fix
closes, and a test that confirmed a defect is false comfort): a tenant
BYOK write naming a loopback endpoint (literal, and through a hostname
whose DNS answer is blocked) is refused on the composed stack with
aigateway.base_url_blocked and stored nowhere -- subsequent consult calls
and smile-simulation jobs keep answering through the boot-time platform
credential, and the refused endpoint never receives a request -- while a
write naming a public-shaped endpoint still lands and the read surface
(the same `Resolve` the call path uses) answers with the tenant's own
row, preserving the legitimate arbitrary-public-vendor capability. The
redirect-to-a-second-endpoint wire proof, which cannot survive a real
dial guard (a fake vendor can only listen on loopback), is carried by the
module's own unit suite: `credential_test.go`'s tenant-row-over-platform
`Resolve` proof plus `ssrf_test.go`'s resolve-level guarded-client wiring
proofs.

## Known limitations / deferred

- No dynamic (`go/config`-backed) model routing -- construction-time
  `WithModelRoute` options only, a deliberate round-1 choice (see
  `route.go`'s doc comment); a later round may add a config-driven layer on
  top without changing `ModelRoute`'s shape. This applies to image routes
  exactly as it does to chat ones -- the two share one mechanism.
- The credential HTTP surface is validate-shape-only: a write stores what
  it is given (a non-empty key, an optional base URL -- the base URL
  SSRF-validated at tenant scope since round 4, see "What round 4 adds")
  and no provider round-trip happens until a real call resolves and uses
  it -- no vendor contact and no key-validity check at write time, by
  design.
- The dial-time SSRF pin (`guardedProviderHTTPClient`) only reaches
  providers that implement this module's unexported `httpClientSettable`
  -- its two OpenAI-compatible built-ins, which are the ones its own
  registry factories construct. A third-party provider subpackage that
  registers into `ChatProviderRegistry`/`ImageProviderRegistry` builds
  its own HTTP client and cannot carry the guarded one, so a TENANT-tier
  credential resolving to such a provider is refused at resolve time with
  the coded `ErrProviderNotSSRFGuardable` (never silently dialed
  unguarded -- an unguardable provider has no rebinding-defeating dial
  check at all); the host's fix is a composition decision, routing the
  logical model to a guardable provider or letting this one resolve at
  the platform tier. The platform-tier write is deliberately outside both
  checks by scope boundary, not by gap (intranet LLM gateways are a
  legitimate operator-chosen platform default).
- The stronger base-URL convergence -- a platform-declared whitelist of
  vendor base URLs, instead of arbitrary-public-URL tenant BYOK -- is a
  recorded product decision for a later round, deliberately not
  implemented by the SSRF round (the coordinator's call; the legitimate
  "point at another public OpenAI-compatible vendor" capability is
  preserved and test-pinned).
- The credential surface is write-only and upsert-shaped: no read-back of
  a stored key (deliberate -- no response ever echoes it), no per-key
  metadata beyond provider/scope/baseUrl, no rotation or expiry lifecycle,
  and no write history -- re-PUTting a provider+scope replaces the row. A
  richer credential lifecycle is future work this surface deliberately
  does not prefigure; an admin-console UI rendering this API is the
  natural next consumer after the reference app.
- `UsageEvent.IdempotencyKey` is a fresh random value per call, not derived
  from any caller business-operation id (there is none to derive it from at
  this layer) -- a `UsageRecorder` wanting exactly-once billing-grade
  semantics cannot rely on it for deduplication, mirroring
  `go/metering`'s own `AnalyticsRecorder` fail-open, undeduped stance. This
  applies to the two new image Feature dimensions exactly as it does to
  `"ai.chat_tokens"`.
- No local/self-hosted inference provider (Ollama/vLLM) yet, for chat or
  image alike -- both `ChatProvider` and `ImageProvider` are deliberately
  unaware of whether an implementation is local or remote, so this is
  purely a future additional registration, not an interface change.
- No self-hosted image inference backend either (the design doc's own
  MVP-does-not-build-a-self-hosted-inference-service deferral, restated for
  images) -- out of scope by this round's own task instruction.
- Image generation is refused with a coded error the instant it is not
  wired (`ErrImageGenerationUnavailable`), but there is no admin/HTTP
  surface to discover WHICH logical keys are routed or whether image
  generation is wired at all short of attempting a call -- the identical
  gap `ErrUnroutedModel`'s own round-1 stance already accepts for chat
  routes.
- No progress reporting during an image-generation job (the `jobs.ProgressFn`
  the handler receives is never called) -- a single vendor HTTP call has no
  natural intermediate progress point the way a multi-step pipeline would,
  unlike go/storage's own derive task.
- **Known, non-flaky WARN under `examples/reference-app`'s own smile-simulation
  flow test: transient `SQLITE_BUSY` contention between this round's
  `ai-gateway.image.generate` job and storage's own
  `storage.object.derive.thumbnail` job for the same request, both landing
  on the app's one shared `jobs.StandaloneQueue` (`WorkerCount` 4 by
  default) against one file-backed SQLite database.** The root cause is now
  precisely known, and it is NOT ordinary busy-timeout contention: the
  derive job's gate (`go/storage/repository.go`'s `insertDerivativeIfAbsent`)
  is a read-then-write transaction, and SQLite answers such a transaction's
  write -- upgrading the SHARED lock its earlier gate SELECT holds -- with
  an IMMEDIATE `SQLITE_BUSY` whenever another connection holds the write
  lock, its deadlock avoidance refusing the upgrade rather than consulting
  the busy handler, so no `busy_timeout` setting changes the outcome (the
  WARN's ~13 ms `duration_ms` is that immediacy; the boundary is spelled out
  and pinned in `go/dbkit/AGENTS.md`'s "SQLite busy timeout" section and
  `go/dbkit/dialect/sqlite/busy_timeout_test.go`). This is not the AuditBus
  same-goroutine self-deadlock either (`go/dbkit/AGENTS.md`'s "Audit trail
  collection" limitation), but it shares that one's property that a busy
  timeout cannot cure it. `go/jobs`' own retry/backoff is the existing,
  working convergence mechanism: the losing attempt logs "job attempt
  failed, scheduling retry" and succeeds on its immediate next attempt, so
  `cmd/server/smilesim_flow_test.go`'s own assertions still pass
  deterministically.
  The dbkit-wide SQLite DSN change this entry once floated as a candidate
  cure has since happened -- as of 2026-09-06 `go/dbkit`'s `dialect/sqlite`
  factory declares `_pragma=busy_timeout(5000)` explicitly on every
  connection (see that section) -- and the WARN is not eliminated: the
  immediate-`SQLITE_BUSY` failure is a deterministic property of the gate's
  transaction shape, pinned in isolation by
  `go/dbkit/dialect/sqlite/busy_timeout_test.go`'s read-then-write-upgrade
  test, while whether this flow test actually hits the collision is
  scheduling-dependent -- 140 consecutive runs in the 2026-09-06 dbkit
  round's environment logged none, so neither "reproduces on every run" nor
  "gone" is claimable from test runs alone, and a WARN-free run does not
  mean the race is gone. Actually removing the WARN line now needs
  `go/storage`-module work on the gate's own transaction shape -- taking
  the write lock first (e.g. `BEGIN IMMEDIATE`) instead of reading then
  upgrading -- or a queue-concurrency change in the reference app's own
  wiring (`cmd/server/server.go`'s shared `StandaloneQueue`); tracked as a
  follow-up for whichever round next touches `go/storage`'s derive gate or
  `go/jobs`' `StandaloneQueue` defaults.
  **Correction, 2026-09-06:** this entry's own framing above --
  "`go/jobs`' own retry/backoff is the existing, working convergence
  mechanism" -- was true of the *job's own eventual outcome*
  (`cmd/server/smilesim_flow_test.go` does converge) but glossed over a
  real, separately audited bug in what that convergence actually cost: if
  the attempt that loses the `SQLITE_BUSY` race is `imageGenerateHandler
  .Handle` itself, mid-`writeImageObject` (its own `Create`/`Complete`
  calls are ordinary write transactions, exactly the kind this section
  says can lose the lock race), the failure lands AFTER the vendor call
  earlier in that same attempt already succeeded -- and prior to this
  date, a retry re-ran the ENTIRE handler unconditionally, calling the
  vendor a second time (re-billing the tenant) and reporting a second
  usage event under a fresh random `IdempotencyKey` no `UsageRecorder`
  could dedup. This is now closed: `image_job_store.go`'s
  `ai_gateway_image_jobs` marker records "the vendor already answered"
  the instant it happens, before `writeImageObject` is even attempted, so
  a retry landing here -- from this WARN's own contention or any other
  cause -- reuses the vendor's prior answer and never calls it again, and
  `recordImageUsage` is gated on the marker's own guarded completion so
  usage is reported at most once regardless. The invariant now holds for
  every path into a retry of this job, this WARN's scenario included, not
  only the specific one this fix's own test reproduces
  (`image_gateway_test.go`'s
  `TestImageGenerateHandler_RetryAfterVendorSuccess_DoesNotRecallVendorOrDoubleRecordUsage`).
  **Correction, 2026-09-06 (second pass):** an audit of the fix above found
  the marker write it relied on -- `claimGenerated`'s own INSERT, run AFTER
  the vendor call succeeded -- was itself exactly the kind of ordinary
  write transaction this section's own WARN documents as losing the
  `SQLITE_BUSY` race sometimes: a transient failure of THAT INSERT (not a
  crash -- an everyday busy-timeout-exhausted contention) left no durable
  row behind, so the next retry saw no marker at all and called the vendor
  a second time regardless, reopening the identical bug the paragraph
  above says is closed. `image_job_store.go` now claims a job (a
  content-less "pending" row) BEFORE the vendor is ever called rather than
  writing the vendor's answer only after -- see its own doc comment for
  the full mechanism. This closes the transient-write-failure window (no
  vendor call has happened by the time that first INSERT could fail) and,
  as a side effect, also closes a concurrent-redelivery race a marker-
  after-the-fact design left open (two overlapping `Handle` calls for the
  same job could both pass the old unlocked "no marker yet" check before
  either wrote anything; the claim's own primary-key uniqueness now
  serializes them). Two narrow, explicitly accepted windows remain, both
  bounded to "the job stalls and eventually dead-letters", never "the
  vendor is billed twice" -- see `image_job_store.go`'s own "Accepted
  residual risk" section for the exact shape of each and why closing them
  further would need either an in-process retry loop this codebase
  deliberately does not use for `SQLITE_BUSY`-class contention (see
  `go/storage/derive.go`'s own doc comment) or a lease/staleness mechanism
  out of proportion with how narrow the windows are.
**Review-fix round, 2026-09-07:** a code review of the module against its
own documented promises closed seven findings (plus one cleanup), each
shipped with a regression test that failed before the fix and passes
after (`gateway_test.go`, `openai_compatible_image_test.go`,
`image_gateway_test.go`):

- `relayStream` now records usage at most once per response (a once-flag),
  whatever a provider sends, so a stream carrying usage on many chunks --
  a violation of `ChatChunk`'s own channel contract -- produces one
  metering event, never one per chunk.
- `recordUsage` falls back to the parts' sum (with a warning) when a
  vendor reports a zero `total_tokens` alongside nonzero prompt/completion
  parts -- the shape some OpenAI-compatible hosts produce by omitting
  `total_tokens` -- so such a response is metered for the honest quantity,
  never 0. The `Usage` values the caller sees are never rewritten; this is
  metering policy at the one place usage becomes a billable quantity.
- The OpenAI-compatible image decode path (`imageResultFromWire`) refuses
  a response carrying more than one image -- a data array longer than one
  entry, or a usage object claiming an `image_count` above one -- with the
  new coded `ErrMultipleImageResults` BEFORE any usage could be recorded:
  the whole pipeline (job handler, `ImageJobResult`) delivers one output
  object id, so an `"n": 4` request can never "charge 4 and silently
  return 1".
- The multipart image-edit path enforces the same model/prompt-wins
  invariant the JSON path promises: `buildImageEditMultipart` drops
  same-named `"model"`/`"prompt"` Params entries instead of writing
  duplicate form fields whose winner is parser-defined.
- `writeImagePart` declares the part's real `Content-Type` from `img.MIME`
  (custom part header, since `CreateFormFile` hardcodes
  `application/octet-stream`); the old comment claiming the part's
  Content-Type was set explicitly was wrong and is fixed.
- A credential stored with an empty `base_url` (legal to store; the write
  API documents an omitted `baseUrl` as leaving a provider default in
  effect) now fails distinguishably at call time: the two registry
  constructors refuse it with the coded, Invalid-classified
  `ErrProviderConfigInvalid` (with `pkgcore.ErrMissingSeamConfig` still
  attached as the cause, so `errors.Is`-based registry callers are
  unchanged) instead of an uncoded error a transport layer must fold into
  a bare internal failure.
- `imageGenerateHandler.Handle`'s losing attempt in the concurrent
  redelivery of a job whose marker already says "generated" now answers
  from the marker's own completed `OutputObjectID` (shared
  `completedMarkerResult` helper with the top-of-Handle completed
  short-circuit) instead of returning its own freshly written orphan's
  id, so two `Handle` runs for one job always agree on the id the caller
  reads from `Job.Result`.
- Log keys reverted from `prompt_units`/`completion_units` to
  `prompt_tokens`/`completion_tokens`: go/observability's "token" stem
  now uses a word-boundary rule (pinned by its own
  `TestRedact_TokenStemDoesNotOverRedactUnrelatedWords`), so the rename
  this module made to dodge the old substring redaction is obsolete and
  the natural keys carry the counts unredacted.

**Review-fix round 2, 2026-09-07:** a second code review of the module
closed five findings (P1-1, P1-2, P2b, P2, P3), each with a regression
that failed before the fix and passes after on real runs
(`openai_compatible_image_test.go`, `ssrf_test.go`, and the mirrored
`go/integration/ssrf_test.go`):

- **The image provider now requests its only supported encoding, and
  refuses an empty result** (`openai_compatible_image.go`): every request
  -- the JSON generations body and the edits multipart alike -- carries
  `response_format` = `"b64_json"` explicitly, overriding a same-named
  `Params` entry exactly as model/prompt win and as the chat side forces
  `stream_options` (some vendors default `response_format` to `url`, and
  a url answer would need a second, unauthenticated fetch this provider
  never performs). On the decode side, a data entry whose `b64_json`
  decodes to zero bytes -- an empty value, or the url-shaped answer of a
  vendor that ignored the request -- is refused with the coded
  `ErrProviderResponseInvalid` (reason "empty image data in response")
  instead of surfacing as a zero-byte successful image with `ImageCount`
  1. Both halves ship together by design: requesting alone can be ignored
  by a vendor, and checking length alone would turn a correctable request
  into a runtime failure.
- **The metering fork, recorded as a decision** (P1-2): `WithUsageRecorder`
  has zero call sites anywhere in the shipped codebase, and the reference
  app -- this module's mandatory first consumer -- deducts credits per
  image generation DIRECTLY through `go/billing`: `smilesim.Service.
  Simulate` calls `CreditService.PreDeduct` (10 credits) before
  `Gateway.GenerateImage` and settles Confirm/Refund at the job's
  terminal status, with a durable reservation row plus a reconciliation
  sweep underneath (smilesim's own doc comment states why the job handler
  cannot do this: ai-gateway and billing sit on the same dependency tier
  and neither may import the other). The seam is therefore bypassed, and
  the recorded decision is: **per-use charging of AI calls is a HOST-layer
  billing responsibility**, exercised through `go/billing`'s reserve/
  confirm lifecycle at the host's own request and settlement points -- a
  lifecycle ai-gateway's job handler cannot express (it knows neither the
  price nor the refund policy, and can never import billing) -- while
  `UsageRecorder` remains the analytics-grade usage-reporting seam it has
  always been (a structural mirror of `metering.Recorder.Record`, fail-
  open, undeduped; billing-grade `metering.Enqueue`-in-transaction was
  never the seam's shape and is not built). The round-1 "a real production
  deployment always wires both" gloss is corrected accordingly (see "What
  round 1 shipped"); a host that charges for AI usage should follow the
  smilesim shape, and a host that wants the usage dimensions in a meter
  wires the seam for that purpose alone. With neither wired, AI usage is
  unmetered and calls still work -- the same optionality every other host
  seam in this module documents.
- **The dial-time ring now has its pinning test, in both modules** (P2b):
  the two dial-time tests verified the CHECK (a blocked literal is
  refused at dial time) while the ring's actual reason to exist -- the
  rebinding-defeating property that the dialed address is the validated
  IP LITERAL, never a re-resolved hostname -- had nothing pinning it, and
  a regression dialing the raw hostname would pass every test while
  reopening the rebinding window. `ssrf.go` now routes the actual dial
  through the package-level `providerDialFunc` and the dial-time
  resolution through `resolveProviderHost` (the default wraps
  `net.DefaultResolver` unchanged); the pinning test
  `TestGuardedProviderHTTPClient_DialsTheValidatedIPLiteral` replaces
  both seams, scripts a first-public-then-private answer sequence
  offline, and asserts the dial receives exactly the single resolution's
  validated public IP literal -- and that a blocked answer never reaches
  the dialer. The two literal-address dial tests' comments are corrected
  to state exactly what they prove. `go/integration`'s ring is fixed
  identically in the same round (`webhookDialFunc`/`resolveWebhookHost`,
  `TestNewSafeHTTPClient_DialsTheValidatedIPLiteral`), per the
  errors.go-mandated keep-in-step rule between the two modules' SSRF
  defenses -- its dial-time tests had the identical literal-only gap,
  confirmed by this round's read of the module.
- **A tenant-layer credential for an unguardable provider is refused**
  (P2): `guardTenantScopeDial` previously stayed SILENT when the resolved
  provider did not implement `httpClientSettable` -- every provider
  shipped by this module is guardable, so the hole was latent, but a
  consumer registering a third-party provider into a registry could pair
  it with a tenant BYOK credential and get write-time validation only:
  exactly the layer DNS rebinding defeats. The combination is now refused
  at resolve time with the coded, Invalid-classified
  `ErrProviderNotSSRFGuardable` (decorated with the provider and model
  params), for chat and image resolve alike; the platform tier is
  unchanged (the operator's own default may legitimately point at an
  intranet gateway, and the provider's own client is the operator's
  choice there).
- **The tenantless-unthrottled cost is now stated in the doc** (P3):
  `ratelimit.go`'s file header records that the tenantless path is
  by-design unthrottled -- before the empty-string rule, tenantless
  callers shared one bounded bucket (wrong but bounded); after it, they
  are entirely unthrottled, which is accepted because only the host's own
  in-process code can produce a tenantless call (HTTP-facing tenants come
  from the request context, never the request itself), so no
  attacker-reachable path reaches the limiter tenantless. Doc-only.
