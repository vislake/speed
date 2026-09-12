# ai-gateway

ai-gateway is a vendor-agnostic LLM and image-generation gateway. The chat
surface is one abstraction layer over every vendor integration:
`ChatProvider`, its zero-dependency default `OpenAICompatibleProvider`, a
`ChatProviderRegistry`, scope-tiered encrypted-at-rest BYOK credential
storage, and the `Gateway` facade. The image surface adds `ImageProvider`
(TextToImage/ImageToImage/Inpaint, with its own default and registry) and
the async-only `Gateway.GenerateImage` pipeline, whose storage and queue
integration this module itself provides. A spec-generated HTTP surface
reads and writes platform and tenant BYOK credentials, serving chat and
image providers alike. The tenant-writable BYOK base URL carries an SSRF
guard -- creation-time validation plus a dial-time re-check that defeats
DNS rebinding -- and the gateway's rate-limit call sites handle
tenantless calls explicitly.

**Metrics instrumentation.** The 09-table "AI gateway" row is instrumented at the sites this module genuinely has (`metrics.go`, unit-tested through a ManualReader in `metrics_test.go`): `aigateway.provider.calls`/`.errors`/`.duration` labeled by the route-resolved provider and call kind (chat, chat_stream or image) -- chat and chat-stream calls at their provider-invocation sites in `gateway.go`, image calls at `imageGenerateHandler.callProvider`, the one choke point every async image job's provider call passes through, never at `GenerateImage`'s enqueue site (enqueuing is not a provider invocation), and a chat stream failing after establishment counted in `relayStream` when its terminal chunk carries Err -- plus `aigateway.rate_limited` at `checkRateLimit`'s single refusal site (shared by every entry point, unlabeled by provider because the check runs before provider resolution by design).


## Chat surface

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
  host assembler makes once, not a tenant-tunable runtime value. An
  unrouted logical key is `ErrUnroutedModel`, never a silent fallback.
- Credential storage (`model.go`, `store.go`, `credential.go`): a single
  scope-tiered `ai_gateway_credentials` table modeled directly on
  `go/config`'s own `configs` table -- `CredentialScopeSystem` /
  `CredentialScopeTenant`, the same empty-string tenant_id sentinel
  convention, the same tenant-override-down-to-system-default resolution
  order. Platform data, not tenant data (see `model.go`'s doc comment):
  queried through a plain `*gorm.DB`, never `dbkit.Repository[T]`, tested
  with `tenancytest.AssertNotTenantScoped`. The API key column is encrypted
  at rest through `CredentialAPIKeySerializerName`, bound by the module's own
  `RegisterCredentialAPIKeySerializer(cipher)` before `dbkit.Open` (a nil
  cipher is refused, never registered) -- no blind index, since a credential
  is only ever looked up by (provider, scope, tenant), never by its own
  value.
- A credential stored with an empty `base_url` is legal to store (the write
  API documents an omitted `baseUrl` as leaving a provider default in
  effect) and fails distinguishably at call time: both registry
  constructors refuse it with the coded, Invalid-classified
  `ErrProviderConfigInvalid`, with `pkgcore.ErrMissingSeamConfig` attached
  as the cause so `errors.Is`-based registry callers are unchanged --
  never an uncoded error a transport layer must fold into a bare internal
  failure.
- `OpenAICompatibleProvider` (`openai_compatible.go`): the default,
  zero-external-dependency `ChatProvider`, implemented directly against the
  OpenAI-compatible chat-completions wire schema (stdlib `net/http` +
  `encoding/json` only). Supports both the JSON response and
  Server-Sent-Events streaming (`data: {...}` lines, `data: [DONE]`
  terminator, `stream_options.include_usage` requested automatically --
  overriding a same-named `Params` entry -- so the final chunk carries
  real token usage).
- `ChatProviderRegistry` (`registry.go`): a package-level
  `pkgcore.SeamRegistry[ChatProvider]`, with `OpenAICompatibleProvider`
  self-registered under `ProviderOpenAICompatible` ("chat.openai-compatible")
  from this package's own `init()`. `Gateway` calls `Build` fresh on every
  request -- the resolved credential (base_url, api_key) is the
  `pkgcore.Config` -- since constructing an `OpenAICompatibleProvider`
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
  no-import module interfaces -- `ai-gateway` sits at the same dependency tier as
  `billing`, `sharing` and `integration`, so none of the three may import
  each other. Both interfaces mirror the real
  `billing.EntitlementsService.Check` and `metering.Recorder.Record`
  shapes without importing either package; a host with both wired
  satisfies them with a one-line assignment or a short closure (see each
  interface's own doc comment and the `*Func` adapters). Both modules are
  OPTIONAL: a `Gateway` built with neither wired still works, it just
  enforces no quota and reports no usage anywhere. Per-use charging of AI
  calls is deliberately not this module's role -- it is a host-layer billing
  responsibility exercised through `go/billing` (see "Usage reporting and
  charging").

## Image generation surface

- `ImageProvider` (`image_types.go`): the vendor-agnostic image abstraction,
  the multi-modal counterpart of `ChatProvider` -- three methods,
  `TextToImage`/`ImageToImage`/`Inpaint`, one per operation, each taking a
  request struct built from `Model` (resolved vendor id) + `Prompt` +
  `Params map[string]any` (the identical vendor-passthrough convention
  `ChatRequest.Params` established) plus `ImageBytes` (raw content + MIME)
  for `Input`/`Mask` where the operation needs one. **`ImageProvider`
  itself trades in raw bytes, never a storage object reference** -- see the
  next bullet for why, and `image_gateway.go`'s own "object-reference /
  raw-bytes boundary" doc comment for the full reasoning this file only
  summarizes.
- **The object-reference boundary is drawn at the Gateway/job-handler
  layer, not inside `ImageProvider`.** Images travel through storage
  uniformly, and the Gateway-facing shapes satisfy that literally:
  `ImageRequest` (`Gateway.GenerateImage`'s own input) and
  `ImageJobResult` (what a caller polls back) carry
  `InputObjectID`/`MaskObjectID`/`OutputObjectID` as the only image-shaped
  fields, never a byte. `ImageProvider`'s own three methods, deliberately,
  do not carry object references: `pkgcore.SeamRegistry[ImageProvider].Build`'s
  `Config` is a flat `map[string]string` (the same shape
  `ChatProviderRegistry.Build` uses for `base_url`/`api_key`), which cannot
  carry a live go/storage handle to a provider resolved fresh per job --
  and forcing every future third-party `ImageProvider` implementation to
  embed its own storage plumbing would needlessly couple simple vendor
  integrations (and their tests) to go/storage, the opposite of
  `OpenAICompatibleImageProvider` staying as easily `httptest`-testable as
  its chat sibling. The job handler is the SINGLE place storage I/O
  happens, translating a reference to bytes before calling the provider
  and bytes back to a reference afterward. The boundary is recorded here
  explicitly rather than silently resolved, per this codebase's own
  discipline that a genuine design choice gets written down, not just
  coded.
- Async-only pipeline (`image_gateway.go`): `Gateway.GenerateImage(ctx,
  ImageRequest) (jobs.JobID, error)` has NO synchronous counterpart at
  all -- stricter than `Chat`, which is synchronous by default with
  `ChatStream` as its only async-shaped variant: image tasks are
  asynchronous by design, running through jobs. It validates the request,
  checks `Entitlements` (reused verbatim, never a second gate) BEFORE
  resolving anything, resolves the route/credential once to fail fast on
  an obviously broken configuration (the built provider is discarded --
  see the next point), and enqueues one `TaskTypeImageGenerate` job
  (`ai-gateway.image.generate`), returning its `JobID` immediately. Every
  real provider call, storage read/write and usage report happens inside
  the job handler, which reruns route/credential resolution fresh -- a job
  may execute long after, and on a different replica than, the enqueuing
  call, and a resolved credential can legitimately have changed in
  between, mirroring how `Gateway.Chat` also resolves a provider fresh on
  every call rather than caching one.
- **A caller retrieves a completed image job's result through go/jobs' own
  mechanism -- there is no second job-status system here.**
  `jobs.Queue.Get(ctx, jobID)` returns the `*jobs.Job`; once `Status` is
  `jobs.StatusSucceeded`, `json.Unmarshal(job.Result.Data, &ImageJobResult{})`
  yields `OutputObjectID` (the generated image's go/storage object id, a
  brand new object under the request's own tenant -- the handler never
  overwrites `InputObjectID`/`MaskObjectID`) and `Usage` (the real vendor
  billing dimensions). This is the same `Queue.Get` polling shape
  go/storage's own consumers use for the thumbnail-derive task;
  ai-gateway builds nothing new on top of it.
- Concurrent redelivery convergence: two `Handle` runs for one job always
  agree on the id the caller reads from `Job.Result` -- a losing attempt
  in a concurrent redelivery of a job whose marker already says
  "generated" answers from the marker's own completed `OutputObjectID`
  (the `completedMarkerResult` helper, shared with the top-of-`Handle`
  completed short-circuit), never its own freshly written orphan's id.
- Usage/metering by real vendor billing dimension: `UsageEvent`'s
  `Feature`/`Quantity`/`Metadata` fields are generic rather than
  chat-token-specific. The job handler reports `"ai.image_count"` and
  `"ai.image_steps"` (new package-level Feature constants,
  `image_gateway.go`) as separate `UsageEvent`s when the vendor's own
  `ImageUsage` reports a positive count for each, folding the categorical
  resolution tier into `Metadata["resolution_tier"]` rather than
  inventing a third numeric Feature for a value that is not a quantity --
  `UsageEvent.Metadata`'s own doc comment already names it for exactly this
  kind of small, bounded call context. `"ai.chat_tokens"` is never reported
  for an image call, and vice versa: `image_gateway_test.go`'s own
  `TestImageGenerateHandler_TextToImage_WritesNewObjectAndReportsUsage`
  pins that the two dimensions never cross-contaminate.
- `ImageProviderRegistry` (`image_registry.go`): a package-level
  `pkgcore.SeamRegistry[ImageProvider]` -- the same `Build`-fresh-per-call
  reasoning and the same flat `pkgcore.Config{"base_url", "api_key"}`
  shape as the chat registry -- with `OpenAICompatibleImageProvider`
  self-registered under `ProviderOpenAICompatibleImage`
  ("image.openai-compatible") from this package's own `init()`.
- Both built-ins are also `pkgcore.Component`s (`provider_components.go`):
  `chat.openai-compatible` (module `"chat"`) and
  `image.openai-compatible` (module `"image"`), each `Providing` its
  contract type, declaring the same
  `MultiReplicaSafe|SurvivesRestart|Stateless` bits its registration
  declares, and taking the same two-key block (`base_url`, `api_key`) the
  flat registration reads. The component name IS the route's logical
  provider name -- the string `WithModelRoute` takes, credential rows are
  keyed by and the registries register under -- so the route-to-component
  resolution is the module's own identity, pinned by
  `TestProviderComponentNames_MatchProviderNamesAndCapabilities`: a host's
  existing routes, credential rows and composition selections need no
  translation and gain no new configuration obligation. The per-request
  shape of the component face is `pkgcore.Build` with an override carrying
  the credential just resolved (`TestProviderComponentsAssembleAndBuildPerCall`),
  and both faces funnel through the same constructors
  (`openaiCompatibleFromConfig` / `openaiCompatibleImageFromConfig`), so
  neither can diverge on validation or on the coded config refusal.
  **Resolution prefers the component face**: a Gateway attached to an
  assembly (`Module.Register` hands it the registry) resolves a route's
  provider through the selected member that carries the route's name first
  and falls back to the package-level `ChatProviderRegistry` /
  `ImageProviderRegistry` for a name the composition did not select -- so a
  composition selecting a provider component serves every request through
  the component face, an unselected name (or a Gateway built outside any
  assembly) behaves exactly as before, and
  `WithChatProviderRegistry` / `WithImageProviderRegistry` are the overrides
  of that fallback face. The two faces build the same implementation under
  the same name, which is what makes the preference behavior-preserving;
  the preference itself is pinned by
  `provider_resolution_test.go` (a route naming a selected component is
  served by its own instance; a route naming an unselected name keeps the
  registry path).
- `OpenAICompatibleImageProvider` (`openai_compatible_image.go`): the
  default, zero-vendor-SDK `ImageProvider`, implemented directly against
  the OpenAI-compatible images wire schema with stdlib `net/http` +
  `encoding/json` + `mime/multipart` only. `TextToImage` posts JSON to
  `/images/generations`; `ImageToImage` and `Inpaint` both post
  `multipart/form-data` to `/images/edits` (an `image` file part whose
  real `Content-Type` comes from `img.MIME` via `writeImagePart`'s custom
  part header -- `CreateFormFile` hardcodes `application/octet-stream` --
  an optional `mask` file part present only for `Inpaint`, `model`/`prompt`
  form fields with same-named `Params` entries dropped by
  `buildImageEditMultipart` rather than written as duplicate fields whose
  winner is parser-defined -- the same model/prompt-wins invariant the
  JSON path promises -- and every other `Params` entry as an additional
  field: a string value verbatim, anything else JSON-encoded). Every
  request asks for exactly the encoding it accepts: `response_format` is
  set to `b64_json` explicitly on the generation JSON body and the edits
  multipart alike, overriding a same-named `Params` entry exactly as
  model/prompt win and as the chat side forces `stream_options` (some
  vendors default `response_format` to `url`, and a url answer would
  require a second, unauthenticated fetch this provider never performs).
  Every response is decoded from `{"data": [{"b64_json": "..."}], "usage":
  {...}}`; the generated bytes' MIME is detected from the decoded bytes
  themselves via `http.DetectContentType`, never trusted from a
  vendor-supplied field -- the same "probe, never trust a header"
  discipline go/storage's own revalidation pipeline applies. A data entry
  that decodes to zero bytes -- an empty `b64_json` value, or the
  url-shaped answer of a vendor that ignored the requested
  `response_format` -- is refused with the coded `ErrProviderResponseInvalid`
  (reason "empty image data in response"), never delivered as a zero-byte
  successful image with `ImageCount` 1; requesting alone can be ignored
  by a vendor and checking length alone would turn a correctable request
  into a runtime failure, so the two halves ship together by design. The
  decode path (`imageResultFromWire`) refuses a response carrying more
  than one image -- a data array longer than one entry, or a usage object
  claiming an `image_count` above one -- with the coded
  `ErrMultipleImageResults` BEFORE any usage could be recorded: the whole
  pipeline (job handler, `ImageJobResult`) delivers one output object id,
  so an `"n": 4` request can never "charge 4 and silently return 1". A
  response with no `usage` object falls back to the real delivered image
  count (`len(data)`) with zero steps and an empty resolution tier,
  rather than fabricating vendor billing data that was never reported.
- Credential and model routing are shared with the chat surface,
  unchanged in shape: image credentials live in the SAME
  `ai_gateway_credentials` table as chat credentials --
  `CredentialService.Resolve`/`SetPlatformCredential`/`SetTenantCredential`
  take a `provider` string that is just a registry name, and
  `"image.openai-compatible"` is exactly as valid a row key as
  `"chat.openai-compatible"`. Model routing is the identical story:
  `WithModelRoute` and `Gateway.routes` are ONE shared
  `map[string]ModelRoute` namespace for both chat and image logical keys
  (a host picks non-colliding prefixes, e.g. `"chat:default"` vs
  `"image:default"`), resolved through `ChatProviderRegistry` or
  `ImageProviderRegistry` depending on which pipeline is asking -- reuse,
  not extension.
- Conditional job-handler registration (`module.go`): `Register` claims
  the image-generation job handler on `reg.JobsSeat()` whenever the module's
  `Gateway` was built with `WithImageGeneration(queue, objects)` -- both
  go/storage's `*storage.ObjectService` and go/jobs' `jobs.Queue` are
  injected directly: both sit below ai-gateway in the dependency graph,
  so this is an ordinary downward dependency, unlike
  `Entitlements`/`UsageRecorder`, which stay structurally-typed no-import
  modules because ai-gateway sits at billing/metering's own tier. A
  `Gateway` built for chat-only use (no `WithImageGeneration`) registers
  no job handler at all, and `Gateway.GenerateImage` on such a `Gateway`
  always fails with `ErrImageGenerationUnavailable`.
- Runnable documentation (`example_test.go`): `Example_generateImage` walks
  the whole async pipeline this section describes -- a real
  `jobs.StandaloneQueue`, a real `storage.ObjectService` (both required by
  `WithImageGeneration`), `Gateway.GenerateImage`, wiring the queue to
  `reg.Jobs` through `jobs.Wire` exactly as `examples/reference-app`'s own
  `internal/app/server.go` does, and polling the enqueued job to
  completion against a fake OpenAI-compatible images endpoint -- alongside
  the chat-only `Example`; a new public API ships with a compilable godoc
  `Example`.

## Usage reporting and charging

- Per-use charging of AI calls is deliberately a HOST-layer billing
  responsibility, exercised through `go/billing`'s reserve/confirm
  lifecycle at the host's own request and settlement points. The reference
  app's smilesim service is the shipped shape of that decision:
  `smilesim.Service.Simulate` calls `CreditService.PreDeduct` (10 credits)
  before `Gateway.GenerateImage` and settles Confirm/Refund at the job's
  terminal status, with a durable reservation row plus a reconciliation
  sweep underneath. smilesim's own doc comment states why the job handler
  cannot do this: ai-gateway and billing sit on the same dependency tier
  and neither may import the other -- the job handler knows neither the
  price nor the refund policy. `UsageRecorder` therefore stays the
  analytics-grade usage-reporting module: a structural mirror of
  `metering.Recorder.Record`, fail-open, undeduped -- billing-grade
  `metering.Enqueue`-in-transaction is not the module's shape and is not
  built -- and no shipped host wires `WithUsageRecorder`. A host that
  charges for AI usage follows the smilesim shape; a host that wants the
  usage dimensions in a meter wires the module for that purpose alone. With
  neither wired, AI usage is unmetered and calls still work -- the same
  optionality every other host-injected module in this module documents.
- Stream usage is recorded at most once per response: `relayStream` guards
  the metering call with a once-flag, so a stream carrying usage on many
  chunks -- a violation of `ChatChunk`'s own channel contract -- produces
  one metering event, never one per chunk.
- `recordUsage` falls back to the parts' sum (with a warning) when a
  vendor reports a zero `total_tokens` alongside nonzero prompt/completion
  parts -- the shape some OpenAI-compatible hosts produce by omitting
  `total_tokens` -- so such a response is metered for the honest quantity,
  never 0. The `Usage` values the caller sees are never rewritten; this is
  metering policy at the one place usage becomes a billable quantity.
- Usage log keys are `prompt_tokens`/`completion_tokens`:
  go/observability's "token"-stem redaction uses a word-boundary rule
  (pinned by its own
  `TestRedact_TokenStemDoesNotOverRedactUnrelatedWords`), so the natural
  keys carry the counts unredacted. The metering behaviours above are
  pinned by the module's own tests (`gateway_test.go`,
  `openai_compatible_image_test.go` and `image_gateway_test.go` carry
  them).

## Credential HTTP surface

- The credential-write HTTP surface (`api/openapi.yaml`): three operations
  under `/api/v1/ai-gateway` -- `GET /credentials/{provider}`,
  `PUT /credentials/{provider}/tenant`,
  `PUT /credentials/{provider}/platform` -- serving chat and image
  providers alike, since provider is just the registry name the
  credentials table's own row keys already use. Regenerated by the pinned
  oapi-codegen v2.8.0 into a committed `api/ai-gateway-server.gen.go` and
  implemented by `Handler` (`handler.go`) behind the generated
  `api.ServerInterface` with a `var _ api.ServerInterface` compile-time
  assertion at the bottom of the file -- a spec change with no matching
  handler change cannot compile.
- **The read path is deliberately metadata-only.** All three operations
  answer with provider/scope/baseUrl and never the api key -- the surface
  is write-only by design, the rule this module's Security obligations (no
  secrets in API responses) and the spec fragment's own header both state.
  GET answers "which scope currently answers for a provider?": it reports
  exactly what `CredentialService.Resolve` resolves under the caller's own
  request context -- the tenant's own BYOK row when one exists, the
  platform-wide row otherwise -- with an unknown provider answering 404
  and the tenant read carrying no scope suffix at all (the resolution IS
  the read).
- **Two deliberately distinct write permissions, never one shared gate**
  (`module.go`'s doc comment on the pair): `PermissionWrite`
  ("ai-gateway:write") gates the tenant-scoped write (PUT .../tenant);
  `PermissionManagePlatform` ("ai-gateway:manage_platform") gates the
  platform-wide write (PUT .../platform), which is materially more
  privileged -- it rewrites the fallback every tenant WITHOUT its own BYOK
  row resolves to. `PermissionRead` ("ai-gateway:read") gates GET/HEAD.
  The Handler itself performs no permission check, exactly like every other
  module handler in this codebase: the host's router-level rbac gate
  decides. The reference app's `aiGatewayPermissionFor` (`internal/app/demo/demo_subject.go`)
  is the first concrete instance of that gate: GET/HEAD read, a
  `/platform`-suffixed path manage_platform, any other write.
- Handler translation only, no new business decision (`handler.go`'s own
  doc comment): every operation is answered entirely by the EXISTING
  `CredentialService` methods (`Resolve` / `SetTenantCredential` /
  `SetPlatformCredential`), with the tenant always read from the request
  context, never from a request parameter, header or body (there is no
  tenant_id anywhere on this surface). The one translation layer: PUT
  .../platform builds the audited system context `SetPlatformCredential`'s
  own contract requires -- `tenancy.WithSystemContext` (the audited
  wrapper, which publishes an `EventSystemContextEntered` audit event on
  the bus `NewHandler` was given, and fails the write closed if that
  publish fails) under `SystemPurposeCredentialWrite`
  ("ai-gateway.credential_write"), attributed to the fixed actor
  "ai-gateway-http". The audit value that matters is WHICH PATH performed
  the write, not a per-request caller identity a system context cannot
  carry here (pkgcore.SystemReason.Actor's own doc comment). This module
  sits well above tenancy in the dependency graph, so
  `tenancy.WithSystemContext` is importable; a bare
  `pkgcore.WithSystemContext` on this live request path would take the
  escape hatch with zero audit events, so the audited wrapper is mandatory
  for this path.
- `Module` (`module.go`) implements the module contract; `Register` declares
  the three permissions on `reg.PermissionsSeat()`,
  registers `SystemPurposeCredentialWrite` via
  `pkgcore.RegisterSystemPurpose`, and mounts the Handler at `apiPath` --
  the module's single `Register(reg *pkgcore.ComponentRegistry)` contract unchanged. The
  module's further contributions are the migrations (the
  `ai_gateway_credentials` table) and the `Gateway`/`CredentialService`
  accessors a host wires directly; `Register` declares no config item,
  notification type, domain event or audit action of its own.
- Tests: `handler_test.go` pins the request/response translation in
  isolation -- an unknown provider's read is 404, an empty key and a
  malformed body are refused, a tenant write with no tenant in the context
  is refused, an omitted baseUrl is omitted from the response, and the
  apiKey is never echoed; `handler_example_test.go`'s `ExampleHandler`
  compiles AND runs a composed walk of the whole surface -- 404 with no
  credential, platform write, tenant BYOK override, and `Resolve`
  answering the tenant's row; and the reference app's `flowtests/ai_gateway_flow_test.go`
  is the mandatory first consumer's end-to-end proof (see "Reference-app
  consumer").

## SSRF and dialing posture

- **SSRF defense for the tenant BYOK base URL** (`provider_guard.go`,
  `errors.go`, `credential.go`, `gateway.go`, `image_gateway.go`): a tenant
  admin writes the base URL of an OpenAI-compatible endpoint that the
  platform's own network then dials presenting the credential's API key --
  the same outbound-dial primitive the codebase's SSRF rules protect for
  webhooks (SSRF protection is mandatory, including DNS-rebinding
  protection), and now the same shared guard: both checks run over
  `go/pkgcore/safehttp`, whose blocked set (loopback, private, link-local,
  CGNAT, NAT64/IPv4-compatible/site-local IPv6, and the rest of the stdlib
  classification) is the one authority for what counts as blocked.
  Creation-time refusal through `ValidateBaseURL` (parse, http/https scheme
  gate through the guard, literal-IP or real-DNS per-address check, this
  module's coded error vocabulary and no-echo shaping around it), and a
  dial-time re-check that PINS the address actually connected: a
  tenant-tier credential's provider is swapped onto
  `guardedProviderHTTPClient`, the guard's client, whose dialler refuses
  any blocked address about to be connected -- never a second resolution a
  rebinding DNS answer could steer. A hostname that passed validation when
  stored but resolves to an internal address by call time fails the call
  closed.
- **The dialed address is the validated one -- the guard's own property**
  (`provider_guard.go`, `go/pkgcore/safehttp`): the rebinding defense (the
  check runs on the exact address about to be dialled, with no second
  lookup in between) lives in the shared guard and is pinned there by
  `TestGuard_DNSRebindingCannotGetPastTheConnectTimeCheck` and
  `TestGuard_ClientCannotReachALoopbackServer`;
  `TestGuardedProviderHTTPClient_RefusesLoopbackAtDialTime` pins this
  module's client's dial-time refusal, matched by
  `errors.Is(err, safehttp.ErrBlockedAddress)` through the provider
  layer's wrapping. The two modules' refusals keep agreeing because there
  is exactly one implementation of the predicate, as `ErrBaseURLBlocked`'s
  own doc comment requires.
- **The scope boundary, stated in code** (`provider_guard.go`'s file header,
  `credential.go`'s both setter doc comments): only the TENANT-tier write
  is validated (and only the tenant-tier dial is guarded), because only
  that tier is tenant-influenceable. The platform-wide row is written by
  the operator under an audited system context, and an intranet
  OpenAI-compatible LLM gateway is a legitimate platform default -- the
  reference app's boot-time platform credential and its loopback test
  harness live on that trusted side of the boundary.
- **The no-IP-echo rule**: a blocked refusal reached through DNS
  resolution never carries the resolved address in its params (that would
  make the refusal an internal-DNS reconnaissance oracle -- submit
  hostnames, read back internal IPs); a refusal of a literal IP the caller
  typed still echoes it. The asymmetry is pinned by tests on both sides,
  and `ErrBaseURLBlocked`'s own doc comment demands the two modules'
  refusals keep agreeing.
- Three coded errors, Invalid-classified like integration's:
  `aigateway.base_url_invalid`, `aigateway.base_url_unresolvable`,
  `aigateway.base_url_blocked`. An empty baseUrl stays legal to store
  ("no base URL configured").
- **The non-2xx response body never reaches the caller or the log**
  (`openai_compatible.go`'s `errorFromResponse`, shared by the chat and
  image providers): a non-2xx answer is the common error path of every
  provider call, so -- unlike the address rule, which only guards blocked
  destinations -- this half of the no-echo posture applies to ALLOWED
  destinations too. The returned error's params carry the status code
  only. The server-side log likewise carries no raw body (however
  bounded): observability's redaction layer masks credential shapes, never
  arbitrary echoed content, and content-moderation-class refusals
  routinely echo the refused request input into the error envelope's
  free-text message. What reaches the log instead is the vendor's own
  error contract: `error.type` / `error.code`, the envelope's two
  enumeration fields, parsed from the JSON body (read at most
  `maxErrorBodyBytes`) into the structured attributes `error_type` /
  `error_code`; a body that is not the JSON envelope contributes no
  attributes and the line carries the status code alone. What operators
  lose is the vendor's prose; what they keep is the status and the two
  coded fields vendors triage by.
- **Tenantless call sites never feed the rate limiter the empty string**
  (`gateway.go`, `image_gateway.go`, `ratelimit.go`): Chat and ChatStream
  gate their per-tenant limiter check on `pkgcore.TenantFromContext`'s
  ok -- a tenantless (system-context) call, a legitimate path the module's
  own docs bless, skips the check with the reason stated at the call site
  rather than sharing an empty-string bucket with every other tenantless
  caller; GenerateImage hoists its existing tenant requirement
  (`ErrImageRequiresTenant`) into the limiter's own pipeline position, so
  the limiter is only ever reached with a real tenant dimension. The
  tenantless path is by design entirely unthrottled (`ratelimit.go`'s file
  header records it): only the host's own in-process code can produce a
  tenantless call (HTTP-facing tenants come from the request context,
  never the request itself), so no attacker-reachable path reaches the
  limiter tenantless.
- Product decision, recorded not implemented: the stronger convergence --
  a platform-declared whitelist of base URLs only -- is not built; the
  legitimate capability (a tenant pointing its BYOK credential at any
  PUBLIC OpenAI-compatible vendor) is deliberately preserved and
  test-pinned.
- Tests: `provider_guard_test.go` pins `ValidateBaseURL`'s refusal
  surface case for case with integration's suite (blocked literals, the
  no-echo/echo asymmetry, public-IP allowed, scheme/malformed/no-host/
  unresolvable refusals), the guarded client's dial-time refusal of
  loopback and non-refusal of public addresses (matched by
  `safehttp.ErrBlockedAddress`) and its no-overall-timeout posture, plus
  resolve/resolveImage wiring proofs that a tenant-tier credential's
  provider carries the guarded client and a platform-tier one does not;
  `credential_test.go`,
  `handler_test.go` and the reference app's `flowtests/ai_gateway_flow_test.go`
  pin the refusals through the service, the HTTP envelope and the
  composed stack; `ratelimit_test.go` pins that tenantless calls never
  consult the limiter; `openai_compatible_test.go` and
  `openai_compatible_image_test.go` pin that a non-2xx answer's raw body
  reaches neither the returned error's params nor the server-side log --
  the envelope's `error_type` / `error_code` attributes are what the log
  carries, and a non-JSON body contributes nothing to it.

## Reference-app consumer

`examples/reference-app/internal/consult` is the chat surface's mandatory
first consumer: a small, non-HTTP-generated Go service that, given a note's
text, asks `gateway.Chat()` for a short AI-generated consultation-suggestion
summary under the logical model key `"chat:default"`. It is mounted as a
hand-written route (`POST /api/v1/consult/suggest`), outside the OpenAPI
machinery -- the same pattern the demo module's own hand-written patient-
message route already establishes in this app. `flowtests/consult_flow_test.go`
drives it through the composed HTTP stack against an `httptest.Server`
standing in for the OpenAI-compatible endpoint -- scripted, deterministic
responses, no live API key required or used.

`examples/reference-app/internal/smilesim` is the image surface's mandatory
first consumer of `Gateway.GenerateImage`: given a patient photo the caller
already uploaded and completed through go/storage's own HTTP surface, it
asks the gateway for an async before/after AI smile simulation -- an
`ImageOperationImageToImage` request under the logical
model key `"image:smile-simulation"`. Two hand-written
routes (`internal/app/smilesim.go`, outside the OpenAPI machinery for the
identical reason internal/app/consult.go's route is): `POST
/api/v1/smile-simulation/simulate` enqueues the job and answers 202 with
its id, and `GET /api/v1/smile-simulation/jobs/{id}` polls the app's own
`jobs.StandaloneQueue` (the same pool storage's thumbnail-derive task and
notification's delivery task share) and, once succeeded, decodes
`Job.Result.Data` into `ImageJobResult` to answer with the generated
image's object id and its real usage dimensions.
`flowtests/smilesim_flow_test.go` drives the whole chain through the
composed HTTP stack -- storage's real upload lifecycle for the input photo,
the async job, and the poll to completion -- against an `httptest.Server`
scripting the OpenAI-compatible images-edits endpoint, asserting the
genuine `multipart/form-data` request (the uploaded photo's own
storage-sanitized bytes, the routed vendor model id, no stray `mask` part
for an image-to-image request) and that the generated output lands as a
real, separate, completed go/storage object.

The reference app is also the credential-write surface's mandatory first
consumer. Its demo boot (`internal/app/server.go`) writes the chat and image
platform defaults from `SPEED_AIGATEWAY_*` configuration; those writes sit
side by side with the module's own HTTP routes, mounted behind
`internal/app/demo/demo_subject.go`'s real rbac gate: `aiGatewayPermissionFor` selects among
the three permissions per request (see "Credential HTTP surface"), and the
seeded demo role `demo-aigateway-tenant-writer` (granted aigateway:read +
aigateway:write in every demo tenant, deliberately NOT
aigateway:manage_platform) proves on the composed stack that a
tenant-scoped principal really is refused the platform-wide write while
the demo owner (BuiltinRoleOwner) is not.
`flowtests/ai_gateway_flow_test.go` proves the SSRF guard's
write-then-resolve shape with real calls: a tenant BYOK write naming a
loopback endpoint (literal, and through a hostname whose DNS answer is
blocked) is refused on the composed stack with aigateway.base_url_blocked
and stored nowhere -- subsequent consult calls and smile-simulation jobs
keep answering through the boot-time platform credential, and the refused
endpoint never receives a request -- while a write naming a public-shaped
endpoint still lands and the read surface (the same `Resolve` the call
path uses) answers with the tenant's own row, preserving the legitimate
arbitrary-public-vendor capability. The redirect-to-a-second-endpoint wire
proof -- a write landing, then the next real call reaching the fake
endpoint presenting the tenant key -- cannot survive a real dial guard (a
fake vendor can only listen on loopback), so that shape is carried by the
module's own unit suite: `credential_test.go`'s tenant-row-over-platform
`Resolve` proof plus `provider_guard_test.go`'s resolve-level
guarded-client wiring proofs.

## Known limitations / deferred

- No dynamic (`go/config`-backed) model routing -- construction-time
  `WithModelRoute` options only, a deliberate choice (see `route.go`'s doc
  comment); `ModelRoute`'s shape would not need to change for a
  config-driven layer on top. This applies to image routes exactly as it
  does to chat ones -- the two share one mechanism.
- The credential HTTP surface is validate-shape-only: a write stores what
  it is given (a non-empty key, an optional base URL, the latter
  SSRF-validated at tenant scope) and no provider round-trip happens until
  a real call resolves and uses it -- no vendor contact and no key-validity
  check at write time, by design.
- The dial-time SSRF pin (`guardedProviderHTTPClient`) only reaches
  providers that implement this module's unexported `httpClientSettable`
  -- its two OpenAI-compatible built-ins, which are the ones its own
  registry factories construct. A third-party provider subpackage that
  registers into `ChatProviderRegistry`/`ImageProviderRegistry` builds
  its own HTTP client and cannot carry the guarded one, so a TENANT-tier
  credential resolving to such a provider is refused at resolve time --
  chat and image resolve alike -- with the coded, Invalid-classified
  `ErrProviderNotSSRFGuardable`, decorated with the provider and model
  params (never silently dialed unguarded: an unguardable provider has no
  rebinding-defeating dial check at all); the host's fix is a composition
  decision, routing the logical model to a guardable provider or letting
  this one resolve at the platform tier. The platform-tier write is
  deliberately outside both checks by scope boundary, not by gap -- an
  intranet LLM gateway is a legitimate operator-chosen platform default,
  and the provider's own client is the operator's choice there.
- The credential surface is write-only and upsert-shaped: no read-back of
  a stored key (deliberate -- no response ever echoes it), no per-key
  metadata beyond provider/scope/baseUrl, no rotation or expiry lifecycle,
  and no write history -- re-PUTting a provider+scope replaces the row.
  The surface is deliberately shaped not to prefigure a richer credential
  lifecycle.
- `UsageEvent.IdempotencyKey` is a fresh random value per call, not derived
  from any caller business-operation id (there is none to derive it from at
  this layer) -- a `UsageRecorder` wanting exactly-once billing-grade
  semantics cannot rely on it for deduplication, in line with
  `go/metering`'s own `AnalyticsRecorder` fail-open, undeduped stance. This
  applies to the two image Feature dimensions exactly as it does to
  `"ai.chat_tokens"`.
- No separately named local/self-hosted inference provider (an Ollama/
  vLLM-style registration) exists, for chat or image -- and none is
  warranted while such hosts speak the OpenAI protocol, because the
  OpenAI-compatible built-ins already reach them as a config variant.
  `OpenAICompatibleProvider`'s own doc comment says it implements the
  chat-completions schema "shared by OpenAI itself and every
  OpenAI-compatible host (Azure OpenAI, DeepSeek, many self-hosted/
  open-weight gateways)" against a fully configurable base URL, and its
  image twin is built the same way; pointing a credential at a
  self-hosted host's OpenAI-compatible endpoint is configuration, not
  code, and a host that ignores the Authorization header accepts any
  non-empty api_key. A non-public endpoint is a platform-tier credential
  by design: the SSRF guard validates and dial-guards only the
  tenant-influenceable tier, and provider_guard.go's own file header names
  "an OpenAI-compatible LLM gateway on the operator's own intranet" as the
  legitimate platform default the guard must not break. The day a
  self-hosted host diverges from the OpenAI protocol, or an operator
  wants a vendor SDK, it becomes one more registration in
  `ChatProviderRegistry`/`ImageProviderRegistry` -- no interface change.
  No self-hosted image inference backend is shipped either (the design's
  own MVP-does-not-build-a-self-hosted-inference-service deferral): the
  module is the client of an inference endpoint, never the inference
  service itself.
- Image generation is refused with a coded error the instant it is not
  wired (`ErrImageGenerationUnavailable`), but there is no admin/HTTP
  surface to discover WHICH logical keys are routed or whether image
  generation is wired at all short of attempting a call -- the same gap
  `ErrUnroutedModel` already accepts for chat routes.
- No progress reporting during an image-generation job (the `jobs.ProgressFn`
  the handler receives is never called) -- a single vendor HTTP call has no
  natural intermediate progress point the way a multi-step pipeline would,
  unlike go/storage's own derive task.
- **Known, non-flaky WARN under `examples/reference-app`'s own
  smile-simulation flow test: transient `SQLITE_BUSY` contention between
  the `ai-gateway.image.generate` job and storage's own
  `storage.object.derive.thumbnail` job for the same request, both landing
  on the app's one shared `jobs.StandaloneQueue` (`WorkerCount` 4 by
  default) against one file-backed SQLite database.** The root cause is
  precise, and it is NOT ordinary busy-timeout contention: the derive
  job's gate (`go/storage/repository.go`'s `insertDerivativeIfAbsent`)
  is a read-then-write transaction, and SQLite answers such a transaction's
  write -- upgrading the SHARED lock its earlier gate SELECT holds -- with
  an IMMEDIATE `SQLITE_BUSY` whenever another connection holds the write
  lock, its deadlock avoidance refusing the upgrade rather than consulting
  the busy handler, so no `busy_timeout` setting changes the outcome (the
  WARN's ~13 ms `duration_ms` is that immediacy; the boundary is spelled
  out and pinned in `go/dbkit/dialect/sqlite/dialect_sqlite_test.go`'s
  busy-timeout suite). This
  is not the write-capture plugin's same-goroutine self-deadlock shape
  either (that one is a separate limitation recorded in go/dbkit's own
  docs), but it shares that one's property that a busy timeout cannot cure
  it. `go/dbkit`'s
  `dialect/sqlite` factory declares `_pragma=busy_timeout(5000)` explicitly
  on every connection, and the WARN is not eliminated by it: the
  immediate `SQLITE_BUSY` failure is a deterministic property of the
  gate's transaction shape, while whether this flow test actually hits the
  collision is scheduling-dependent -- neither "reproduces on every run"
  nor "gone" is claimable from test runs alone, and a WARN-free run does
  not mean the race is gone. `go/jobs`' own retry/backoff is the working
  convergence mechanism: the losing attempt logs "job attempt failed,
  scheduling retry" and succeeds on its immediate next attempt, so
  `flowtests/smilesim_flow_test.go`'s own assertions still pass
  deterministically. Removing the WARN line itself requires
  `go/storage`-module work on the gate's own transaction shape -- taking
  the write lock first (e.g. `BEGIN IMMEDIATE`) instead of reading then
  upgrading -- or a queue-concurrency change in the reference app's own
  wiring (`internal/app/server.go`'s shared `StandaloneQueue`).
  A retry of `imageGenerateHandler.Handle` itself never re-runs the vendor
  call or double-records usage: `image_job_store.go` claims a job (a
  content-less "pending" row) BEFORE the vendor is ever called rather than
  writing the vendor's answer only after -- see its own doc comment for
  the full mechanism. The claim's primary-key uniqueness serializes two
  overlapping `Handle` calls for the same job, and a retry landing
  mid-`writeImageObject` reuses the vendor's prior answer: the
  `ai_gateway_image_jobs` marker records "the vendor already answered"
  the instant it happens, before `writeImageObject` is even attempted,
  `recordImageUsage` is gated on the marker's own guarded completion so
  usage is reported at most once regardless. The no-second-vendor-call
  invariant holds for every path into a retry of this job, this WARN's
  scenario included -- the specific shape is reproduced by
  `image_gateway_test.go`'s
  `TestImageGenerateHandler_RetryAfterVendorSuccess_DoesNotRecallVendorOrDoubleRecordUsage`.
  Two narrow, explicitly accepted windows remain, both bounded to "the job
  stalls and eventually dead-letters", never "the vendor is billed twice"
  -- see `image_job_store.go`'s own "Accepted residual risk" section for
  the exact shape of each and why closing them further would need either
  an in-process retry loop this codebase deliberately does not use for
  `SQLITE_BUSY`-class contention (see `go/storage/derive.go`'s own doc
  comment) or a lease/staleness mechanism out of proportion with how
  narrow the windows are.
