package pki

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// The domain events this module publishes, named <module>.<entity>.<action>
// per backend coding standard §8.
//
// Round 2 declared three of the eventual signing-key lifecycle events --
// staged, activated, retired -- matching exactly the three transitions its
// expiry scan drives (docs/internal/22-pki.md's "delivery rounds" table).
// There is deliberately NO ".pending" event (a key enters pending at the
// moment it is staged, so EventSigningKeyStaged already communicates it)
// and NO ".retiring" event (a key enters retiring at the moment its
// replacement is activated, so EventSigningKeyActivated already
// communicates both halves of that one atomic transition -- see
// SigningKeyRepository.PromoteToActive's own doc comment).
//
// Round 3 (this round) adds the fourth signing-key event,
// EventSigningKeyRevoked, plus the module's first pki.certificate.* event,
// EventCertificateRevoked -- revocation.go's Service.RevokeSigningKey and
// CAService.RevokeCertificate respectively. Every other
// pki.certificate.*/pki.authority.* event named in docs/internal/22-pki.md's
// module-contract section (.issued/.renewed/.expiring, .expiring) stays
// undeclared: this module's own round-1 table already established the
// discipline that an undeclared-but-unused event is dead catalog weight,
// not forward compatibility, and only .revoked has a real emitter this
// round.
//
// Consumers: this module's own process-local key-set cache (cache.go)
// subscribes to every signing-key event, including the new .revoked one --
// see attachBus in service.go, which is what makes "the process-local
// cache must be invalidated on revocation" true through the SAME mechanism
// round 2 built, never a second cache-clearing path. EventCertificateRevoked
// has no in-module subscriber (CAService keeps no cache), and exists for an
// eventual audit/notification consumer this round does not build, matching
// the identical reasoning SigningKeyLifecycleEvent's own doc comment
// already gives for KID/OccurredAt.
const (
	// EventSigningKeyStaged is published when the expiry scan generates a
	// new SigningKeyStatusPending key ahead of an approaching expiry (see
	// service.go's StageDueRotations). The host is expected to subscribe if
	// it needs to roll the new public key out to an external system before
	// relying on it -- this module never does that itself (docs/internal/
	// 22-pki.md's "rotation" section is explicit that pushing to any
	// external system is never this module's job).
	EventSigningKeyStaged = "pki.signing_key.staged"

	// EventSigningKeyActivated is published when a pending key is promoted
	// to SigningKeyStatusActive past its propagation window. Per this
	// file's own doc comment, this single event also communicates the
	// previously active key (if any) moving to SigningKeyStatusRetiring --
	// there is no separate event for that half.
	EventSigningKeyActivated = "pki.signing_key.activated"

	// EventSigningKeyRetired is published when a retiring key is retired
	// past its overlap period.
	EventSigningKeyRetired = "pki.signing_key.retired"

	// EventSigningKeyRevoked is published when Service.RevokeSigningKey
	// (revocation.go) transitions a key -- of any prior status -- to
	// SigningKeyStatusRevoked. Round 3's addition.
	EventSigningKeyRevoked = "pki.signing_key.revoked"

	// EventCertificateRevoked is published when CAService.RevokeCertificate
	// (revocation.go) transitions a tenant certificate to
	// CertificateStatusRevoked. Round 3's addition, and this module's first
	// pki.certificate.* event.
	EventCertificateRevoked = "pki.certificate.revoked"
)

// certificateEventPayloadType names the payload EventCertificateRevoked
// carries, for EventDecl.PayloadType -- the pki.certificate.* counterpart
// of signingKeyEventPayloadType above. A single named constant even though
// only one event uses it today, matching that same precedent (a second
// pki.certificate.* event, when one is declared, reuses this type rather
// than inventing a new payload shape for the same entity).
const certificateEventPayloadType = "pki.CertificateRevokedEvent"

// signingKeyEventPayloadType names the payload every signing-key lifecycle
// event above carries, for EventDecl.PayloadType. One shape serves all
// three events -- they describe the same fact (this kid, this purpose,
// something happened to it) in different directions, exactly like rbac's
// RoleBindingChangedEvent serves both its assign and revoke events.
const signingKeyEventPayloadType = "pki.SigningKeyLifecycleEvent"

// eventDecls is what Register declares to pkgcore's EventRegistrar. A
// package-level var, not a literal inside Register, so this module's own
// tests can assert the declared set without re-typing it -- the same
// reason rbac's eventDecls is a var (go/rbac/events.go).
var eventDecls = []pkgcore.EventDecl{
	{
		Type:        EventSigningKeyStaged,
		PayloadType: signingKeyEventPayloadType,
		Description: "Published when the expiry scan generates a new pending signing key ahead of an approaching expiry.",
	},
	{
		Type:        EventSigningKeyActivated,
		PayloadType: signingKeyEventPayloadType,
		Description: "Published when a pending signing key is promoted to active past its propagation window, demoting the purpose's previously active key into retiring.",
	},
	{
		Type:        EventSigningKeyRetired,
		PayloadType: signingKeyEventPayloadType,
		Description: "Published when a retiring signing key is retired past its overlap period.",
	},
	{
		Type:        EventSigningKeyRevoked,
		PayloadType: signingKeyEventPayloadType,
		Description: "Published when a signing key is revoked, of any prior status.",
	},
	{
		Type:        EventCertificateRevoked,
		PayloadType: certificateEventPayloadType,
		Description: "Published when a tenant certificate is revoked.",
	},
}

// SigningKeyLifecycleEvent is the payload of EventSigningKeyStaged,
// EventSigningKeyActivated, EventSigningKeyRetired and (round 3)
// EventSigningKeyRevoked -- signingKeyEventPayloadType's shape.
//
// Its first job is this module's own cache invalidation (cache.go): Purpose
// is the only field the cache strictly needs, since it drops (or refreshes)
// the whole purpose entry rather than editing it in place. KID and the
// timestamp travel alongside for an eventual audit/notification consumer,
// the same reasoning rbac.RoleBindingChangedEvent's own doc comment gives
// for carrying more than the cache invalidation path strictly requires.
type SigningKeyLifecycleEvent struct {
	// Purpose is the signing key's purpose, e.g. "authn.access_token".
	Purpose string

	// KID is the affected key's id.
	KID string

	// PreviousKID is the purpose's previously active key's id, set only on
	// EventSigningKeyActivated when a previous active key existed to demote
	// (empty on a purpose's very first rotation-staged activation, and
	// always empty on the other two event types).
	PreviousKID string

	// OccurredAt is when the transition was written.
	OccurredAt time.Time
}

// signingKeyLifecycleEventFromWire recovers a SigningKeyLifecycleEvent from
// whatever the EventBus delivered.
//
// Two shapes must both work, and this is not a nicety: the standalone
// mode's in-memory bus passes the concrete struct through untouched, but
// the distributed mode's bus serializes the payload as JSON and hands the
// subscriber a plain map[string]any keyed by the struct's Go field names
// (identical to rbac.roleBindingChangedFromWire's own reasoning). Cache
// invalidation across replicas is the whole point of publishing these
// events, so an event that crossed a replica boundary must decode here or
// a distributed deployment would silently fall back to the cache's TTL
// poll alone.
func signingKeyLifecycleEventFromWire(payload any) (SigningKeyLifecycleEvent, bool) {
	switch p := payload.(type) {
	case SigningKeyLifecycleEvent:
		return p, true
	case map[string]any:
		purpose, ok := p["Purpose"].(string)
		if !ok || purpose == "" {
			return SigningKeyLifecycleEvent{}, false
		}
		evt := SigningKeyLifecycleEvent{Purpose: purpose}
		if kid, ok := p["KID"].(string); ok {
			evt.KID = kid
		}
		if prev, ok := p["PreviousKID"].(string); ok {
			evt.PreviousKID = prev
		}
		return evt, true
	default:
		return SigningKeyLifecycleEvent{}, false
	}
}

// CertificateRevokedEvent is the payload of EventCertificateRevoked --
// certificateEventPayloadType's shape. Unlike SigningKeyLifecycleEvent,
// this module keeps no cache over certificates, so no in-module subscriber
// ever needs to decode this payload back out of a distributed bus's
// map[string]any -- it exists purely for CAService.RevokeCertificate's
// publish call and an eventual audit/notification consumer, per this
// file's own doc comment.
type CertificateRevokedEvent struct {
	// TenantID is the certificate's owning tenant.
	TenantID string

	// CertificateID is the revoked pki_certificates row's id.
	CertificateID string

	// AuthorityID is the Authority that issued the revoked certificate.
	AuthorityID string

	// Serial is the revoked certificate's serial number, lower-case hex --
	// the same encoding Certificate.Serial and CertificateRevocation.Serial
	// use.
	Serial string

	// RevocationReason is the reason RevokeCertificate was given.
	RevocationReason string

	// OccurredAt is when the revocation was written.
	OccurredAt time.Time
}
