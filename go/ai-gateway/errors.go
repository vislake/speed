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
)
