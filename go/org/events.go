package org

import (
	"context"
	"encoding/json"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// EventUserCreated is the authn domain event org subscribes to in order to
// give a brand-new user their workspace.
//
// This constant is a COORDINATION POINT with the authn module, and this is
// the only place in org the string appears. authn owns the event: it
// declares it through pkgcore.Registry.Events.Publishes and org must NOT,
// or a host running both modules fails to bootstrap with
// ErrDuplicateEventType. org only subscribes.
//
// org does not -- and may not -- import authn's payload type. The root
// CLAUDE.md's canonical module-boundary example is this exact pair ("authn
// publishes UserCreated, org subscribes to create the default workspace;
// org never imports authn.User"), and the payload's shape is not even
// stable across deployment modes: pkgcore's Redis bus documents that a
// cross-replica payload arrives as a map[string]any with numbers decoded as
// float64, while a same-replica payload arrives as the publisher's own
// struct. userIDFromPayload therefore normalizes any shape through JSON and
// reads a field, and never type-asserts.
const EventUserCreated = "authn.user.created"

// The domain events org publishes about its roster. Names follow
// pkgcore.EventDecl's <module>.<entity>.<action> convention.
const (
	// EventMemberInvited announces a pending invitation. The M2 notification
	// module is its intended subscriber: once it exists, it delivers the
	// invitation and org's own mail leg is switched off with the
	// org.invitation_email flag.
	EventMemberInvited = "org.member.invited"

	// EventMemberJoined announces an accepted invitation, or any other path
	// by which a person became a member of the tenant.
	EventMemberJoined = "org.member.joined"

	// EventMemberRemoved announces a membership that no longer exists. authn
	// is its most important subscriber: docs/internal/05-identity-and-access
	// .md requires the removed user's tokens for that tenant to be revoked,
	// and the session state those tokens live in is authn's, never org's.
	EventMemberRemoved = "org.member.removed"
)

// The payloads carried by org's events.
//
// Every field carries a json tag because a payload crossing the distributed
// mode's Redis bus is marshalled to JSON and arrives at the subscriber as a
// map keyed by these names. The tags are therefore part of the public event
// contract, not a serialization detail.
//
// None of them repeats the tenant: pkgcore.Event carries TenantID in its own
// field, and duplicating it into the payload invites the two copies to
// disagree.
type (
	// NodeCreated is the payload of org.node.created.
	NodeCreated struct {
		NodeID   string `json:"node_id"`
		ParentID string `json:"parent_id"`
		Path     string `json:"path"`
		Depth    int    `json:"depth"`
		Kind     string `json:"kind"`
	}

	// NodeMoved is the payload of org.node.moved. It carries both paths
	// because the whole point of the event is that every descendant's path
	// changed: a subscriber caching anything keyed on a node path invalidates
	// on the old prefix and re-reads under the new one.
	NodeMoved struct {
		NodeID      string `json:"node_id"`
		OldParentID string `json:"old_parent_id"`
		NewParentID string `json:"new_parent_id"`
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
	}

	// NodeDeleted is the payload of org.node.deleted.
	NodeDeleted struct {
		NodeID  string `json:"node_id"`
		Path    string `json:"path"`
		Cascade bool   `json:"cascade"`
		// RemovedCount is how many rows the delete removed, the node itself
		// included.
		RemovedCount int64 `json:"removed_count"`
	}

	// MemberInvited is the payload of org.member.invited.
	//
	// It carries the blind index of the invitee's address, NEVER the address
	// itself. An event payload is written to a broker, logged by whoever
	// subscribes and often traced; putting an email address in one publishes
	// PII to every one of those places. A subscriber that must reach the
	// person reads the invitation row through org, where the address is
	// encrypted at rest.
	MemberInvited struct {
		InvitationID  string `json:"invitation_id"`
		NodeID        string `json:"node_id"`
		EmailIndex    string `json:"email_index"`
		InviterUserID string `json:"inviter_user_id"`
	}

	// MemberJoined is the payload of org.member.joined. InvitationID is empty
	// when the membership was not created by accepting an invitation.
	MemberJoined struct {
		MembershipID string `json:"membership_id"`
		UserID       string `json:"user_id"`
		NodeID       string `json:"node_id"`
		InvitationID string `json:"invitation_id"`
	}

	// MemberRemoved is the payload of org.member.removed.
	MemberRemoved struct {
		MembershipID string `json:"membership_id"`
		UserID       string `json:"user_id"`
		NodeID       string `json:"node_id"`
	}
)

// memberEventDecls is the catalog entry for each roster event, declared in
// Register alongside the tree events.
var memberEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventMemberInvited,
		PayloadType: "org.MemberInvited",
		Description: "A person was invited into a tenant, at a specific node of its organization tree.",
	},
	{
		Type:        EventMemberJoined,
		PayloadType: "org.MemberJoined",
		Description: "A person became a member of a tenant.",
	},
	{
		Type:        EventMemberRemoved,
		PayloadType: "org.MemberRemoved",
		Description: "A person's membership of a tenant was removed; their sessions for it should be revoked.",
	},
}

// hostSeams is the read-only view of the host's *pkgcore.Registry that org's
// runtime reads AT CALL TIME rather than capturing during Register.
//
// The distinction is not stylistic. Registry.Locales() is documented to be
// nil while modules are registering -- Kernel.Bootstrap installs the merged
// catalog only after every module's Register has returned -- so a service
// that captured the catalog in Register would capture nil and fail on its
// first rendered message. Holding the registry itself and asking it for each
// seam when the seam is used makes that mistake unrepresentable.
//
// *pkgcore.Registry satisfies this interface structurally; org declares it
// rather than taking the concrete type so that a test can substitute a host
// without building a kernel.
type hostSeams interface {
	// KVStore is the store the rate limiter counts in.
	KVStore() pkgcore.KVStore
	// Mailer is the outbound-email transport. org never holds an SMTP
	// client, a provider SDK or a template engine of its own.
	Mailer() pkgcore.Mailer
	// Locales is the merged message catalog. Nil until Bootstrap installs
	// it, which is exactly why this is a method call and not a field.
	Locales() *i18n.Catalog
	// EventBus is the bus subscriptions were installed on, so a publisher
	// reaches them.
	EventBus() pkgcore.EventBus
}

// compile-time check that the host's own registry satisfies the seam view
// org reads it through. It is what makes Module.Register's single assignment
// legal without org depending on anything the registry does not already
// offer.
var _ hostSeams = (*pkgcore.Registry)(nil)

// publishEvent emits one org event on the host's bus.
//
// A publish failure is logged and swallowed, never returned to the caller:
// by the time anything is published the business write has already
// committed, so surfacing the bus error would report a failure that did not
// happen. The Warn line is the operator's signal that a subscriber missed a
// fact.
func publishEvent(ctx context.Context, host hostSeams, eventType string, payload any) {
	if host == nil {
		return
	}
	bus := host.EventBus()
	if bus == nil {
		return
	}
	tenant, err := tenantOf(ctx)
	if err != nil {
		obs.FromContext(ctx).Warn("org event not published, context carries no tenant",
			"event_type", eventType, "error", err)
		return
	}
	if err := bus.Publish(ctx, pkgcore.Event{Type: eventType, TenantID: tenant, Payload: payload}); err != nil {
		obs.FromContext(ctx).Warn("org event publish failed",
			"event_type", eventType, "error", err)
	}
}

// handleUserCreated is org's subscriber for EventUserCreated: it gives a
// newly created user somewhere to be.
//
// # The resilience contract
//
// The publisher is another module, released on its own schedule, and org has
// no compile-time knowledge of its payload. Four cases, all of them tested:
//
//  1. Nobody publishes the event. The subscription is installed and simply
//     never fires. Registry.Events.Subscribe returns nothing and cannot
//     fail, so an absent publisher is not even observable here.
//
//  2. The payload is not a shape org recognizes -- a different struct, a
//     bare string, a map without a user id. Logged at Warn, and nil is
//     returned. Returning an error would be actively harmful: on the
//     in-memory bus a handler's error propagates back to the publisher's
//     own Publish call, so org's failure to understand a payload would
//     surface inside authn as a failed user creation.
//
//  3. The event carries no tenant. A self-registering user genuinely has no
//     tenant yet -- docs/internal/04 notes that at the moment a social login
//     succeeds there is no tenant at all -- so there is no workspace to
//     create. Logged at Debug and skipped; the tenant-creating path is the
//     explicit CreateTenantRoot call a host makes when a tenant is born.
//
//  4. The event carries a tenant. The tenant context is rebuilt from the
//     event (pkgcore.WithTenant) because a handler invoked by the
//     distributed mode's bus runs on a context that carries none, and every
//     Repository call would otherwise fail closed. Then the tenant's root
//     node and the user's membership are ensured IDEMPOTENTLY: a redelivered
//     event -- which an at-least-once broker will produce -- creates neither
//     a second root nor a second membership.
func (m *Module) handleUserCreated(ctx context.Context, evt pkgcore.Event) error {
	log := obs.FromContext(ctx)

	userID, ok := userIDFromPayload(evt.Payload)
	if !ok {
		// The payload itself is never logged: it belongs to another module
		// and may carry an email address or a name.
		log.Warn("org ignored a user-created event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if evt.TenantID == "" {
		log.Debug("org ignored a user-created event with no tenant",
			"event_type", evt.Type, "user_id", userID)
		return nil
	}

	ctx = pkgcore.WithTenant(ctx, evt.TenantID)
	root, err := m.ensureRoot(ctx)
	if err != nil {
		log.Warn("org could not ensure a workspace for a new user",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return nil
	}
	membership, created, err := m.members.ensure(ctx, userID, root.ID)
	if err != nil {
		log.Warn("org could not ensure a membership for a new user",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return nil
	}
	if !created {
		// A redelivery, or a user who already had a seat. Nothing changed,
		// so nothing is announced.
		return nil
	}
	m.members.publish(ctx, EventMemberJoined, MemberJoined{
		MembershipID: membership.ID,
		UserID:       membership.UserID,
		NodeID:       membership.NodeID,
	})
	return nil
}

// ensureRoot returns the tenant's root node, creating it when the tenant has
// none yet. Two concurrent deliveries can both find no root; the loser of
// that race sees ErrRootAlreadyExists and re-reads instead of failing, which
// is what makes the whole handler safe to repeat.
func (m *Module) ensureRoot(ctx context.Context) (*OrgNode, error) {
	switch root, err := m.tree.Root(ctx); {
	case err == nil:
		return root, nil
	case !hasCode(err, ErrNodeNotFound.Code):
		return nil, err
	}

	root, err := m.tree.CreateRoot(ctx, m.defaultWorkspaceName(ctx), defaultRootKind)
	if err == nil {
		return root, nil
	}
	if hasCode(err, ErrRootAlreadyExists.Code) || hasCode(err, ErrDuplicateSiblingName.Code) {
		return m.tree.Root(ctx)
	}
	return nil, err
}

// defaultRootKind is the Kind a workspace root created for a brand-new user
// carries. Kind is the tenant's own business vocabulary and org enumerates
// none of it, so the automatic path picks the most neutral word available
// and lets the tenant rename the node afterwards.
const defaultRootKind = "workspace"

// defaultWorkspaceName renders the name of an automatically created root
// from the message catalog, so the one node org ever names on its own is not
// an English string literal buried in Go.
//
// The catalog is read here, at call time, from the host seam -- never
// captured during Register, where Registry.Locales() is documented to be
// nil.
//
// The locale is the platform default: this node is created before anybody
// has expressed a preference org can see, and a tenant renames the node the
// moment it means something else to them. If the catalog is missing or the
// lookup fails, the tenant id is used instead of a language-specific string:
// an identifier is not user-facing text, so the fallback cannot smuggle one
// language into the other's UI.
func (m *Module) defaultWorkspaceName(ctx context.Context) string {
	fallback := func() string {
		tenant, err := tenantOf(ctx)
		if err != nil {
			return defaultRootKind
		}
		return string(tenant)
	}
	if m.host == nil {
		return fallback()
	}
	catalog := m.host.Locales()
	if catalog == nil {
		return fallback()
	}
	name, err := catalog.Lookup(i18n.LocaleZHCN, msgDefaultWorkspaceName, nil)
	if err != nil {
		obs.FromContext(ctx).Warn("org could not render the default workspace name",
			"message_id", msgDefaultWorkspaceName, "error", err)
		return fallback()
	}
	return name
}

// userCreatedUserIDKeys are the field spellings org accepts for the user id
// inside an authn.user.created payload, probed in order.
//
// Several spellings are accepted because the payload reaches org as data,
// not as a type: a same-process publish delivers authn's struct (whose JSON
// tag org cannot see at compile time), while the Redis bus delivers a map
// built from that struct's tags. Probing a small ordered list is what lets
// org read both without ever naming authn's type.
var userCreatedUserIDKeys = []string{"user_id", "userId", "UserID", "userID"}

// userIDFromPayload extracts the user id from an authn.user.created payload
// of any shape, by round-tripping it through JSON into a map and probing the
// accepted key spellings.
//
// It never type-asserts the publisher's concrete type -- doing so would
// require importing it, which is the one thing this module may not do -- and
// it returns ok=false rather than an error for every unusable shape, because
// the caller's contract is to log and continue, not to fail the publisher.
func userIDFromPayload(payload any) (string, bool) {
	if payload == nil {
		return "", false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", false
	}
	for _, key := range userCreatedUserIDKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		id, ok := value.(string)
		if ok && id != "" {
			return id, true
		}
	}
	return "", false
}
