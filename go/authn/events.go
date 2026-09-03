package authn

import (
	"github.com/vislake/speed/go/pkgcore"
)

// Domain event types this module publishes, following
// "<module>.<entity>.<action>".
//
// Publishing is how authn reaches everything it must not import. The
// notification module subscribes to these and decides what to send; org
// subscribes to EventUserCreated to provision whatever a new user needs.
// authn imports neither, and the arrangement is what keeps the dependency
// graph acyclic while still letting a new sign-in cause an email.
//
// The one thing that does NOT travel this way is a synchronous verification
// code, which has to be delivered inside the request that asked for it and so
// goes through the Mailer and SMS seams directly.
const (
	// EventUserCreated is published when a new user record exists.
	EventUserCreated = "authn.user.created"

	// EventUserLoggedIn is published on every successful sign-in.
	EventUserLoggedIn = "authn.user.logged_in"

	// EventLoginFailed is published on every failed sign-in attempt. Its
	// payload carries the failure reason for detection pipelines; the API
	// response it accompanies does not.
	EventLoginFailed = "authn.login.failed"

	// EventSessionRevoked is published whenever a session stops being
	// usable, whoever ended it.
	EventSessionRevoked = "authn.session.revoked"

	// EventSessionReplayDetected is published when a consumed refresh
	// token is presented again. It is one of the very few signals that
	// detects a stolen credential automatically, so it is a first-class
	// event rather than a log line: the notification module turns it into
	// the security notice the account owner cannot switch off.
	EventSessionReplayDetected = "authn.session.replay_detected"

	// EventTenantSwitched is published when a session changes which
	// tenant its access tokens are issued for.
	EventTenantSwitched = "authn.tenant.switched"

	// EventIdentityBound is published when an external identity -- a
	// social account or an enterprise single sign-on subject -- is
	// attached to a user. It is a security-relevant change to how an
	// account can be signed into, which is exactly the class of change the
	// account owner should hear about, so it is announced rather than only
	// written down.
	EventIdentityBound = "authn.identity.bound"

	// EventIdentityUnbound is published when an external identity is
	// detached from a user.
	EventIdentityUnbound = "authn.identity.unbound"
)

// Audit actions this module registers. They use the present-tense verb form
// of an operation performed ("authn.user.login"), deliberately distinct from
// the past-tense fact an event records ("authn.user.logged_in"): the audit
// trail answers "what did someone do", the event answers "what became true".
const (
	// AuditActionUserRegister records an account being created.
	AuditActionUserRegister = "authn.user.register"
	// AuditActionUserLogin records a sign-in.
	AuditActionUserLogin = "authn.user.login"
	// AuditActionSessionRevoke records a session being signed out.
	AuditActionSessionRevoke = "authn.session.revoke"
	// AuditActionTenantSwitch records a session changing its active tenant.
	AuditActionTenantSwitch = "authn.tenant.switch"
	// AuditActionIdentityBind records an external identity being attached
	// to an account.
	AuditActionIdentityBind = "authn.identity.bind"
	// AuditActionIdentityUnbind records an external identity being
	// detached from an account.
	AuditActionIdentityUnbind = "authn.identity.unbind"
	// AuditActionSSOConfigure records a tenant's enterprise single
	// sign-on configuration being written.
	AuditActionSSOConfigure = "authn.sso.configure"
)

// UserCreatedPayload is the Event.Payload of EventUserCreated.
//
// It carries IDs and no personal data. A subscriber that needs the address
// reads it from the user record under its own access control; putting it in
// the payload would broadcast it to every subscriber of the bus, and in the
// distributed deployment mode would write it into the broker.
type UserCreatedPayload struct {
	// UserID is the new User.ID.
	UserID string
	// HasEmail and HasPhone report which identifiers the account was
	// created with, which is enough for a subscriber to decide whether it
	// can reach the user at all without learning how.
	HasEmail bool
	HasPhone bool
}

// UserLoggedInPayload is the Event.Payload of EventUserLoggedIn.
type UserLoggedInPayload struct {
	// UserID is the signed-in User.ID.
	UserID string
	// SessionID is the session the sign-in created.
	SessionID string
	// TenantID is the tenant the first access token was issued for,
	// carried as a plain string because an event payload is a wire shape.
	TenantID string
	// Method is one of the Method* constants.
	Method string
	// AMR lists the authentication methods the session was established
	// with.
	AMR []string
	// IP is the client address the sign-in came from.
	IP string
}

// LoginFailedPayload is the Event.Payload of EventLoginFailed.
type LoginFailedPayload struct {
	// UserID is the matched account, or empty when the identifier matched
	// none. The attempted identifier itself is deliberately absent: it is
	// PII, and for an unmatched attempt it is PII belonging to someone who
	// may have no relationship with this deployment at all.
	UserID string
	// Method is one of the Method* constants.
	Method string
	// Reason is one of the FailureReason* constants.
	Reason string
	// IP is the client address the attempt came from.
	IP string
}

// SessionRevokedPayload is the Event.Payload of EventSessionRevoked.
type SessionRevokedPayload struct {
	// UserID owns the revoked session.
	UserID string
	// SessionID is the revoked Session.ID.
	SessionID string
	// Reason is one of the RevokeReason* constants.
	Reason string
}

// SessionReplayDetectedPayload is the Event.Payload of
// EventSessionReplayDetected.
type SessionReplayDetectedPayload struct {
	// UserID owns the session whose token was replayed.
	UserID string
	// SessionID is the session revoked in response.
	SessionID string
	// FamilyID is the refresh-token family revoked in response.
	FamilyID string
}

// TenantSwitchedPayload is the Event.Payload of EventTenantSwitched.
type TenantSwitchedPayload struct {
	// UserID owns the session.
	UserID string
	// SessionID is the session that switched, unchanged by the switch.
	SessionID string
	// FromTenantID and ToTenantID are the tenants before and after.
	FromTenantID string
	ToTenantID   string
}

// IdentityBoundPayload is the Event.Payload of EventIdentityBound.
//
// It carries no external identifier and no email address. A subscriber that
// needs either reads the identity row under its own access control; putting a
// provider's account identifier on the bus would broadcast it to every
// subscriber and, in the distributed deployment mode, write it into the
// broker.
type IdentityBoundPayload struct {
	// UserID is the account the identity was attached to.
	UserID string
	// IdentityID is the new UserIdentity.ID.
	IdentityID string
	// Provider is the channel, one of the Provider* constants or an
	// "oidc:<tenant>" enterprise channel.
	Provider string
	// AutoLinked reports whether the binding happened automatically during
	// a sign-in, rather than being requested by an already-signed-in user.
	// It is the field a security notice keys on: an automatic link is the
	// one the account owner did not explicitly ask for.
	AutoLinked bool
}

// IdentityUnboundPayload is the Event.Payload of EventIdentityUnbound.
type IdentityUnboundPayload struct {
	// UserID is the account the identity was detached from.
	UserID string
	// IdentityID is the removed UserIdentity.ID.
	IdentityID string
	// Provider is the channel the removed identity belonged to.
	Provider string
}

// eventDecls is what Module.Register declares to the registry. Declaring an
// event is a documentation and mapping contract -- it is what lets the
// integration module map internal facts onto a versioned public schema
// without subscribing to each of them first -- not a precondition for
// publishing one.
var eventDecls = []pkgcore.EventDecl{
	{
		Type:        EventUserCreated,
		PayloadType: "authn.UserCreatedPayload",
		Description: "Published when a new user account has been created.",
	},
	{
		Type:        EventUserLoggedIn,
		PayloadType: "authn.UserLoggedInPayload",
		Description: "Published on every successful sign-in.",
	},
	{
		Type:        EventLoginFailed,
		PayloadType: "authn.LoginFailedPayload",
		Description: "Published on every failed sign-in attempt.",
	},
	{
		Type:        EventSessionRevoked,
		PayloadType: "authn.SessionRevokedPayload",
		Description: "Published when a session is signed out or revoked.",
	},
	{
		Type:        EventSessionReplayDetected,
		PayloadType: "authn.SessionReplayDetectedPayload",
		Description: "Published when a consumed refresh token is presented again, indicating the token leaked.",
	},
	{
		Type:        EventTenantSwitched,
		PayloadType: "authn.TenantSwitchedPayload",
		Description: "Published when a session changes the tenant its access tokens are issued for.",
	},
	{
		Type:        EventIdentityBound,
		PayloadType: "authn.IdentityBoundPayload",
		Description: "Published when an external social or single sign-on identity is attached to an account.",
	},
	{
		Type:        EventIdentityUnbound,
		PayloadType: "authn.IdentityUnboundPayload",
		Description: "Published when an external social or single sign-on identity is detached from an account.",
	},
}

// auditActions is what Module.Register registers with the audit-action
// registrar.
var auditActions = []string{
	AuditActionUserRegister,
	AuditActionUserLogin,
	AuditActionSessionRevoke,
	AuditActionTenantSwitch,
	AuditActionIdentityBind,
	AuditActionIdentityUnbind,
	AuditActionSSOConfigure,
}
