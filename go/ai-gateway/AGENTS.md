# ai-gateway

go/ai-gateway is a vendor-agnostic LLM and image-generation gateway: a
unified `ChatProvider`/`ImageProvider` abstraction, logical-to-vendor model
routing, platform/BYOK credential storage with an SSRF guard on
tenant-writable base URLs, and a `Gateway` facade that wires entitlement
checks and usage reporting around every call. This file is the module-level
discipline that ships with the module to consuming projects; the
repository-wide rules it operates under are the root `CLAUDE.md` plus
`.claude/skills/backend-coding-standards`.

**Metrics instrumentation.** The 09-table "AI gateway" row
is instrumented at the sites this module genuinely has (`metrics.go`,
unit-tested through a ManualReader in `metrics_test.go`):
`aigateway.provider.calls`/`.errors`/`.duration` labeled by the
route-resolved provider and call kind (chat, chat_stream or image) --
chat and chat-stream calls at their provider-invocation sites in
`gateway.go`, image calls at `imageGenerateHandler.callProvider`, the one
choke point every async image job's provider call passes through, never
at `GenerateImage`'s enqueue site (enqueuing is not a provider
invocation), and a chat stream failing after establishment counted in
`relayStream` when its terminal chunk carries Err -- plus
`aigateway.rate_limited` at `checkRateLimit`'s single refusal site
(shared by every entry point, unlabeled by provider because the check
runs before provider resolution by design).

## Public API and module shape

### Chat half

- `ChatProvider` (`types.go`): the vendor-agnostic `Chat`/`ChatStream`
  interface every chat integration implements. `ChatRequest.Model` is a
  caller-facing LOGICAL model key at the `Gateway` boundary and the resolved
  concrete vendor model id once a provider receives it -- see `ChatRequest`'s
  own doc comment for the exact translation point. `ChatChunk`'s doc comment
  pins the streaming channel contract: ordinary chunks, then exactly one
  terminal chunk (real `Usage` on success, `Err` on failure), then the
  channel closes -- no separate error channel. A provider whose vendor
  closes a stream without ever reporting usage ends the channel with neither
  terminal shape rather than fabricating a zero `Usage`; the warning that
  explains the metering gap is logged (see `ChatChunk`'s "known, deliberate
  exception" and `OpenAICompatibleProvider`'s `warnIfNoUsage`).
- `Gateway` (`gateway.go`): the facade -- `Chat`/`ChatStream` are the only
  entry points business code calls. Pipeline: validate the request ->
  check `Entitlements` (if wired) BEFORE any credential/provider resolution,
  so a refused caller is never billed -> resolve the credential -> resolve
  the route -> resolve the provider -> call it -> report usage to
  `UsageRecorder` (if wired). For `ChatStream`, usage is reported at most
  once per response, only once the terminal success chunk (real `Usage`) is
  observed -- never speculatively at stream start, and a provider setting
  `Usage` on several chunks produces one metering event, not one per chunk
  (`relayStream`'s once-flag). When a vendor reports a zero `total_tokens`
  alongside nonzero prompt/completion parts (some OpenAI-compatible hosts
  omit `total_tokens`), the parts' sum is what gets recorded, with a
  warning; the `Usage` the caller's response carries is never rewritten.
- Model routing (`route.go`): `WithModelRoute(logicalKey, provider,
  vendorModel)` is a construction-time `GatewayOption`, not a dynamic
  `go/config` item -- routing is an infrastructure-composition decision the
  host assembler makes once, not a tenant-tunable runtime value, and there
  is no runtime-configurable routing layer. An unrouted logical key is
  `ErrUnroutedModel`, never a silent fallback. Calling `WithModelRoute`
  twice for the same key replaces the earlier route (options apply in
  order; last wins).
- Credential storage (`model.go`, `store.go`, `credential.go`): a single
  scope-tiered `ai_gateway_credentials` table shared by chat and image
  credentials, shaped like `go/config`'s own `configs` table --
  `CredentialScopeSystem` / `CredentialScopeTenant`, the same empty-string
  tenant_id sentinel, the same tenant-override-down-to-system-default
  resolution order (`CredentialService.Resolve`). Platform data, not tenant
  data (see `model.go`'s doc comment): queried through a plain `*gorm.DB`,
  never `dbkit.Repository[T]`, tested with
  `tenancytest.AssertNotTenantScoped`. The API key column is encrypted at
  rest through `CredentialAPIKeySerializerName`, a host-registered
  `dbkit.RegisterEncryptedSerializer` call before `dbkit.Open` -- no blind
  index, since a credential is only ever looked up by (provider, scope,
  tenant), never by its own value. The cipher registered under this name
  must be a DIFFERENT key from any HMAC blind-index key in the host's
  wiring.
- `OpenAICompatibleProvider` (`openai_compatible.go`): the default,
  zero-external-dependency `ChatProvider`, implemented directly against the
  OpenAI-compatible chat-completions wire schema (stdlib `net/http` +
  `encoding/json` only). Supports both the JSON response and
  Server-Sent-Events streaming (`data: {...}` lines, `data: [DONE]`
  terminator, `stream_options.include_usage` requested automatically so the
  final chunk carries real token usage).
- `ChatProviderRegistry` (`registry.go`): a package-level
  `pkgcore.SeamRegistry[ChatProvider]`, after the database/sql
  driver-registration pattern, with `OpenAICompatibleProvider`
  self-registered under `ProviderOpenAICompatible` ("chat.openai-compatible")
  from this package's own `init()`. Unlike pkgcore's four kernel seams,
  `Gateway` calls `Build` fresh on every request -- the resolved credential
  (base_url, api_key) is the `pkgcore.Config` -- since constructing an
  `OpenAICompatibleProvider` performs no I/O. An additional vendor-SDK-backed
  provider (a hypothetical `go/ai-gateway/provider/anthropic` subpackage)
  self-registers into this same registry from its own `init()`; a host that
  never imports that subpackage never resolves its name. Provider
  registrations carry capability bits that nothing in this module validates
  the way `Kernel.Bootstrap` validates the four kernel seams: this registry
  is ai-gateway's own private mechanism.

### Image half

- `ImageProvider` (`image_types.go`): the vendor-agnostic image abstraction,
  the multi-modal counterpart of `ChatProvider` -- three methods,
  `TextToImage`/`ImageToImage`/`Inpaint`, one per operation, each taking a
  request struct built from `Model` (resolved vendor id) + `Prompt` +
  `Params` (the same vendor-passthrough convention `ChatRequest.Params`
  establishes) plus `ImageBytes` (raw content + MIME) for `Input`/`Mask`
  where the operation needs one. **`ImageProvider` itself trades in raw
  bytes, never a storage object reference** -- see `image_gateway.go`'s
  "object-reference / raw-bytes boundary" doc comment for why.
- **The object-reference boundary is drawn at the Gateway/job-handler
  layer, not inside `ImageProvider`.** Images travel through storage
  uniformly, and the interface passes object references, never byte
  streams. `ImageRequest` (`Gateway.GenerateImage`'s own input) and
  `ImageJobResult` (what a caller polls back) satisfy that literally:
  `InputObjectID`/`MaskObjectID`/`OutputObjectID` are the only image-shaped
  fields either carries. `ImageProvider`'s own methods deliberately do not
  carry object references: the registry `Build` config is a flat
  `map[string]string`, which cannot carry a live go/storage handle to a
  provider resolved fresh per job, and forcing a vendor integration to
  embed its own storage plumbing would couple simple integrations (and
  their tests) to go/storage. The job handler is the SINGLE place storage
  I/O happens, translating a reference to bytes before calling the provider
  and bytes back to a reference afterward.
- Async-only pipeline (`image_gateway.go`): `Gateway.GenerateImage(ctx,
  ImageRequest) (jobs.JobID, error)` has NO synchronous counterpart at all
  -- stricter than `Chat`. Every image task runs asynchronously through
  go/jobs. GenerateImage validates the request, checks `Entitlements`
  (reused verbatim, never a second gate), enforces its tenant requirement
  (`ErrImageRequiresTenant`) in the rate limiter's own pipeline position so
  the per-tenant limiter is only ever reached with a real tenant dimension,
  resolves the route/credential once to fail fast (the built provider is
  discarded), and enqueues one `TaskTypeImageGenerate` job
  (`ai-gateway.image.generate`), returning its `JobID` immediately. Every
  real provider call, storage read/write and usage report happens inside
  the job handler, which reruns route/credential resolution fresh -- a job
  may execute long after, and on a different replica than, the enqueuing
  call, and a resolved credential can legitimately have changed in between.
- **A caller retrieves a completed image job's result through go/jobs' own
  mechanism -- there is no second job-status system here.**
  `jobs.Queue.Get(ctx, jobID)` returns the `*jobs.Job`; once `Status` is
  `jobs.StatusSucceeded`, `json.Unmarshal(job.Result.Data, &ImageJobResult{})`
  yields `OutputObjectID` (the generated image's go/storage object id, a
  brand new object under the request's own tenant -- the handler never
  overwrites `InputObjectID`/`MaskObjectID`) and `Usage` (the real vendor
  billing dimensions).
- **Job idempotency** (`image_job_store.go`): the handler enforces, per
  enqueued job, at most one successful vendor call and at most one usage
  record, no matter how many times go/jobs re-runs `Handle` or how many
  overlapping calls run for the same job. A job-id-keyed marker row
  (`ai_gateway_image_jobs`, tenant data, one row per `jobs.JobID`) is
  claimed with a content-less "pending" INSERT BEFORE the vendor is ever
  called (the row's own primary key makes it a compare-and-swap), advanced
  to "generated" with the vendor's answer immediately after the successful
  call, and to "completed" only once go/storage durably holds the output.
  `Handle` settles the marker first on every attempt: a "completed" row
  answers from the row alone; a "generated" row is reused verbatim, never
  re-calling the vendor; a "pending" row refuses this attempt
  (`ErrImageJobClaimInFlight`) rather than resurrecting an orphaned claim.
  An attempt that loses the completion race answers with the winner's
  `OutputObjectID` from the marker row, never a fresh orphan id of its own.
  Usage recording is gated on `markCompleted`'s own guarded transition, so
  it happens at most once per job independent of what a wired
  `UsageRecorder` dedups. Two narrow crash windows remain, both bounded to
  "the job stalls and eventually dead-letters", never "the vendor is billed
  twice" -- see `image_job_store.go`'s "Accepted residual risk" section.
  A "pending" row that never gets cleaned up needs an operator to delete it
  by hand.
- Usage/metering by real vendor billing dimension: the job handler reports
  `"ai.image_count"` and `"ai.image_steps"` (package-level Feature
  constants, `image_gateway.go`) as separate `UsageEvent`s when the vendor's
  own `ImageUsage` reports a positive count for each, folding the
  categorical resolution tier into `Metadata["resolution_tier"]` rather
  than inventing a third numeric Feature for a value that is not a quantity.
  Image usage is billed by real vendor dimensions, never tokens, and the
  two dimensions never cross-contaminate `"ai.chat_tokens"`.
- `ImageProviderRegistry` (`image_registry.go`): the image-generation
  counterpart of `ChatProviderRegistry` (same registration mechanism, same
  `Build`-fresh-per-call shape), with `OpenAICompatibleImageProvider`
  self-registered under `ProviderOpenAICompatibleImage`
  ("image.openai-compatible") from this package's own `init()`.
- `OpenAICompatibleImageProvider` (`openai_compatible_image.go`): the
  default, zero-vendor-SDK `ImageProvider`, implemented directly against the
  OpenAI-compatible images wire schema with stdlib `net/http` +
  `encoding/json` + `mime/multipart` only. `TextToImage` posts JSON to
  `/images/generations`; `ImageToImage` and `Inpaint` both post
  `multipart/form-data` to `/images/edits` (an `image` file part, an
  optional `mask` file part present only for `Inpaint`, `model`/`prompt`
  form fields, and every `Params` entry as an additional field -- a string
  value verbatim, anything else JSON-encoded, with same-named `model`/
  `prompt` entries dropped so the routed model and prompt always win). Every
  request asks for exactly the encoding it accepts: `response_format` is set
  to `b64_json` explicitly on the generation JSON body and the edits
  multipart alike, overriding any same-named `Params` entry (a url answer
  would require a second, unauthenticated fetch this provider never
  performs). The generated bytes' MIME is detected from the decoded bytes
  themselves via `http.DetectContentType`, never trusted from a
  vendor-supplied field -- the same "probe, never trust a header" discipline
  go/storage's own revalidation pipeline applies. A data entry that decodes
  to zero bytes -- an empty `b64_json` value, or the url-shaped answer of a
  vendor that ignored the requested `response_format` -- is refused with the
  coded `ErrProviderResponseInvalid`, never delivered as a zero-byte
  successful image. A response carrying more than one image (a data array
  longer than one, or an `image_count` above one) is refused with the coded
  `ErrMultipleImageResults` before any usage is recorded or output written,
  since the whole pipeline delivers exactly one output object. A response
  with no `usage` object falls back to the real delivered image count with
  zero steps and an empty resolution tier, rather than fabricating vendor
  billing data that was never reported.
- Credential and model routing reuse: image credentials live in the SAME
  `ai_gateway_credentials` table as chat credentials --
  `CredentialService.Resolve`/`SetPlatformCredential`/`SetTenantCredential`
  take a `provider` string that is just a registry name, and
  `"image.openai-compatible"` is exactly as valid a row key as
  `"chat.openai-compatible"`. Model routing is the identical story:
  `WithModelRoute` and `Gateway.routes` are ONE shared namespace for both
  chat and image logical keys (a host picks non-colliding prefixes, e.g.
  `"chat:default"` vs `"image:default"`), resolved through
  `ChatProviderRegistry` or `ImageProviderRegistry` depending on which
  pipeline is asking.

### Seams (Entitlements, UsageRecorder)

- `Entitlements` and `UsageRecorder` (`seams.go`): structurally-typed,
  no-import seams. `ai-gateway` sits at the same dependency tier as
  `billing`, `sharing` and `integration`, so none of the three may import
  each other. Both interfaces mirror the real
  `billing.EntitlementsService.Check` and `metering.Recorder.Record` shapes
  without importing either package; a host with both wired satisfies them
  with a one-line assignment or a short closure (see each interface's own
  doc comment and the `*Func` adapters). Both seams are OPTIONAL: a
  `Gateway` built with neither wired still works, it just enforces no quota
  and reports no usage anywhere. Shipping without `Entitlements` wired
  means this gateway enforces NO quota at all.
- **Per-use charging of AI calls is a HOST-layer billing responsibility.**
  The reference app's smilesim service charges per generation directly
  through `go/billing`'s reserve/confirm lifecycle (`CreditService`) at its
  own request and settlement points -- a lifecycle the job handler cannot
  express: it knows neither the price nor the refund policy, and ai-gateway
  and billing sit on the same dependency tier so neither may import the
  other. `UsageRecorder` is the analytics-grade usage-reporting seam: a
  structural mirror of `metering.Recorder.Record`, fail-open, undeduped; a
  host that charges for AI usage should follow the smilesim shape, and a
  host that wants the usage dimensions in a meter wires the seam for that
  purpose alone.
- `UsageEvent.IdempotencyKey` is a fresh random value per chat call (there
  is no caller business-operation id to derive one from at that layer) --
  a `UsageRecorder` wanting exactly-once billing-grade semantics cannot rely
  on it for deduplication. An image-generation call is different: it is a
  queued `jobs.Job`, which carries a stable id across retries, so its
  `IdempotencyKey` is derived deterministically from the job id and the
  Feature dimension (`imageUsageIdempotencyKey`). That key is defense in
  depth for a recorder that dedups; the invariant's real enforcement point
  is the marker gate that calls `recordImageUsage` at most once per job.

### Module wiring

- `Module` (`module.go`): implements `pkgcore.Module`. `Register` declares
  the module's three permissions (`PermissionRead` "ai-gateway:read",
  `PermissionWrite` "ai-gateway:write", `PermissionManagePlatform`
  "ai-gateway:manage_platform"), registers
  `SystemPurposeCredentialWrite` ("ai-gateway.credential_write") via
  `pkgcore.RegisterSystemPurpose`, claims the image-generation job handler
  on `reg.Jobs` whenever the Gateway was built with `WithImageGeneration`,
  attaches the registry as the Gateway's hostSeams (rate limiting over the
  deployment mode's resolved KVStore), and mounts the HTTP `Handler` at
  `apiPath` (`/api/v1/ai-gateway`) on the host's router. Register performs
  no I/O. The module declares no dynamic config item, no notification type
  and no domain event of its own. The migrations carry the
  `ai_gateway_credentials` and `ai_gateway_image_jobs` tables.
- `WithImageGeneration(queue, objects)` wires the two seams the image
  pipeline needs: the `jobs.Queue` tasks are enqueued on and the handler is
  registered against, and the `storage.ObjectService` the job handler reads
  input images from and writes generated outputs to. Both go/storage and
  go/jobs sit below go/ai-gateway in the module dependency graph, so
  importing them directly is an ordinary downward dependency. A Gateway
  built for chat-only use (no `WithImageGeneration`) registers no job
  handler, and `GenerateImage` on it fails with
  `ErrImageGenerationUnavailable`.

### HTTP surface

- The credential-write HTTP surface (`api/openapi.yaml`): three operations
  under `/api/v1/ai-gateway` -- `GET /credentials/{provider}`,
  `PUT /credentials/{provider}/tenant`,
  `PUT /credentials/{provider}/platform` -- serving chat and image
  providers alike, since provider is just the registry name the
  credentials table's row keys already use. Regenerated from the fragment
  by pinned oapi-codegen v2.8.0 into a committed
  `api/ai-gateway-server.gen.go` and implemented by `Handler` (`handler.go`)
  behind the generated `api.ServerInterface` with a `var _ api.ServerInterface`
  compile-time assertion at the bottom of the file -- a spec change with no
  matching handler change cannot compile.
- **The read path is deliberately metadata-only.** All three operations
  answer with provider/scope/baseUrl and never the api key -- the surface
  is write-only by design: a raw API key, once written, is never returned
  by any operation. GET reports exactly what `CredentialService.Resolve`
  resolves under the caller's own request context -- the tenant's own BYOK
  row when one exists, the platform-wide row otherwise -- with an unknown
  provider answering 404.
- **Two deliberately distinct write permissions, never one shared gate**:
  `PermissionWrite` gates the tenant-scoped write (PUT .../tenant);
  `PermissionManagePlatform` gates the platform-wide write (PUT
  .../platform), which is materially more privileged -- it rewrites the
  fallback every tenant WITHOUT its own BYOK row resolves to. The Handler
  itself performs no permission check: enforcement is the host's
  router-level rbac gate, never the handler.
- Handler translation only, no business decision: every operation is
  answered by the `CredentialService` methods it delegates to, with the
  tenant always read from the request context, never from a request
  parameter, header or body -- there is no tenant_id anywhere on this
  surface. PUT .../platform builds the audited system context
  `SetPlatformCredential`'s own contract requires --
  `tenancy.WithSystemContext` (the audited wrapper, which publishes an
  `EventSystemContextEntered` audit event on the bus `NewHandler` was
  given, and fails the write closed if that publish fails) under
  `SystemPurposeCredentialWrite`, attributed to the fixed actor
  "ai-gateway-http": the audit value that matters is WHICH PATH performed
  the write, not a per-request caller identity a system context cannot
  carry (pkgcore.SystemReason.Actor's own doc comment). The audited wrapper
  is the mandatory path for this write; the module's platform-credential
  operation can only be served when the handler has a bus (a hand-wired
  handler with none fails closed before granting anything).
- Errors: every exported error is an `*apperr.Error` builder; decorated
  errors derive new instances, so match by `apperr.As` and Code, never by
  pointer or `errors.Is` against the vars.

## SSRF defense for the tenant BYOK base URL

- A tenant-supplied dial destination is the same attack class as an
  outbound webhook URL: the platform dials it from its own network
  presenting the credential's API key, so internal destinations must be
  refused at write time and re-checked at dial time, DNS rebinding
  included (`ssrf.go`'s file header states the full boundary). The two
  checks mirror go/integration's own SSRF defense; the two modules sit on
  the same tier and cannot share helpers, so the small stdlib-only
  functions are duplicated and must stay behaviorally equivalent -- the
  `ErrBaseURLBlocked` asymmetry comment in errors.go depends on the two
  refusals agreeing.
- Creation-time refusal: `ValidateBaseURL` (parse, http/https scheme
  whitelist, literal-IP or real-DNS per-address check) runs from
  `SetTenantCredential` before any row is written, over the blocked ranges:
  loopback, private, link-local, CGNAT, NAT64/IPv4-compatible/site-local
  IPv6, and the rest of the stdlib classification (`isBlockedIP`). A
  hostname resolving to both a public and a private address is refused on
  the private answer alone.
- Dial-time re-check: a tenant-tier credential's provider is swapped onto
  `guardedProviderHTTPClient`, whose transport resolves the host once,
  refuses any blocked candidate, and dials the validated IP by address --
  never a second re-resolution a rebinding DNS answer could steer. A
  hostname that passed validation when stored but resolves to an internal
  address by call time fails the call closed. Redirects re-dial through the
  same guarded transport.
- **The scope boundary, stated in code**: only the TENANT-tier write is
  validated (and only the tenant-tier dial is guarded), because only that
  tier is tenant-influenceable. The platform-wide row is written by the
  operator under an audited system context, and an intranet
  OpenAI-compatible LLM gateway is a legitimate platform default -- refusing
  private destinations there would break exactly that deployment.
- **The no-IP-echo rule**: a blocked refusal reached through DNS resolution
  never carries the resolved address in its params (that would make the
  refusal an internal-DNS reconnaissance oracle -- submit hostnames, read
  back internal IPs); a refusal of a literal IP the caller typed still
  echoes it. The asymmetry is load-bearing and pinned by tests
  (`ErrBaseURLBlocked`'s own doc comment).
- The dial-time pin only reaches providers that implement this module's
  unexported `httpClientSettable` -- the two OpenAI-compatible built-ins. A
  tenant-tier credential resolving to a third-party provider without it is
  refused at resolve time with `ErrProviderNotSSRFGuardable` (never
  silently dialed unguarded); the host's fix is a composition decision,
  routing the logical model to a guardable provider or letting this one
  resolve at the platform tier.
- **Tenantless call sites never feed the rate limiter the empty string**
  (`gateway.go`, `image_gateway.go`, `ratelimit.go`): Chat and ChatStream
  gate their per-tenant limiter check on `pkgcore.TenantFromContext`'s ok
  -- a tenantless (system-context) call skips the check rather than sharing
  an empty-string bucket with every other tenantless caller; GenerateImage
  enforces its tenant requirement (`ErrImageRequiresTenant`) before the
  limiter is reached, so the limiter only ever sees a real tenant dimension.
  The tenantless path is by design UNTHROTTLED -- an accepted trade because
  only the host's own in-process code can produce a tenantless call at all
  (HTTP-facing tenants come from the request context, never from the
  request itself), so no attacker-reachable request path reaches the
  limiter with no tenant to key it on.
- Provider error bodies are never echoed: a non-2xx answer's raw body
  reaches neither the returned error's params nor the server-side log. The
  error carries the status code and, when the body is the vendor's JSON
  error envelope, the `error_type`/`error_code` fields parsed from it (read
  at most `maxErrorBodyBytes`) as structured log attributes; observability's
  redaction layer masks credential shapes, never arbitrary echoed content,
  so the body itself is dropped at both sinks.

## Reference-app consumer

- `examples/reference-app/internal/consult`: a small, non-HTTP-generated
  Go service that, given a note's text, asks `gateway.Chat()` for a short
  AI-generated consultation-suggestion summary under the logical model key
  `"chat:default"`. It is mounted as a hand-written route
  (`POST /api/v1/consult/suggest`), outside the OpenAPI machinery.
  `cmd/server/consult_flow_test.go` drives it through the composed HTTP
  stack against an `httptest.Server` standing in for the
  OpenAI-compatible endpoint -- scripted, deterministic responses, no live
  API key.
- `examples/reference-app/internal/smilesim`: the mandatory first consumer
  of `Gateway.GenerateImage` -- given a patient photo already uploaded and
  completed through go/storage's own HTTP surface, it asks the gateway for
  an async before/after AI smile simulation (an `ImageOperationImageToImage`
  request under the logical model key `"image:smile-simulation"`). Two
  hand-written routes: `POST /api/v1/smile-simulation/simulate` enqueues
  the job and answers 202 with its id, and
  `GET /api/v1/smile-simulation/jobs/{id}` polls the app's own
  `jobs.StandaloneQueue` and, once succeeded, decodes `Job.Result.Data`
  into `ImageJobResult` to answer with the generated image's object id and
  its real usage dimensions. smilesim charges per generation through
  `go/billing`'s reserve/confirm lifecycle (see the seams section above).
- The reference app is also the mandatory first consumer of the
  credential-write surface: its demo boot writes the chat and image
  platform defaults from `SPEED_AIGATEWAY_*` configuration, and the module's
  HTTP routes are mounted behind the demo identity layer's real rbac gate
  (`aiGatewayPermissionFor` selects among the three permissions per
  request). `cmd/server/ai_gateway_flow_test.go` proves on the composed
  stack that a tenant-scoped BYOK write naming a loopback endpoint
  (literal, or through a hostname whose DNS answer is blocked) is refused
  with `aigateway.base_url_blocked` and stored nowhere, while a
  public-shaped endpoint still lands and the read surface answers with the
  tenant's own row.

## Known limitations

- No dynamic (`go/config`-backed) model routing -- construction-time
  `WithModelRoute` options only (see `route.go`'s doc comment). Applies to
  image routes exactly as to chat ones -- the two share one mechanism.
- The credential HTTP surface is validate-shape-only: a write stores what
  it is given (a non-empty key, an optional base URL -- the base URL
  SSRF-validated at tenant scope) and no provider round-trip happens until
  a real call resolves and uses it -- no vendor contact and no key-validity
  check at write time, by design.
- The credential surface is write-only and upsert-shaped: no read-back of a
  stored key (deliberate -- no response ever echoes it), no per-key
  metadata beyond provider/scope/baseUrl, and no rotation, expiry or write
  history -- re-PUTting a provider+scope replaces the row.
- A stronger base-URL convergence -- a platform-declared whitelist of
  vendor base URLs instead of arbitrary-public-URL tenant BYOK -- is not
  implemented: the legitimate "point at another public OpenAI-compatible
  vendor" capability is deliberately preserved and test-pinned.
- No local/self-hosted inference provider (Ollama/vLLM), for chat or image
  alike -- both `ChatProvider` and `ImageProvider` are deliberately unaware
  of whether an implementation is local or remote, so a self-hosted
  backend would arrive as an additional registration, not an interface
  change.
- There is no surface to discover which logical keys are routed or whether
  image generation is wired at all short of attempting a call.
- No progress reporting during an image-generation job (the
  `jobs.ProgressFn` the handler receives is never called): a single vendor
  HTTP call has no natural intermediate progress point the way a multi-step
  pipeline would.
- `UsageEvent.IdempotencyKey` for chat is a fresh random value per call --
  a `UsageRecorder` wanting exactly-once semantics cannot dedup on it
  (image usage keys are deterministic; see the seams section).
- The image job's claim/answer marker has two narrow crash windows that
  stall a job until go/jobs dead-letters it (recovery needs an operator to
  delete a stuck row) -- bounded, never a double vendor bill; see
  `image_job_store.go`'s "Accepted residual risk".
- A transient `SQLITE_BUSY` WARN can surface under the reference app's
  smile-simulation flow test when the `ai-gateway.image.generate` job and
  storage's own `storage.object.derive.thumbnail` job collide on the app's
  one shared `jobs.StandaloneQueue` against one file-backed SQLite
  database. The mechanism is precisely known and pinned in
  `go/dbkit`'s SQLite busy-timeout tests: storage's derive gate is a
  read-then-write transaction, and SQLite answers such a transaction's
  lock upgrade with an immediate `SQLITE_BUSY` that no `busy_timeout`
  setting changes. go/jobs' retry/backoff converges the losing attempt on
  its next try, so the flow test's assertions still pass
  deterministically; whether a given run actually hits the collision is
  scheduling-dependent. Removing the WARN line entirely needs a change to
  the derive gate's own transaction shape (taking the write lock first)
  or to the reference app's queue wiring.
- The module's own suite is unit-level, backed by SQLite and an in-memory
  `pkgcore.EventBus`/`KVStore`; SSRF dial-time behavior is exercised
  offline through the `providerDialFunc`/`resolveProviderHost` seams
  (`ssrf_test.go`'s `TestGuardedProviderHTTPClient_DialsTheValidatedIPLiteral`).
