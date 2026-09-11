package aigateway

import (
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the ai-gateway module. Every exported error is an
// *apperr.Error builder: match a decorated error with apperr.As(err) and
// compare its Code -- the same convention config and dbkit document. A call
// that returns an error decorated with WithParam or WithCause derives a new
// *apperr.Error, so never compare a once-returned error against a var here
// with == or errors.Is.

var (
	// ErrEmptyModel reports a ChatRequest whose Model (the logical model
	// key) is empty.
	ErrEmptyModel = apperr.Invalid("aigateway.empty_model")

	// ErrEmptyMessages reports a ChatRequest with no messages, or a message
	// whose Content is empty.
	ErrEmptyMessages = apperr.Invalid("aigateway.empty_messages")

	// ErrUnroutedModel reports a logical model key with no ModelRoute
	// declared for it. Routing an unknown key never falls back to some
	// default provider -- see WithModelRoute's doc comment.
	ErrUnroutedModel = apperr.NotFound("aigateway.unrouted_model")

	// ErrEntitlementDenied reports an Entitlements.Check call that answered
	// Allowed: false. It is returned before the credential is resolved or
	// the provider is called, so a refused caller is never billed.
	ErrEntitlementDenied = apperr.Forbidden("aigateway.entitlement_denied")

	// ErrRateLimitCheckFailed reports that Gateway's per-tenant rate-limit
	// check (ratelimit.go's checkRateLimit) could not get an answer from a
	// wired Limiter -- a KVStore outage, never a genuine over-limit
	// decision (see ErrRateLimited for that). Fails the call closed rather
	// than silently allowing it through -- this module's own rule for a
	// wired-but-failing limiter, for the reason checkRateLimit's doc
	// comment (ratelimit.go) records.
	ErrRateLimitCheckFailed = apperr.Internal("aigateway.rate_limit_check_failed")

	// ErrCredentialNotFound reports that CredentialService.Resolve found
	// neither a tenant BYOK row nor a platform-wide row for a provider.
	ErrCredentialNotFound = apperr.NotFound("aigateway.credential_not_found")

	// ErrCredentialRequired reports a Set call whose provider or api key is
	// empty -- a credential with no key is never storable, and the empty
	// string is not a value a caller can be trying to store on purpose.
	ErrCredentialRequired = apperr.Invalid("aigateway.credential_required")

	// ErrTenantScopeRequiresTenant reports SetTenantCredential on a context
	// that carries no tenant. The owning tenant always comes from the
	// context -- never from a caller-supplied identifier -- so a
	// tenant-scoped write on an unscoped context fails closed. The cause is
	// pkgcore.ErrNoTenant.
	ErrTenantScopeRequiresTenant = apperr.Invalid("aigateway.tenant_scope_requires_tenant").
					WithCause(pkgcore.ErrNoTenant)

	// ErrSystemScopeRequiresSystemContext reports SetPlatformCredential on a
	// context that carries no audited system reason. Platform-wide
	// credentials are only writable through the audited system-context path
	// (pkgcore.WithSystemContext, or tenancy.WithSystemContext, which adds
	// its own audit event).
	ErrSystemScopeRequiresSystemContext = apperr.Forbidden("aigateway.system_scope_requires_system_context")

	// ErrBaseURLInvalid reports a tenant BYOK credential write whose baseURL
	// is malformed, names no host, or uses a scheme other than http/https.
	// WithParam("reason", ...) names which -- "unparseable", "missing_host"
	// or "scheme" (the latter carrying the offending scheme in its own
	// WithParam("scheme", ...)). See provider_guard.go's ValidateBaseURL.
	ErrBaseURLInvalid = apperr.Invalid("aigateway.base_url_invalid")

	// ErrBaseURLUnresolvable reports a tenant BYOK credential write whose
	// baseURL host could not be resolved to any address at all --
	// WithParam("host", ...) names the host. See provider_guard.go's ValidateBaseURL.
	ErrBaseURLUnresolvable = apperr.Invalid("aigateway.base_url_unresolvable")

	// ErrBaseURLBlocked reports a tenant BYOK credential write whose baseURL
	// names a private, loopback, link-local, multicast or otherwise
	// never-a-legitimate-vendor-destination address -- the SSRF refusal
	// that keeps one tenant from turning the platform's own network into a
	// request source pointed at the platform's intranet. See
	// provider_guard.go's ValidateBaseURL and the file header there for why
	// this is the tenant-scope write's check and why the platform-scope
	// write deliberately skips it.
	//
	// WithParam("ip", ...) is deliberately asymmetric between the two paths
	// that raise this code, and the asymmetry is a load-bearing invariant a
	// refactor must not flatten:
	//
	//   - The literal-IP path (provider_guard.go, ValidateBaseURL) carries the blocked
	//     address: the caller typed it into the URL, so the param is an
	//     echo of what the caller already knows -- zero disclosure -- and
	//     genuinely useful diagnostics naming exactly which address was
	//     refused.
	//   - The resolution path (a hostname whose DNS answer is blocked)
	//     deliberately carries no ip param: the resolved address is
	//     information the caller does not have -- for a name resolvable only
	//     inside the platform's own network, exactly the answer an
	//     internal-DNS reconnaissance oracle would give -- so echoing it
	//     back would let a tenant admin submit hostnames and read back the
	//     internal IPs they resolve to. The coded error, whose generic
	//     base_url_blocked rendering names no address, is the honest answer
	//     shape for that path.
	ErrBaseURLBlocked = apperr.Invalid("aigateway.base_url_blocked")

	// ErrProviderRequestFailed reports a transport-level failure calling a
	// ChatProvider's upstream vendor endpoint: a network error, a
	// non-2xx HTTP status, or a stream that ended in an I/O error before
	// its final chunk arrived.
	ErrProviderRequestFailed = apperr.Internal("aigateway.provider_request_failed")

	// ErrProviderResponseInvalid reports a response (or one streamed chunk)
	// from a ChatProvider's upstream vendor endpoint that could not be
	// parsed as the expected wire shape.
	ErrProviderResponseInvalid = apperr.Internal("aigateway.provider_response_invalid")

	// ErrProviderConfigInvalid reports Gateway.Chat/ChatStream/GenerateImage
	// resolving a route whose stored credential cannot build the routed
	// provider implementation. The module's own OpenAI-compatible providers
	// (chat and image) both require a non-empty base_url, so a credential
	// stored without one -- legal to store: the write API documents an
	// omitted baseUrl as "leave the provider's own default in effect", a
	// contract no provider in this module actually offers -- is a
	// misconfiguration no call can ever succeed with. The refusal is
	// declared at registry-constructor time, where the provider's own
	// config needs are known, and classified Invalid with this code so a
	// caller can tell "fix the stored credential" apart from a genuine
	// provider/vendor failure -- never an uncoded error a transport layer
	// must fold into a bare internal failure. The cause is
	// pkgcore.ErrMissingSeamConfig, so errors.Is-based registry callers keep
	// recognizing the refusal.
	ErrProviderConfigInvalid = apperr.Invalid("aigateway.provider_config_invalid").
					WithCause(pkgcore.ErrMissingSeamConfig)

	// ErrProviderNotSSRFGuardable reports Gateway.Chat/ChatStream/GenerateImage
	// resolving a TENANT-tier credential to a provider that cannot be
	// SSRF-guarded -- a provider not implementing this module's unexported
	// httpClientSettable, which today means a third-party registration into
	// ChatProviderRegistry/ImageProviderRegistry rather than one of the two
	// module-native OpenAI-compatible built-ins. A tenant-scope credential
	// is the caller's own influence over where the platform dials (provider_guard.go's
	// file header), and only a provider carrying the guarded client has the
	// dial-time re-check that defeats DNS rebinding; the alternative --
	// silently letting the tenant-influenced dial go out on the provider's
	// own unguarded client -- is exactly the write-time-validation-only
	// state provider_guard.go exists to close. guardTenantScopeDial therefore refuses
	// the combination at resolve time with this coded error, and the host's
	// fix is a composition decision: route the logical model to a guardable
	// provider, or let this provider resolve at the platform tier (the
	// operator's own default, outside the guard by scope boundary, never by
	// gap). Classified Invalid like ErrProviderConfigInvalid, since the
	// stored credential itself is well-formed and the call is refused for a
	// combination reason. Resolve-time decoration adds the provider and
	// model params (gateway.go's resolve / image_gateway.go's resolveImage).
	ErrProviderNotSSRFGuardable = apperr.Invalid("aigateway.provider_not_ssrf_guardable")

	// ErrEmptyPrompt reports an ImageRequest whose Prompt is empty. Every
	// ImageOperation requires one.
	ErrEmptyPrompt = apperr.Invalid("aigateway.empty_prompt")

	// ErrInvalidImageOperation reports an ImageRequest.Operation that is
	// none of the declared ImageOperation constants.
	ErrInvalidImageOperation = apperr.Invalid("aigateway.invalid_image_operation")

	// ErrImageInputRequired reports an ImageOperationImageToImage or
	// ImageOperationInpaint request whose InputObjectID is empty.
	ErrImageInputRequired = apperr.Invalid("aigateway.image_input_required")

	// ErrImageMaskRequired reports an ImageOperationInpaint request whose
	// MaskObjectID is empty.
	ErrImageMaskRequired = apperr.Invalid("aigateway.image_mask_required")

	// ErrImageInputNotAllowed reports an ImageRequest carrying
	// InputObjectID or MaskObjectID fields its own Operation does not use
	// -- for example a MaskObjectID on an ImageOperationImageToImage
	// request, or either field on an ImageOperationTextToImage one. Never a
	// silent ignore: a caller populating a field its operation cannot use
	// is almost always a mistake worth surfacing.
	ErrImageInputNotAllowed = apperr.Invalid("aigateway.image_input_not_allowed")

	// ErrImageGenerationUnavailable reports Gateway.GenerateImage called on
	// a Gateway never given WithImageGeneration -- a Gateway built for
	// chat-only use has no queue or storage module to run the async-only
	// image pipeline on.
	ErrImageGenerationUnavailable = apperr.Internal("aigateway.image_generation_unavailable")

	// ErrImageRequiresTenant reports Gateway.GenerateImage called on a
	// context that carries no tenant. Every jobs.Task must carry a tenant
	// (jobs.Task.TenantID's own doc comment), so an image-generation call
	// with no tenant to attribute the resulting job to fails closed before
	// anything is enqueued. The cause is pkgcore.ErrNoTenant, mirroring
	// ErrTenantScopeRequiresTenant's identical shape.
	ErrImageRequiresTenant = apperr.Invalid("aigateway.image_requires_tenant").
				WithCause(pkgcore.ErrNoTenant)

	// ErrImageObjectUnavailable reports the image-generation job handler
	// failing to read InputObjectID or MaskObjectID's bytes from go/storage
	// -- the object does not exist, is not yet completed, or the store
	// refused the read.
	ErrImageObjectUnavailable = apperr.Internal("aigateway.image_object_unavailable")

	// ErrImageOutputWriteFailed reports the image-generation job handler
	// failing to write a provider's generated image bytes back into
	// go/storage as a new object.
	ErrImageOutputWriteFailed = apperr.Internal("aigateway.image_output_write_failed")

	// ErrImageJobClaimInFlight reports that imageGenerateHandler.Handle
	// found ai_gateway_image_jobs already holding a "pending" row for this
	// job -- either a genuinely concurrent Handle call for the same
	// jobs.JobID is running right now (a redelivery race the standalone
	// in-process queue cannot produce, but a lease-based distributed queue
	// can -- see image_job_store.go's own doc comment), or an earlier
	// attempt claimed the job and crashed before ever recording a vendor
	// answer. Either way, THIS attempt must not call ImageProvider: doing
	// so could bill the vendor a second time for work another attempt may
	// already be doing or may have already done. The queue's own retry
	// policy governs what happens next -- eventually a dead letter if the
	// claim is never resolved, which is the safe failure mode (never a
	// double vendor call) this package accepts in exchange for the rare
	// case actually being a stuck claim that needs operator attention.
	ErrImageJobClaimInFlight = apperr.Conflict("aigateway.image_job_claim_in_flight")

	// ErrMultipleImageResults reports an OpenAI-compatible image response
	// carrying more than one generated image -- a data array longer than
	// one entry, or a usage object claiming an image_count above one. The
	// whole image-generation pipeline (ImageJobResult through the job
	// handler's single go/storage write) carries exactly one output object
	// id, so a multi-image response -- the ordinary answer to an "n" > 1
	// request smuggled through Params -- is refused with this coded error
	// BEFORE any usage is recorded or any output object is written, never
	// silently decoded to the first image while the response's own
	// image_count bills for all of them. Classified Invalid since a caller
	// can avoid it entirely (by not asking for more than one image); an
	// image job that hits it fails the attempt and eventually dead-letters,
	// exactly like the other request-shaped refusals the job handler makes.
	ErrMultipleImageResults = apperr.Invalid("aigateway.multiple_image_results")

	// ErrInternal reports an HTTP-layer failure Handler cannot classify --
	// an error returned by something below it that is not an *apperr.Error
	// (writeError's fallback), or the system-context reason
	// aiGateway_setPlatformCredential builds for the caller (see
	// SystemPurposeCredentialWrite) being rejected, which is unreachable in
	// practice since Module.Register always registers that purpose before
	// any request can reach the handler -- handled anyway rather than
	// assumed away, mirroring storage's and org's identical fallback.
	ErrInternal = apperr.Internal("aigateway.internal_error")
)
