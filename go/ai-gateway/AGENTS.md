# ai-gateway

Round 1 shipped `docs/internal/08-ai-gateway.md`'s unified-abstraction-layer
section: a chat-only LLM gateway. Round 2 (this round) ships the design
doc's multi-modal-expansion section: `ImageProvider`, the async-only
`Gateway.GenerateImage` pipeline, and the go/storage + go/jobs integration
round 1 deliberately left unbuilt. Nothing from round 1 changed except the
`Gateway` struct gaining new fields and `Module.Register` gaining one
conditional job-handler claim -- see "What round 2 adds" below.

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
  it just enforces no quota and reports no usage anywhere -- a real
  production deployment always wires both.
- `Module` (`module.go`): implements `pkgcore.Module`. This round declares
  nothing on the registry (`Register` is a no-op) -- no HTTP surface, no
  permission, no config item, no notification type, no domain event. What it
  contributes is the migrations (the `ai_gateway_credentials` table) and the
  `Gateway`/`CredentialService` accessors a host wires directly.

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
  value verbatim, anything else JSON-encoded). Every response is decoded
  from `{"data": [{"b64_json": "..."}], "usage": {...}}`; the generated
  bytes' MIME is detected from the decoded bytes themselves via
  `http.DetectContentType`, never trusted from a vendor-supplied field --
  the same "probe, never trust a header" discipline go/storage's own
  revalidation pipeline applies. A response with no `usage` object falls
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

## Reference-app consumer

`examples/reference-app/internal/consult` remains round 1's mandatory first
consumer: a small, non-HTTP-generated Go service that, given a note's text,
asks `gateway.Chat()` for a short AI-generated consultation-suggestion
summary under the logical model key `"chat:default"`. It is mounted as a
hand-written route (`POST /api/v1/consult/suggest`), outside the OpenAPI
machinery -- the same pattern the demo module's own hand-written patient-
message route already establishes in this app, since `ai-gateway` itself
ships no HTTP surface for the reference app's notes-spec fragment to grow
into. `cmd/server/consult_flow_test.go` drives it through the composed HTTP
stack against an `httptest.Server` standing in for the OpenAI-compatible
endpoint -- scripted, deterministic responses, no live API key required or
used.

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

## Known limitations / deferred

- No dynamic (`go/config`-backed) model routing -- construction-time
  `WithModelRoute` options only, a deliberate round-1 choice (see
  `route.go`'s doc comment); a later round may add a config-driven layer on
  top without changing `ModelRoute`'s shape. This applies to image routes
  exactly as it does to chat ones -- the two share one mechanism.
- No admin/HTTP surface for writing platform or tenant BYOK credentials --
  `CredentialService.SetPlatformCredential` / `SetTenantCredential` are Go
  APIs a host or a future admin-console round wires into its own surface,
  for chat and image credentials alike.
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
