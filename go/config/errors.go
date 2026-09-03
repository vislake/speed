package config

import (
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the config module. Every exported error is an
// *apperr.Error builder: match a decorated error with apperr.As(err) and
// compare its Code, the convention tenancy and dbkit document. A call that
// returns an error decorated with WithParam or WithCause derives a new
// *apperr.Error, so never compare a once-returned error against a var here
// with == or errors.Is.
var (
	// ErrItemUnset reports that a key has neither a row at any scope the
	// context entitles the caller to read nor a schema default, so there is
	// no value to return. It is a config.unknown_key-class absence, distinct
	// from ErrUnknownKey only in that the key itself is a valid schema key.
	ErrItemUnset = apperr.NotFound("config.item_unset")

	// ErrUnknownKey reports a key that no module declared as a ConfigItem:
	// a Set or Get for an undeclared key is a programming error on the
	// caller's side, never a silently accepted free-form setting.
	ErrUnknownKey = apperr.NotFound("config.unknown_key")

	// ErrUnknownFlag reports a key that no module declared as a FeatureFlag.
	// IsEnabled and EnabledFlags only reason about declared flags, so this
	// error means the caller asked about a flag that does not exist (or
	// passed a plain ConfigItem key where only a FeatureFlag key has the
	// dependency semantics IsEnabled resolves).
	ErrUnknownFlag = apperr.NotFound("config.unknown_flag")

	// ErrInvalidScope reports a Set for a Scope outside the closed set of
	// system, tenant and user.
	ErrInvalidScope = apperr.Invalid("config.invalid_scope")

	// ErrUserScopeUnavailable reports a Set for ScopeUser. The tier is
	// reserved by the design and deliberately unimplemented in this
	// milestone; the error names the reservation rather than pretending the
	// scope was mistyped.
	ErrUserScopeUnavailable = apperr.Invalid("config.user_scope_unavailable")

	// ErrTenantScopeRequiresTenant reports a Set for ScopeTenant on a
	// context that carries no tenant. The owning tenant always comes from
	// the context -- never from a caller-supplied identifier -- so a
	// tenant-scoped write on an unscoped context fails closed. The cause is
	// pkgcore.ErrNoTenant, so callers that already classify that sentinel
	// keep working.
	ErrTenantScopeRequiresTenant = apperr.Invalid("config.tenant_scope_requires_tenant").
					WithCause(pkgcore.ErrNoTenant)

	// ErrSystemScopeRequiresSystemContext reports a Set for ScopeSystem on a
	// context that carries no system reason. Platform-wide configuration is
	// only writable through the audited system-context path
	// (pkgcore.WithSystemContext, or tenancy.WithSystemContext which adds
	// its own audit event), so an ordinary tenant-scoped request can never
	// widen a platform setting.
	ErrSystemScopeRequiresSystemContext = apperr.Forbidden("config.system_scope_requires_system_context")

	// ErrActorRequired reports a Set whose Actor is empty. Every write must
	// be attributable to someone; the actor is what the audit trail and the
	// config.item.changed event carry.
	ErrActorRequired = apperr.Invalid("config.actor_required")

	// ErrInvalidValue reports a Set value that does not fit the item's
	// declared Type or falls outside its declared Min/Max range. The
	// decorated error carries the key; the value itself is never echoed,
	// because the item may be Sensitive.
	ErrInvalidValue = apperr.Invalid("config.invalid_value")

	// ErrTypedValueMismatch reports a GetTyped[T] whose T does not match the
	// item's declared Type (a string item read as int64, an int item read as
	// string, and so on). Reading an int item with GetTyped[int] also fails
	// this way: int values are served as int64.
	ErrTypedValueMismatch = apperr.Invalid("config.typed_value_mismatch")

	// ErrSchemaConflict reports an Attach-time schema snapshot in which the
	// same key was declared both as a ConfigItem and as a FeatureFlag. The
	// two declarations would fight over one row in the configs table, which
	// is always a wiring bug rather than a merge.
	ErrSchemaConflict = apperr.Internal("config.schema_conflict")

	// ErrFeatureFlagDependencyCycle reports an Attach-time schema snapshot
	// whose feature flag dependencies form a cycle (a depends on b depends
	// on a). pkgcore.ValidateFeatureGraph proves every dependency resolves;
	// proving the graph is acyclic is this module's job, because a cyclic
	// "enabled" definition would never stabilize.
	ErrFeatureFlagDependencyCycle = apperr.Internal("config.feature_flag_dependency_cycle")

	// ErrCipherRequired reports an Attach that found Sensitive items in the
	// schema but no dbkit.Cipher wired in. Sensitive values must be
	// encrypted at rest, so a schema that declares them cannot be served
	// without the host's master key.
	ErrCipherRequired = apperr.Internal("config.cipher_required")

	// ErrAuditPublishFailed reports that a Set wrote its row but could not
	// publish the resulting config.item.changed event. The write itself has
	// happened -- this module has no outbox, and the deployment-mode bus may
	// be down -- but the audit trail has a hole, so the caller must treat
	// the set as needing a retry or investigation rather than as fully
	// successful. The decorated error carries the publish failure as its
	// cause.
	ErrAuditPublishFailed = apperr.Internal("config.audit_publish_failed")

	// ErrServiceNotAttached reports a call through a Service whose Module has
	// not been Attached to a Registry yet (or whose Attach failed). Routes
	// are mounted during Register and resolve the service lazily, so this
	// error can only surface in the window between Register and Attach, a
	// host wiring bug.
	ErrServiceNotAttached = apperr.Internal("config.service_not_attached")

	// ErrAlreadyAttached reports a second Attach call on one Module. The
	// schema snapshot freezes at the first Attach; a second call would
	// re-read a Registry whose registrars may have grown, silently serving a
	// different schema than the first snapshot, so it fails instead.
	ErrAlreadyAttached = apperr.Internal("config.already_attached")

	// ErrStorage reports a failure to read or write the configs table, or
	// to seal or unseal a stored Sensitive value. It wraps the underlying
	// error as its cause; the cause chain never reaches an HTTP response
	// (see http.go), so no storage detail or value content leaks outward.
	ErrStorage = apperr.Internal("config.storage_error")
)
