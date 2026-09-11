package aigateway

// This file is the image-generation pipeline: Gateway.GenerateImage, the
// async job it enqueues, and the job handler that actually reads storage,
// calls an ImageProvider and writes storage back.
//
// # The object-reference / raw-bytes boundary
//
// Image bytes travel through storage uniformly, and the interface passes
// object references, never byte streams. This package draws that boundary
// at the Gateway/job-handler layer, not inside ImageProvider itself:
//
//   - ImageRequest (Gateway.GenerateImage's own input) and ImageJobResult
//     (what a caller polls back through jobs.Queue.Get) carry ONLY object
//     references -- InputObjectID, MaskObjectID, OutputObjectID -- never a
//     byte; the boundary business code actually touches stays
//     reference-only.
//   - ImageProvider's own three methods (image_types.go) trade in
//     ImageBytes -- raw content plus a MIME type -- mirroring ChatProvider,
//     which trades in ChatMessage content rather than a storage reference
//     of its own. This is a deliberate choice, not an oversight: pkgcore.
//     SeamRegistry[T].Build's Config is a flat map[string]string (the same
//     shape ChatProviderRegistry.Build already uses for base_url/api_key),
//     which cannot carry a live go/storage handle, so an ImageProvider
//     resolved fresh per job the way ChatProviderRegistry resolves fresh
//     per chat call could not reach a storage accessor even if its
//     signature named one. Making every ImageProvider implementation --
//     including every future third-party one -- responsible for its own
//     storage reads and writes would also needlessly couple simple vendor
//     integrations (and their tests) to go/storage; keeping ImageProvider
//     byte-based keeps it exactly as easy to implement and test via
//     httptest.Server as ChatProvider is (see openai_compatible_image.go).
//     The job handler below is the SINGLE place storage I/O actually
//     happens, translating references to bytes before calling the
//     provider and bytes back to a reference afterward.
//
// # Why image generation is enqueue-then-poll, never synchronous
//
// Every image task is asynchronous by default, running through jobs and
// returning a JobID; Gateway.GenerateImage has no synchronous counterpart
// at all -- unlike
// Chat, which is synchronous by default with ChatStream as its only
// async-shaped variant. GenerateImage validates the
// request, checks the Entitlements seam (reused verbatim from the chat
// pipeline, never a second gate), resolves the route and credential once
// to fail fast on an obviously broken configuration, and enqueues one
// jobs.Task -- returning its JobID immediately. Every real provider call,
// every storage read and write, and the usage report all happen inside the
// job handler below, which reruns route and credential resolution fresh
// (a job may execute long after -- and on a different replica than --
// the Gateway.GenerateImage call that enqueued it, and a resolved
// credential can legitimately change in between; this mirrors how
// Gateway.Chat also resolves a provider fresh on every call).
//
// A caller retrieves a completed image job's result through go/jobs' own,
// already-shipped mechanism -- there is no second job-status system here:
// call jobs.Queue.Get(ctx, jobID) and, once Job.Status is
// jobs.StatusSucceeded, json.Unmarshal(job.Result.Data, &ImageJobResult{})
// to read the OutputObjectID and the real ImageUsage the job recorded.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/storage"
)

// TaskTypeImageGenerate is the go/jobs task type Gateway.GenerateImage
// enqueues, and the type Module.Register claims a handler for (via
// reg.Jobs.Handle) whenever a Gateway was built with WithImageGeneration.
const TaskTypeImageGenerate = "ai-gateway.image.generate"

// The Feature dimensions Gateway reports for a successful image-generation
// job -- image count and diffusion steps. Image usage is billed by real
// vendor dimensions (image count, diffusion steps, resolution tier), never
// tokens. Resolution tier is not its own Feature: it is categorical, not a
// quantity, so it travels in UsageEvent.Metadata instead (see
// recordImageUsage) -- UsageEvent's own doc comment already names Metadata
// as the field for exactly this kind of small, bounded call context.
const (
	usageFeatureImageCount = "ai.image_count"
	usageFeatureImageSteps = "ai.image_steps"
)

// ImageJobResult is the JSON shape of jobs.Result.Data for a successfully
// completed TaskTypeImageGenerate job -- see this file's own doc comment
// for how a caller retrieves it.
type ImageJobResult struct {
	// OutputObjectID is the go/storage object id of the generated image, a
	// brand new object under the request's own tenant -- the job handler
	// never overwrites InputObjectID or MaskObjectID.
	OutputObjectID string `json:"output_object_id"`
	// Usage is the real vendor usage ImageProvider reported for this call,
	// the same value recordImageUsage reported to the wired UsageRecorder.
	Usage ImageUsage `json:"usage"`
}

// imageGenerateTaskPayload is TaskTypeImageGenerate's JSON job payload --
// ImageRequest's fields, unpacked so the payload has no method set of its
// own to (de)serialize.
type imageGenerateTaskPayload struct {
	Model         string         `json:"model"`
	Operation     string         `json:"operation"`
	Prompt        string         `json:"prompt"`
	InputObjectID string         `json:"input_object_id,omitempty"`
	MaskObjectID  string         `json:"mask_object_id,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
}

// WithImageProviderRegistry overrides the package-level
// ImageProviderRegistry a Gateway resolves ImageProvider implementations
// from. Tests use this to isolate a Gateway under test from the
// process-global registry's real registrations, mirroring
// WithChatProviderRegistry exactly.
func WithImageProviderRegistry(registry *pkgcore.SeamRegistry[ImageProvider]) GatewayOption {
	return func(g *Gateway) {
		if registry != nil {
			g.imageRegistry = registry
		}
	}
}

// WithImageGeneration wires the two seams Gateway.GenerateImage needs to
// run the async-only image pipeline at all: queue is the jobs.Queue the
// generated task is enqueued on and the module's job handler is registered
// against (Module.Register, via reg.Jobs), and objects is the go/storage
// ObjectService the job handler reads input images from and writes
// generated output images to, always as a brand new object under the
// request's own tenant.
//
// Both go/storage and go/jobs sit below go/ai-gateway in the module
// dependency graph, so importing them directly here is an ordinary
// downward dependency -- unlike Entitlements and UsageRecorder in
// seams.go, which stay structurally-typed no-import seams precisely
// because ai-gateway sits at the SAME tier as billing/metering's own
// consumers and may not import either.
//
// Without this option, Gateway.GenerateImage always fails with
// ErrImageGenerationUnavailable: a Gateway built for chat-only use has no
// reason to wire either seam, exactly like a Gateway that never calls
// WithEntitlements enforces no quota.
func WithImageGeneration(queue jobs.Queue, objects *storage.ObjectService) GatewayOption {
	return func(g *Gateway) {
		g.imageQueue = queue
		g.objectService = objects
	}
}

// resolveImage runs the routing and credential-resolution legs shared by
// GenerateImage (once, to fail fast) and the job handler (again, at
// execution time) -- the image-side mirror of Gateway.resolve.
func (g *Gateway) resolveImage(ctx context.Context, logicalModel string) (ImageProvider, ModelRoute, error) {
	route, ok := g.routes[logicalModel]
	if !ok {
		return nil, ModelRoute{}, ErrUnroutedModel.WithParam("model", logicalModel)
	}

	cred, err := g.credentials.Resolve(ctx, route.Provider)
	if err != nil {
		return nil, route, err
	}

	provider, _, err := g.imageRegistry.Build(route.Provider, pkgcore.Config{
		"base_url": cred.BaseURL,
		"api_key":  cred.APIKey,
	})
	if err != nil {
		return nil, route, fmt.Errorf("aigateway: resolve image provider %q for model %q: %w", route.Provider, logicalModel, err)
	}
	// The identical tenant-tier dial guard Gateway.resolve applies to chat
	// providers applies here -- see that call site's comment and provider_guard.go's
	// file header. It runs in the job worker too (callProvider re-resolves
	// fresh at execution time), so a tenant BYOK image credential is
	// dial-guarded wherever the job executes, on whichever replica -- and an
	// image provider that cannot carry the guarded client is refused with
	// the same coded error, exactly like its chat twin.
	if err := guardTenantScopeDial(provider, cred.Scope); err != nil {
		// The identical decoration Gateway.resolve applies to the guard's
		// coded refusal applies here -- see that call site's comment.
		if appErr, ok := apperr.As(err); ok {
			err = appErr.WithParam("provider", route.Provider).WithParam("model", logicalModel)
		}
		return nil, route, err
	}
	return provider, route, nil
}

// GenerateImage validates req, checks Entitlements (if wired) and resolves
// the route/credential once to fail fast, then enqueues one
// TaskTypeImageGenerate job and returns its JobID immediately -- it never
// calls a provider, reads storage or writes storage itself. See this
// file's own doc comment for the full pipeline and the object-reference
// boundary.
func (g *Gateway) GenerateImage(ctx context.Context, req ImageRequest) (jobs.JobID, error) {
	if err := req.validate(); err != nil {
		return "", err
	}
	logicalModel := req.Model

	// GenerateImage requires a tenant, full stop: every jobs.Task must
	// carry one (jobs.Task.TenantID's own doc comment), and this call is
	// the enqueuing side. The requirement is therefore enforced HERE, in
	// the rate limiter's own pipeline position, so the per-tenant limiter
	// is only ever reached with a real tenant dimension -- never with the
	// empty string. The coded refusal is ErrImageRequiresTenant.
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return "", ErrImageRequiresTenant
	}
	if err = g.checkRateLimit(ctx, string(tenant)); err != nil {
		return "", err
	}

	if err = g.checkEntitlement(ctx, logicalModel); err != nil {
		return "", err
	}

	// Resolved once here purely to fail fast on an unrouted model or a
	// missing credential -- the built provider itself is discarded; the
	// job handler resolves its own, fresh, at execution time (see this
	// file's own doc comment for why).
	if _, _, err = g.resolveImage(ctx, logicalModel); err != nil {
		return "", err
	}

	if g.imageQueue == nil || g.objectService == nil {
		return "", ErrImageGenerationUnavailable
	}

	payload, err := json.Marshal(imageGenerateTaskPayload{
		Model:         logicalModel,
		Operation:     string(req.Operation),
		Prompt:        req.Prompt,
		InputObjectID: req.InputObjectID,
		MaskObjectID:  req.MaskObjectID,
		Params:        req.Params,
	})
	if err != nil {
		return "", fmt.Errorf("aigateway: encode image generation task payload: %w", err)
	}

	jobID, err := g.imageQueue.Enqueue(ctx, jobs.Task{
		Type:     TaskTypeImageGenerate,
		TenantID: tenant,
		Payload:  payload,
	})
	if err != nil {
		return "", err
	}

	obs.FromContext(ctx).Info("aigateway: image generation enqueued",
		"model", logicalModel, "operation", string(req.Operation), "job_id", string(jobID))
	return jobID, nil
}

// readImageObject reads objectID's completed content from go/storage and
// returns it as ImageBytes -- the job handler's translation of an object
// reference into the raw bytes ImageProvider's methods actually trade in.
func (g *Gateway) readImageObject(ctx context.Context, objectID string) (ImageBytes, error) {
	obj, rc, err := g.objectService.OpenContent(ctx, objectID)
	if err != nil {
		return ImageBytes{}, ErrImageObjectUnavailable.WithCause(err).WithParam("object_id", objectID)
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		return ImageBytes{}, ErrImageObjectUnavailable.WithCause(err).WithParam("object_id", objectID)
	}
	mime := ""
	if obj.MIME != nil {
		mime = *obj.MIME
	}
	return ImageBytes{Content: raw, MIME: mime}, nil
}

// writeImageObject writes img into go/storage as a brand new object of the
// caller's own tenant (Create always mints a fresh id, so this can never
// overwrite an input or mask object) and runs it through the ordinary
// Create/Upload/Complete transfer lifecycle -- the same pipeline any other
// storage consumer's upload goes through, including go/storage's own
// revalidation and thumbnail-derive enqueue for the image types it
// recognizes.
func (g *Gateway) writeImageObject(ctx context.Context, img ImageBytes) (string, error) {
	size := int64(len(img.Content))
	created, err := g.objectService.Create(ctx, storage.CreateParams{
		DeclaredSize: size,
		DeclaredType: img.MIME,
	})
	if err != nil {
		return "", ErrImageOutputWriteFailed.WithCause(err)
	}
	if uploadErr := g.objectService.Upload(ctx, created.ID, &size, bytes.NewReader(img.Content)); uploadErr != nil {
		return "", ErrImageOutputWriteFailed.WithCause(uploadErr)
	}
	completed, err := g.objectService.Complete(ctx, created.ID)
	if err != nil {
		return "", ErrImageOutputWriteFailed.WithCause(err)
	}
	return completed.ID, nil
}

// recordImageUsage reports usage to the wired UsageRecorder under the
// image billing dimensions, reusing UsageEvent/UsageRecorder verbatim --
// the shape is generic enough for images, since Feature/Quantity/Metadata
// carry the image dimensions unchanged (see seams.go's own doc comment).
// It is a no-op when no UsageRecorder is wired or ctx carries no tenant,
// exactly like Gateway.recordUsage for chat.
//
// jobID is the owning Job's stable id (see imageUsageIdempotencyKey's own
// doc comment for why this, unlike chat's recordUsage, can derive a stable
// IdempotencyKey at all). imageGenerateHandler.Handle calls this at most
// once per job -- see image_job_store.go's markCompleted -- so jobID's
// only purpose here is the key's stability across a UsageRecorder's own
// dedup, defense in depth rather than the enforcement point itself.
func (g *Gateway) recordImageUsage(ctx context.Context, logicalModel, jobID string, usage ImageUsage) {
	if g.usage == nil {
		return
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return
	}

	metadata := map[string]string{"model": logicalModel}
	if usage.ResolutionTier != "" {
		metadata["resolution_tier"] = usage.ResolutionTier
	}
	if usage.ImageCount > 0 {
		g.reportImageUsageEvent(ctx, tenant, jobID, usageFeatureImageCount, float64(usage.ImageCount), metadata)
	}
	if usage.Steps > 0 {
		g.reportImageUsageEvent(ctx, tenant, jobID, usageFeatureImageSteps, float64(usage.Steps), metadata)
	}
}

// reportImageUsageEvent records one UsageEvent for one image billing
// dimension. A Record failure is logged and swallowed, never failing the
// image job that already succeeded -- the identical rule
// Gateway.recordUsage's own doc comment states for chat.
func (g *Gateway) reportImageUsageEvent(ctx context.Context, tenant pkgcore.TenantID, jobID, feature string, quantity float64, metadata map[string]string) {
	event := UsageEvent{
		TenantID:       string(tenant),
		Feature:        feature,
		Quantity:       quantity,
		IdempotencyKey: imageUsageIdempotencyKey(jobID, feature),
		Metadata:       metadata,
	}
	if err := g.usage.Record(ctx, event); err != nil {
		obs.FromContext(ctx).Warn("aigateway: image usage recording failed",
			"feature", feature, "error", err)
	}
}

// imageUsageIdempotencyKey derives a stable UsageEvent.IdempotencyKey for
// one image-generation Job's one billing dimension -- unlike chat's
// newIdempotencyKey (gateway.go), an image job carries a stable identity
// across every retry (jobs.Job.ID never changes between attempts of the
// same Job -- see jobs.Job.ID's own doc comment), so this package CAN
// derive a deterministic key here where it cannot for chat. Keyed on both
// jobID and feature (never jobID alone): recordImageUsage reports up to two
// dimensions (image_count, steps) per job, and a UsageRecorder that treats
// IdempotencyKey as identifying one event must not see the two dimensions
// of the same job collide under one key. This is the identical
// derived-not-random-key convention go/storage's own
// thumbnailDeriveIdempotencyKey and expirySweepIdempotencyKey already
// follow for their own jobs.Task.IdempotencyKey.
func imageUsageIdempotencyKey(jobID, feature string) string {
	return "aigateway.image_usage:" + jobID + ":" + feature
}

// imageJobHandler returns the jobs.Handler Module.Register claims on
// reg.Jobs, and whether image generation is wired at all -- ok is false
// when GenerateImage was never given WithImageGeneration, in which case
// Module.Register registers nothing rather than a handler that could never
// run (see module.go).
func (g *Gateway) imageJobHandler() (jobs.Handler, bool) {
	if g.imageQueue == nil || g.objectService == nil {
		return nil, false
	}
	return &imageGenerateHandler{gateway: g}, true
}

// imageGenerateHandler is the jobs.Handler claiming TaskTypeImageGenerate,
// the task Gateway.GenerateImage enqueues. Its Handle is where every real
// piece of work happens: reading storage, calling the resolved
// ImageProvider, writing storage, and reporting usage.
type imageGenerateHandler struct {
	gateway *Gateway
}

// Type implements jobs.Handler.
func (h *imageGenerateHandler) Type() string { return TaskTypeImageGenerate }

// Handle implements jobs.Handler. ctx already carries the job's tenant via
// pkgcore.WithTenant, rebuilt by the queue worker from job.TenantID before
// this call -- never inherited from whatever context the original
// GenerateImage call ran in, which may no longer exist by the time a
// worker picks the job up (jobs.Handler.Handle's own doc comment).
//
// A payload that fails to decode, names no model/prompt, or names an
// operation this package does not know is a task-shape violation: it fails
// this attempt and can never succeed by re-running, so the queue's retry
// policy will eventually dead-letter it -- mirroring
// go/storage/derive.go's deriveHandler.Handle's identical stance on a
// malformed task payload.
//
// Handle's own idempotency invariant (see image_job_store.go's own doc
// comment for the full mechanism): for one enqueued Job, at most one
// successful ImageProvider call and at
// most one usage record ever reach the outside world, no matter how many
// times go/jobs re-runs this method for it, or how many overlapping calls
// ever run for the same Job at once -- and every Handle call answers with
// the marker's own OutputObjectID: an attempt that loses the completion
// race to a concurrent winner returns the winner's id from the marker row,
// never a fresh orphan object id of its own. The FIRST thing every attempt
// does --
// before decoding is even relevant to the invariant, but genuinely before
// anything that could call the vendor -- is settle "has an earlier attempt
// already gotten a successful answer for this exact job, or claimed it and
// not yet answered" by consulting h.gateway.imageJobs, never the reverse;
// a job with no prior attempt claims it (imageJobRepository.claimPending)
// BEFORE calling the vendor, not after, which is what makes both a
// transient claim-write failure and two overlapping Handle calls for the
// same job safe -- see image_job_store.go's own doc comment for why.
func (h *imageGenerateHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var payload imageGenerateTaskPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("aigateway: undecodable image-generate task payload: %w", err)
	}
	req := ImageRequest{
		Model:         payload.Model,
		Operation:     ImageOperation(payload.Operation),
		Prompt:        payload.Prompt,
		InputObjectID: payload.InputObjectID,
		MaskObjectID:  payload.MaskObjectID,
		Params:        payload.Params,
	}
	if err := req.validate(); err != nil {
		return jobs.Result{}, err
	}

	jobID := string(job.ID)

	// Settle "already done" BEFORE anything that could call the vendor --
	// see this method's own doc comment and image_job_store.go.
	marker, err := h.gateway.imageJobs.get(ctx, jobID)
	if err != nil {
		return jobs.Result{}, err
	}

	var img ImageBytes
	var usage ImageUsage
	var providerName string

	switch {
	case marker != nil && marker.Status == imageJobStatusCompleted:
		// The whole job already finished on an earlier attempt (a retry
		// after markCompleted committed, or an at-least-once redelivery of
		// an already-succeeded Job): answer from the row alone. No vendor
		// call, no storage write, no usage record -- reported once,
		// already, when this row was first written.
		return completedMarkerResult(marker)

	case marker != nil && marker.Status == imageJobStatusGenerated:
		// An earlier attempt already got a successful vendor answer but
		// did not finish writing it to storage. Reuse that answer verbatim
		// -- never call the vendor again, and never re-read the input/mask
		// objects, which this reused answer no longer needs.
		img = marker.image()
		usage = marker.usage()
		providerName = marker.Provider

	case marker != nil && marker.Status == imageJobStatusPending:
		// A claim already exists for this job with no vendor answer
		// recorded against it -- either a genuinely concurrent Handle call
		// for this job is running right now, or an earlier attempt
		// crashed before ever calling the vendor. Either way this attempt
		// must not call it: see image_job_store.go's own doc comment for
		// why a "pending" row is never resurrected here.
		obs.FromContext(ctx).Warn("aigateway: image job claim already pending, refusing to call the vendor again",
			"job_id", jobID)
		return jobs.Result{}, ErrImageJobClaimInFlight.WithParam("job_id", jobID)

	default: // marker == nil: no attempt has reached the vendor yet.
		claimed, claimErr := h.gateway.imageJobs.claimPending(ctx, jobID)
		if claimErr != nil {
			// No vendor call has happened yet, so surfacing this error --
			// including the exact transient contention image_job_store.go's
			// own doc comment discusses -- as an ordinary attempt failure
			// is always safe: a retry redoes the claim from a clean slate.
			return jobs.Result{}, claimErr
		}
		if !claimed {
			// Lost the race to claim this job: a concurrent attempt's own
			// INSERT committed first.
			return jobs.Result{}, ErrImageJobClaimInFlight.WithParam("job_id", jobID)
		}

		result, resolvedProvider, callErr := h.callProvider(ctx, req)
		if callErr != nil {
			// No successful vendor answer exists to remember -- release
			// the claim so the next attempt starts from a clean slate
			// instead of being permanently refused by
			// ErrImageJobClaimInFlight (image_job_store.go's own doc
			// comment on releaseClaim).
			if relErr := h.gateway.imageJobs.releaseClaim(ctx, jobID); relErr != nil {
				obs.FromContext(ctx).Warn("aigateway: failed to release image job claim after a failed attempt",
					"job_id", jobID, "error", relErr)
			}
			return jobs.Result{}, callErr
		}
		providerName = resolvedProvider

		// The vendor call just succeeded. Persist that fact, verbatim,
		// BEFORE attempting anything that could still fail -- this write,
		// not the eventual storage write, is what closes the
		// double-billing window (image_job_store.go's own doc comment).
		if markErr := h.gateway.imageJobs.markGenerated(ctx, jobID, providerName, result.Image, result.Usage); markErr != nil {
			return jobs.Result{}, markErr
		}
		img = result.Image
		usage = result.Usage
	}

	outputObjectID, err := h.gateway.writeImageObject(ctx, img)
	if err != nil {
		// The marker (from either branch above) stays at
		// imageJobStatusGenerated: the next attempt skips the vendor call
		// and redoes only this write.
		return jobs.Result{}, err
	}

	completed, err := h.gateway.imageJobs.markCompleted(ctx, jobID, outputObjectID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !completed {
		// Lost markCompleted's guarded transition: another attempt's
		// transition committed first, so the marker now durably names that
		// attempt's output object, and THIS attempt's freshly written one
		// is the accepted orphan image_job_store.go's own doc comment
		// describes. Answer from the marker row alone -- the same answer
		// every later redelivery will give -- so two Handle runs for one
		// job always agree on the OutputObjectID the caller reads from
		// Job.Result, never this attempt's own brand-new id. No usage
		// record here: the winning attempt's recordImageUsage, gated on the
		// same guarded transition, already reported this job exactly once.
		marker, getErr := h.gateway.imageJobs.get(ctx, jobID)
		if getErr != nil {
			// The job did succeed (the marker is completed); surfacing this
			// read failure lets go/jobs retry, converging on Handle's own
			// completed short-circuit above.
			return jobs.Result{}, getErr
		}
		if marker == nil || marker.Status != imageJobStatusCompleted {
			return jobs.Result{}, ErrInternal.WithParam("job_id", jobID).
				WithParam("reason", "markCompleted lost its transition but no completed marker row exists")
		}
		return completedMarkerResult(marker)
	}
	// Gated on markCompleted's own guarded transition, not merely on
	// having reached this line: this is what makes "at most one usage
	// record ever" true even if this exact line somehow ran twice for
	// one job (image_job_store.go's markCompleted doc comment).
	h.gateway.recordImageUsage(ctx, req.Model, jobID, usage)

	obs.FromContext(ctx).Info("aigateway: image generated",
		"model", req.Model,
		"provider", providerName,
		"operation", string(req.Operation),
		"output_object_id", outputObjectID,
		"image_count", usage.ImageCount,
		"steps", usage.Steps,
		"resolution_tier", usage.ResolutionTier,
	)

	resultData, err := json.Marshal(ImageJobResult{OutputObjectID: outputObjectID, Usage: usage})
	if err != nil {
		return jobs.Result{}, fmt.Errorf("aigateway: encode image generation job result: %w", err)
	}
	return jobs.Result{Data: resultData}, nil
}

// completedMarkerResult renders the jobs.Result a completed marker row
// answers with: the row's own OutputObjectID and the ImageUsage it
// recorded, nothing else. It is the shared answer shape of Handle's
// completed short-circuit at the top and of an attempt that lost
// markCompleted's guarded transition to a concurrent winner -- the two
// paths on which a Handle call must report a job it did not itself finish.
func completedMarkerResult(marker *imageJobRow) (jobs.Result, error) {
	resultData, marshalErr := json.Marshal(ImageJobResult{OutputObjectID: marker.OutputObjectID, Usage: marker.usage()})
	if marshalErr != nil {
		return jobs.Result{}, fmt.Errorf("aigateway: encode image generation job result: %w", marshalErr)
	}
	return jobs.Result{Data: resultData}, nil
}

// callProvider resolves the route/credential for req.Model, reads any
// input/mask objects the operation needs, and dispatches to the resolved
// ImageProvider's matching method -- the "everything that must succeed
// before a claimed job can be marked generated" bundle, isolated from
// Handle's own claim/release bookkeeping so a failure anywhere in it takes
// the identical releaseClaim path.
func (h *imageGenerateHandler) callProvider(ctx context.Context, req ImageRequest) (ImageResult, string, error) {
	provider, route, resolveErr := h.gateway.resolveImage(ctx, req.Model)
	if resolveErr != nil {
		return ImageResult{}, "", resolveErr
	}

	var input, mask *ImageBytes
	if req.InputObjectID != "" {
		b, readErr := h.gateway.readImageObject(ctx, req.InputObjectID)
		if readErr != nil {
			return ImageResult{}, route.Provider, readErr
		}
		input = &b
	}
	if req.MaskObjectID != "" {
		b, readErr := h.gateway.readImageObject(ctx, req.MaskObjectID)
		if readErr != nil {
			return ImageResult{}, route.Provider, readErr
		}
		mask = &b
	}

	// aigateway.provider.* (metrics.go): the image operations' provider
	// invocations all pass through this one choke point (the async job
	// path), so calls/errors/duration are recorded here under the job's
	// resolved provider -- never at GenerateImage's enqueue site, which
	// is not a provider invocation.
	start := time.Now()
	var result ImageResult
	var err error
	switch req.Operation {
	case ImageOperationTextToImage:
		result, err = provider.TextToImage(ctx, TextToImageRequest{Model: route.VendorModel, Prompt: req.Prompt, Params: req.Params})
	case ImageOperationImageToImage:
		result, err = provider.ImageToImage(ctx, ImageToImageRequest{Model: route.VendorModel, Prompt: req.Prompt, Input: *input, Params: req.Params})
	case ImageOperationInpaint:
		result, err = provider.Inpaint(ctx, InpaintRequest{Model: route.VendorModel, Prompt: req.Prompt, Input: *input, Mask: *mask, Params: req.Params})
	default:
		// Unreachable: req.validate(), called by Handle before this method
		// is ever reached, already refused any operation not among the
		// three ImageOperation constants.
		return ImageResult{}, route.Provider, ErrInvalidImageOperation.WithParam("operation", string(req.Operation))
	}
	h.gateway.recordProviderCall(ctx, route.Provider, providerCallImage, start, err)
	return result, route.Provider, err
}

// compile-time check that *imageGenerateHandler satisfies jobs.Handler.
var _ jobs.Handler = (*imageGenerateHandler)(nil)
