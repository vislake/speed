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
	// pkgcore.ErrNoTenant, mirroring config's identical rule.
	ErrTenantScopeRequiresTenant = apperr.Invalid("aigateway.tenant_scope_requires_tenant").
					WithCause(pkgcore.ErrNoTenant)

	// ErrSystemScopeRequiresSystemContext reports SetPlatformCredential on a
	// context that carries no audited system reason. Platform-wide
	// credentials are only writable through the audited system-context path
	// (pkgcore.WithSystemContext, or tenancy.WithSystemContext, which adds
	// its own audit event), mirroring config's ScopeSystem rule exactly.
	ErrSystemScopeRequiresSystemContext = apperr.Forbidden("aigateway.system_scope_requires_system_context")

	// ErrProviderRequestFailed reports a transport-level failure calling a
	// ChatProvider's upstream vendor endpoint: a network error, a
	// non-2xx HTTP status, or a stream that ended in an I/O error before
	// its final chunk arrived.
	ErrProviderRequestFailed = apperr.Internal("aigateway.provider_request_failed")

	// ErrProviderResponseInvalid reports a response (or one streamed chunk)
	// from a ChatProvider's upstream vendor endpoint that could not be
	// parsed as the expected wire shape.
	ErrProviderResponseInvalid = apperr.Internal("aigateway.provider_response_invalid")

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
	// chat-only use has no queue or storage seam to run the design doc's
	// async-only image pipeline on.
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
)
