---
title: ai-gateway
description: "A vendor-agnostic chat and image-generation gateway: provider registries over an OpenAI-compatible default, scope-tiered BYOK credentials, and an async-only image pipeline through jobs and storage."
weight: 2
---

# ai-gateway

ai-gateway is speed's AI gateway: one abstraction layer over every LLM
and image-generation vendor. Business code calls the `Gateway` facade —
synchronous `Chat`/`ChatStream`, or async-only `GenerateImage` — and
never a vendor SDK; providers register behind `ChatProvider`/
`ImageProvider` seams, and tenants bring their own keys through a
scope-tiered credential store.

## What it is for

**The chat surface.** `ChatProvider` (`Chat`/`ChatStream`) is the
vendor-agnostic interface; `OpenAICompatibleProvider` is the
zero-dependency default implemented directly against the
OpenAI-compatible wire schema (stdlib only), and `ChatProviderRegistry`
is where a third-party provider registers its name. `Gateway.Chat`
runs the fixed pipeline: validate → check `Entitlements` (if wired)
*before* anything is resolved, so a refused caller is never billed →
resolve credential → route → provider → call → report usage.
`WithModelRoute(logicalKey, provider, vendorModel)` maps a
caller-facing logical key (e.g. `"chat:default"`) to a concrete
vendor model at construction time; an unrouted key is
`ErrUnroutedModel`, never a silent fallback. Credentials live in one
`ai_gateway_credentials` table — `CredentialScopeSystem` or
`CredentialScopeTenant`, tenant override down to platform default —
with the API key encrypted at rest. **The image surface.**
`ImageProvider` (`TextToImage`/`ImageToImage`/`Inpaint`) mirrors the
chat seam with its own default and registry; `Gateway.GenerateImage`
has *no* synchronous counterpart — it validates, checks entitlements,
enqueues one `jobs` task and returns its `JobID` immediately. The job
handler is the single place storage I/O happens, and the caller reads
the result back through `jobs.Queue.Get`: a succeeded job's
`Result.Data` unmarshals into `ImageJobResult{OutputObjectID, Usage}`.

What it is **not**: it is not an inference service — the module is the
client of an inference endpoint, never the endpoint (a self-hosted
OpenAI-compatible host is a credential configuration, not a new
registration). No dynamic model routing exists (`go/config`-backed
routing is deliberately absent; routing is the construction-time
`WithModelRoute` decision). The credential surface is write-only: no
response ever echoes a key, and no rotation or expiry lifecycle ships.
Per-use charging is deliberately not this module's job — ai-gateway and
billing sit on the same tier and neither imports the other; charging is
host-layer work through `go/billing`'s reserve/confirm lifecycle, as
the reference app's smilesim service demonstrates.

## When to choose it

Your product calls an LLM for chat or completions, or generates images
asynchronously, and you want vendor code behind one facade — with
per-tenant BYOK credentials, an entitlement gate, usage reporting and
SSRF-guarded tenant endpoints. The OpenAI-compatible built-ins already
reach OpenAI, Azure OpenAI, DeepSeek and most self-hosted gateways; a
different vendor protocol is one more `ChatProviderRegistry`/`ImageProviderRegistry`
registration. If you need an image job that returns synchronously,
this is not the module — the async pipeline is the shipped shape.

## Wiring it in

```go
m := aigateway.NewModule(db,
    aigateway.WithModelRoute("chat:default", "chat.openai-compatible", "gpt-4o-mini"),
    aigateway.WithModelRoute("image:smile", "image.openai-compatible", "gpt-image-1"),
    aigateway.WithImageGeneration(queue, objectService), // arms the async pipeline
    aigateway.WithEntitlements(billingEntitlements),     // optional: judge before billing
    aigateway.WithUsageRecorder(meteringRecorder),       // optional: report usage
)
g := m.Gateway()

// chat — req.Model is the logical key, resolved at call time:
resp, err := g.Chat(ctx, req)

// image generation — async only; poll through the queue you wired:
jobID, err := g.GenerateImage(ctx, imageReq)
// ... later, on the worker or a poller:
job, err := queue.Get(ctx, jobID) // StatusSucceeded -> unmarshal Result.Data
var res aigateway.ImageJobResult // OutputObjectID, Usage
_ = json.Unmarshal(job.Result.Data, &res)
```

`WithImageGeneration(queue, objects)` takes `go/jobs`' queue and
`go/storage`'s `ObjectService` directly (both sit below ai-gateway);
a chat-only gateway built without it registers no job handler, and
`GenerateImage` answers `ErrImageGenerationUnavailable`. Routing and
credential namespaces are shared between the two halves — pick
non-colliding logical prefixes (`chat:`, `image:`). Both
`Entitlements` and `UsageRecorder` are structurally-typed, optional
seams satisfied by `billing.EntitlementsService` and
`metering.Recorder` with no import edge. Tenant BYOK credentials are
written over HTTP — `PUT /api/v1/ai-gateway/credentials/{provider}/tenant`
(gated `ai-gateway:write`) or `.../platform`
(`ai-gateway:manage_platform`); reads answer provider/scope/baseUrl
metadata only, never the key.

## Core concepts and API surface

- **The credential table is platform data.** Modeled on `go/config`'s
  `configs` table: empty-string tenant sentinel for the platform row,
  tenant-override resolution order, `AssertNotTenantScoped`. The key
  column is encrypted at rest through a host-registered `dbkit`
  encrypted serializer.
- **The object-reference boundary is drawn at the job handler.**
  `ImageProvider` trades in raw bytes (so a future vendor integration
  needs no storage plumbing); `Gateway`-facing shapes carry only
  `go/storage` object ids, and the handler translates between them.
  A completed job's output is a brand-new object under the request's
  own tenant.
- **SSRF defense is two-stage for the tenant-writable base URL.**
  `ValidateBaseURL` refuses blocked addresses at write time; at call
  time a guarded HTTP client resolves the host once, refuses any
  blocked candidate and dials the validated IP — never a re-resolved
  hostname, so DNS rebinding fails closed. A blocked refusal never
  echoes the resolved address (no internal-DNS oracle); platform-tier
  credentials are deliberately outside the guard — an intranet gateway
  is a legitimate operator default.
- **Retries never re-run the vendor or double-record usage.** The job
  claims a pending row before the vendor is called and records "the
  vendor answered" before the output write; usage is gated on that
  marker, so overlapping `Handle` runs converge on one result.
- **Coded errors** — the SSRF trio `aigateway.base_url_invalid` /
  `aigateway.base_url_unresolvable` / `aigateway.base_url_blocked`,
  `aigateway.entitlement_denied` (403, with model and reason params) and
  the rest — are indexed in the [error code index](/docs/user-guide/error-codes/#aigateway).

## Limitations and links

- The credential write surface is validate-shape-only: no provider
  round-trip and no key-validity check happens at write time.
- A third-party provider that cannot carry the guarded HTTP client is
  refused for tenant-tier credentials (`ErrProviderNotSSRFGuardable`)
  — the host fixes it by routing to a guardable provider or resolving
  at the platform tier.
- No progress reporting during an image job (one vendor call has no
  natural intermediate point), and no surface to discover which logical
  keys are routed short of attempting a call.
- `UsageRecorder` is the analytics-grade, fail-open seam; a host that
  charges for AI usage follows the reference app's smilesim shape
  (reserve credits before enqueueing, settle at the job's terminal
  status, with durable reservation bookkeeping underneath).

### Source

- [go/ai-gateway/AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md) — the authoritative document (provider seams, credential store, async pipeline, SSRF posture, limitations)
- Design rationale: [docs/internal/08-ai-gateway.md](https://github.com/vislake/speed/blob/main/docs/internal/08-ai-gateway.md)
- Related pages: [billing](/docs/user-guide/modules/capabilities/billing/), [metering](/docs/user-guide/modules/services/metering/), [storage](/docs/user-guide/modules/services/storage/), the domain guide [Storage, sharing and AI](/docs/user-guide/domains/storage-sharing-and-ai/)
