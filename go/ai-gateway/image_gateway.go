package aigateway

// This file is round 2's image-generation pipeline: Gateway.GenerateImage,
// the async job it enqueues, and the job handler that actually reads
// storage, calls an ImageProvider and writes storage back.
//
// # The object-reference / raw-bytes boundary
//
// docs/internal/08-ai-gateway.md's multi-modal-expansion section states
// that image bytes travel through storage uniformly and the interface
// passes object references, never byte streams. This package draws that
// boundary at the Gateway/job-handler layer, not inside ImageProvider
// itself:
//
//   - ImageRequest (Gateway.GenerateImage's own input) and ImageJobResult
//     (what a caller polls back through jobs.Queue.Get) carry ONLY object
//     references -- InputObjectID, MaskObjectID, OutputObjectID -- never a
//     byte, exactly satisfying the design doc's rule at the boundary
//     business code actually touches.
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
// Per the design doc's explicit rule that every image task is
// asynchronous by default, running through jobs and returning a JobID,
// Gateway.GenerateImage has no synchronous counterpart at all -- unlike
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

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"
)

// TaskTypeImageGenerate is the go/jobs task type Gateway.GenerateImage
// enqueues, and the type Module.Register claims a handler for (via
// reg.Jobs.Handle) whenever a Gateway was built with WithImageGeneration.
const TaskTypeImageGenerate = "ai-gateway.image.generate"

// The Feature dimensions Gateway reports for a successful image-generation
// job -- image count and diffusion steps, per the design doc's explicit
// rule that image usage is billed by real vendor dimensions (image count,
// diffusion steps, resolution tier), never tokens. Resolution tier is not
// its own Feature: it is categorical, not a quantity, so it travels in
// UsageEvent.Metadata instead (see recordImageUsage) -- UsageEvent's own
// doc comment already
// names Metadata as the field for exactly this kind of small, bounded
// call context.
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
// run the design doc's async-only image pipeline at all: queue is the
// jobs.Queue the generated task is enqueued on and the module's job
// handler is registered against (Module.Register, via reg.Jobs), and
// objects is the go/storage ObjectService the job handler reads input
// images from and writes generated output images to, always as a brand
// new object under the request's own tenant.
//
// Both go/storage and go/jobs sit below go/ai-gateway in root CLAUDE.md's
// module dependency graph ("... -> config/jobs -> storage/notification ->
// ... -> billing/ai-gateway/... "), so importing them directly here is an
// ordinary downward dependency -- unlike Entitlements and UsageRecorder in
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

	tenant, _ := pkgcore.TenantFromContext(ctx)
	if err := g.checkRateLimit(ctx, string(tenant)); err != nil {
		return "", err
	}

	if err := g.checkEntitlement(ctx, logicalModel); err != nil {
		return "", err
	}

	// Resolved once here purely to fail fast on an unrouted model or a
	// missing credential -- the built provider itself is discarded; the
	// job handler resolves its own, fresh, at execution time (see this
	// file's own doc comment for why).
	if _, _, err := g.resolveImage(ctx, logicalModel); err != nil {
		return "", err
	}

	if g.imageQueue == nil || g.objectService == nil {
		return "", ErrImageGenerationUnavailable
	}

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return "", ErrImageRequiresTenant
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
// see seams.go's own doc comment: the shape needed NO change at all for
// images, since Feature/Quantity/Metadata were already generic. It is a
// no-op when no UsageRecorder is wired or ctx carries no tenant, exactly
// like Gateway.recordUsage for chat.
func (g *Gateway) recordImageUsage(ctx context.Context, logicalModel string, usage ImageUsage) {
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
		g.reportImageUsageEvent(ctx, tenant, usageFeatureImageCount, float64(usage.ImageCount), metadata)
	}
	if usage.Steps > 0 {
		g.reportImageUsageEvent(ctx, tenant, usageFeatureImageSteps, float64(usage.Steps), metadata)
	}
}

// reportImageUsageEvent records one UsageEvent for one image billing
// dimension. A Record failure is logged and swallowed, never failing the
// image job that already succeeded -- the identical rule
// Gateway.recordUsage's own doc comment states for chat.
func (g *Gateway) reportImageUsageEvent(ctx context.Context, tenant pkgcore.TenantID, feature string, quantity float64, metadata map[string]string) {
	event := UsageEvent{
		TenantID:       string(tenant),
		Feature:        feature,
		Quantity:       quantity,
		IdempotencyKey: newIdempotencyKey(),
		Metadata:       metadata,
	}
	if err := g.usage.Record(ctx, event); err != nil {
		obs.FromContext(ctx).Warn("aigateway: image usage recording failed",
			"feature", feature, "error", err)
	}
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
// worker picks the job up (jobs.Handler.Handle's own doc comment, and root
// CLAUDE.md's "workers do not inherit tenant context" trap).
//
// A payload that fails to decode, names no model/prompt, or names an
// operation this package does not know is a task-shape violation: it fails
// this attempt and can never succeed by re-running, so the queue's retry
// policy will eventually dead-letter it -- mirroring
// go/storage/derive.go's deriveHandler.Handle's identical stance on a
// malformed task payload.
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

	provider, route, err := h.gateway.resolveImage(ctx, req.Model)
	if err != nil {
		return jobs.Result{}, err
	}

	var input, mask *ImageBytes
	if req.InputObjectID != "" {
		b, readErr := h.gateway.readImageObject(ctx, req.InputObjectID)
		if readErr != nil {
			return jobs.Result{}, readErr
		}
		input = &b
	}
	if req.MaskObjectID != "" {
		b, readErr := h.gateway.readImageObject(ctx, req.MaskObjectID)
		if readErr != nil {
			return jobs.Result{}, readErr
		}
		mask = &b
	}

	var result ImageResult
	switch req.Operation {
	case ImageOperationTextToImage:
		result, err = provider.TextToImage(ctx, TextToImageRequest{Model: route.VendorModel, Prompt: req.Prompt, Params: req.Params})
	case ImageOperationImageToImage:
		result, err = provider.ImageToImage(ctx, ImageToImageRequest{Model: route.VendorModel, Prompt: req.Prompt, Input: *input, Params: req.Params})
	case ImageOperationInpaint:
		result, err = provider.Inpaint(ctx, InpaintRequest{Model: route.VendorModel, Prompt: req.Prompt, Input: *input, Mask: *mask, Params: req.Params})
	default:
		// Unreachable: req.validate() above already refused any operation
		// not among the three ImageOperation constants.
		return jobs.Result{}, ErrInvalidImageOperation.WithParam("operation", payload.Operation)
	}
	if err != nil {
		return jobs.Result{}, err
	}

	outputObjectID, err := h.gateway.writeImageObject(ctx, result.Image)
	if err != nil {
		return jobs.Result{}, err
	}

	obs.FromContext(ctx).Info("aigateway: image generated",
		"model", req.Model,
		"provider", route.Provider,
		"operation", string(req.Operation),
		"output_object_id", outputObjectID,
		"image_count", result.Usage.ImageCount,
		"steps", result.Usage.Steps,
		"resolution_tier", result.Usage.ResolutionTier,
	)
	h.gateway.recordImageUsage(ctx, req.Model, result.Usage)

	resultData, err := json.Marshal(ImageJobResult{OutputObjectID: outputObjectID, Usage: result.Usage})
	if err != nil {
		return jobs.Result{}, fmt.Errorf("aigateway: encode image generation job result: %w", err)
	}
	return jobs.Result{Data: resultData}, nil
}

// compile-time check that *imageGenerateHandler satisfies jobs.Handler.
var _ jobs.Handler = (*imageGenerateHandler)(nil)
