package pkgcore

import "context"

// ActorType classifies who is behind an Actor: a signed-in human, a
// platform operator, an API key, or the system itself. It is a closed set
// rather than free text so that a downstream consumer -- an audit-event
// persister, in particular -- can group and filter by kind without first
// having to enumerate every string a caller might have typed.
type ActorType string

const (
	// ActorTypeUser identifies a signed-in tenant user.
	ActorTypeUser ActorType = "user"
	// ActorTypePlatformAdmin identifies a platform operator acting outside
	// any single tenant's own membership, for example through the escape
	// hatch WithSystemContext grants.
	ActorTypePlatformAdmin ActorType = "platform_admin"
	// ActorTypeAPIKey identifies a request authenticated by an API key
	// rather than a signed-in session.
	ActorTypeAPIKey ActorType = "api_key"
	// ActorTypeSystem identifies an automated process acting with no human
	// behind it -- a scheduled job, a background worker, an event
	// subscriber reacting to another module's action.
	ActorTypeSystem ActorType = "system"
)

// Actor identifies who is making a request or performing an action, for
// attribution in contexts such as an audit trail. ID is the actor's stable
// identifier within its Type's own namespace (a user id, an API key id, a
// named system task); DisplayName is a human-readable label carried
// alongside it so a record stays readable even after the identified
// account is later renamed or deleted.
type Actor struct {
	Type        ActorType
	ID          string
	DisplayName string
}

// WithActor returns a copy of ctx carrying a as the current actor.
//
// WithActor and WithOnBehalfOf are layered independently of one another:
// setting one never clears or overwrites the other. This is deliberate --
// during an impersonated (admin-as-user) session, Actor identifies the
// impersonated user while OnBehalfOf identifies the real administrator
// performing the action, and both must be settable, and readable, at once.
// See docs/internal/10-compliance-and-audit.md's impersonation rule and
// OnBehalfOfFromContext's own doc comment.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, ctxKeyActor, a)
}

// ActorFromContext returns the Actor carried by ctx. The second result is
// false when ctx carries none, in which case the first result is the zero
// Actor.
//
// Unlike TenantFromContext, an Actor with a zero-value ID is still reported
// present as long as WithActor was called: this function answers "was an
// actor set on this context", not "does the actor look genuinely
// populated" -- the latter judgment, if a caller needs it, belongs to the
// caller (mirroring SystemReasonFromContext's identical convention).
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(ctxKeyActor).(Actor)
	return a, ok
}

// WithOnBehalfOf returns a copy of ctx carrying a as the real actor on
// whose behalf the current, possibly impersonated, actor is acting. See
// WithActor's doc comment for how the two layer independently.
func WithOnBehalfOf(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, ctxKeyOnBehalfOf, a)
}

// OnBehalfOfFromContext returns the Actor set by WithOnBehalfOf. The second
// result is false when ctx carries none, in which case the first result is
// the zero Actor.
//
// A present OnBehalfOf means the current Actor (see ActorFromContext) is
// impersonated: per docs/internal/10-compliance-and-audit.md, every audit
// record produced while impersonating must carry both identities, because
// an investigation that only sees the impersonated user's own id looks
// exactly like that user acting on their own account -- the single most
// common accountability mistake in an admin console.
func OnBehalfOfFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(ctxKeyOnBehalfOf).(Actor)
	return a, ok
}
