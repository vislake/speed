package org

import (
	"context"
	"embed"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/org/locales"
	"github.com/vislake/speed/go/org/migrations"
)

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// moduleName is org's the module contract's Name(). It is also the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "org"

// apiPath is the common prefix org's HTTP routes are mounted at (see
// Register below). It must agree with the "paths:" keys of this module's
// own OpenAPI fragment (api/openapi.yaml): every one of them starts with
// this prefix, and Handler's inner mux (built by api.HandlerFromMux, see
// handler.go) registers each spec path as an ABSOLUTE net/http pattern --
// mounting at apiPath here only tells the host's outer mux which requests to
// hand to Handler at all, exactly as notes' identical apiPath constant does
// for its own single route.
const apiPath = "/api/v1/org"

// The permissions org contributes to the platform's permission catalog.
// Enforcement belongs to rbac, which decides which role holds which of
// these; org only declares that they exist and what they are called.
const (
	// PermissionRead covers reading the organization tree and its members.
	PermissionRead = "org:read"
	// PermissionManage covers creating, renaming, moving and deleting nodes.
	PermissionManage = "org:manage"
	// PermissionInviteMember covers inviting a person into the tenant.
	PermissionInviteMember = "org:invite_member"
	// PermissionRemoveMember covers removing a member from the tenant.
	PermissionRemoveMember = "org:remove_member"
)

// The dbkit.Auditable resource labels OrgNode, Membership and Invitation
// return from AuditResourceType (model.go, membership.go, invitation.go) --
// the single source the whole audit vocabulary is built from. dbkit's
// write-capture plugin derives a captured write's action as
// "<label>.<operation>" (go/dbkit/audit_capture.go), and go/dbkit/audit's
// persister derives the persisted action the same way (audit/module.go);
// the AuditAction* constants below are constant expressions over exactly
// these labels, so the three sides -- a model's AuditResourceType, the
// declared vocabulary and the mechanism's own derivation -- can never
// drift: a solo rename of a label here propagates through the models and
// the declared actions in the same edit, where the earlier
// two-independent-literal shape let a label rename silently change what
// the mechanism derives while the declared actions kept their old
// strings -- a mismatch the persister's vocabulary gate answers with an
// alert and a dropped row, never an error.
const (
	AuditResourceTypeNode       = "org.node"
	AuditResourceTypeMember     = "org.member"
	AuditResourceTypeInvitation = "org.invitation"
)

// The audit actions org contributes to the audit vocabulary -- and the
// exact vocabulary org's write paths actually produce. Each is the
// resource label above concatenated with the GORM processor the write
// ran -- "create" or "update" -- the identical derivation dbkit's
// write-capture plugin performs at capture time, so a declared action is
// always a string the mechanism can genuinely derive: org's rows are only
// ever created or updated, since every delete org performs is a
// mark-delete UPDATE and nothing in this module ever issues a physical
// DELETE, so no org write is captured under a "delete" operation.
//
// This block deliberately does NOT declare the event-shaped names the
// operations might suggest (org.node.rename / org.node.move /
// org.node.delete / org.member.invite / org.member.accept /
// org.member.remove): nothing in org emits rows under those names, and the automatic capture
// mechanism cannot produce them, its operation vocabulary being the
// processor's own three. The semantic operations all still land in the
// trail, under the generic action plus the write's own changes diff:
// a rename, a move, a mark-delete and a restore each record as
// "org.node.update" (distinguishable by which columns the diff shows
// changed); an invite records as "org.invitation.create"; an acceptance
// records as "org.invitation.update" (the invitation row's status flip)
// plus "org.member.create" (the membership row); a revoke as
// "org.invitation.update"; a member removal or restore as
// "org.member.update". A host wiring the capture plugin must declare
// exactly this vocabulary on its AuditActionRegistrar (org's own Register
// does), because go/dbkit/audit's persister refuses -- with a structured
// alert -- any captured event whose derived action no module declared.
const (
	AuditActionNodeCreate = AuditResourceTypeNode + ".create"
	AuditActionNodeUpdate = AuditResourceTypeNode + ".update"

	AuditActionMemberCreate = AuditResourceTypeMember + ".create"
	AuditActionMemberUpdate = AuditResourceTypeMember + ".update"

	AuditActionInvitationCreate = AuditResourceTypeInvitation + ".create"
	AuditActionInvitationUpdate = AuditResourceTypeInvitation + ".update"
)

// AuditableModels returns the models org ships with the dbkit.Auditable
// marker -- the module's own declaration of which of its writes the
// automatic write-capture mechanism may capture. A host wiring that
// mechanism on the connection org writes through lists this slice in
// dbkit.Options.AuditModels, so the host's capture scope is this module's
// own declaration rather than hand-kept literals:
//
//	dbkit.Open(ctx, dbkit.Options{
//		Dialect: ..., DSN: ...,
//		AuditBus:    bus,
//		AuditModels: org.AuditableModels(),
//	})
//
// The contract is asymmetric on purpose, and the asymmetry is dbkit's own,
// not this module's: Open refuses an entry that lost the marker
// (dbkit.invalid_audit_model -- a listed model that has lost the Auditable
// marker must not be silently skipped), while nothing can
// refuse a model that GAINED the marker while this list forgot it, because
// Open receives no model inventory -- GORM's models register lazily, per
// statement. The marker side of the contract is therefore this module's
// own: adding dbkit.Auditable to an org model belongs in this list in the
// same edit, and module_test.go's
// TestModule_AuditableModels_IsExactlyTheMarkedModels pins the list to the
// models that carry the marker so the two cannot silently diverge. A
// model deliberately OUT of the list is out of the capture scope of every
// host -- which is how a host keeps a same-connection Auditable model
// owned by another recording path (the reference app's notes.Note, which
// persists its own trail through audit.Emit) out of automatic capture:
// org's list contains only org's models by construction.
func AuditableModels() []any {
	return []any{OrgNode{}, Membership{}, Invitation{}}
}

// The feature flags org contributes.
//
// Both default to on: a tenant that installed an organization product wants
// to be able to add people to it. A host or an operator turns them off per
// tenant through the config module, which org reads through the FeatureGate
// seam below without importing it.
const (
	// FeatureInvitations gates the whole invitation flow. With it off,
	// Invite reports org.invitations_disabled and nothing is stored or sent.
	FeatureInvitations = "org.invitations"

	// FeatureInvitationEmail gates only the delivery leg. With it off, an
	// invitation is still created and org.member.invited is still published
	// -- which is exactly the arrangement the M2 notification module
	// subscribes into, taking delivery over from org without a code change.
	//
	// It depends on FeatureInvitations: there is nothing to deliver when
	// invitations themselves are off.
	FeatureInvitationEmail = "org.invitation_email"
)

// featureFlagDecls is the catalog entry for each flag, declared in Register.
var featureFlagDecls = []pkgcore.FeatureFlag{
	{
		Key:         FeatureInvitations,
		Default:     true,
		Description: "Allow members of this tenant to invite people into its organization.",
	},
	{
		Key:         FeatureInvitationEmail,
		Default:     true,
		Description: "Let org deliver the invitation email itself, rather than leaving delivery to a notification module.",
		DependsOn:   []string{FeatureInvitations},
	},
}

// bootstrapKeyDecl is the process-start key material this module consumes: the
// HMAC key its invitation-address blind indexer is built from
// (WithEmailIndexer over dbkit.NewBlindIndexer).
//
// It is a separate secret from the host's configuration cipher key on purpose.
// A host reusing that cipher to encrypt org's Invitation.Email column is the
// ordinary wiring, and dbkit's own rule is that an AES key must never double as
// an HMAC key; introducing this one additional key is what keeps that rule real
// rather than aspirational. An invitation whose address cannot be indexed can
// never be found again, so the key must not change between restarts.
var bootstrapKeyDecl = pkgcore.BootstrapKey{
	Key:         "org.invitation_email_index_key",
	Format:      "hexkey",
	Default:     "documented non-secret development default",
	Sensitive:   true,
	Description: "HMAC key org's blind indexer indexes invitation email addresses with; separate from every cipher key, because an AES key never doubles as an HMAC key.",
	Group:       moduleName,
}

// FeatureGate reports whether a feature flag is enabled for the tenant in
// ctx.
//
// It is the same no-import technique as Scope, in the other direction: the
// signature is built from stdlib types only, so *config.Service satisfies it
// structurally through its own IsEnabled method. org never imports config
// (config sits beside org rather than beneath it), and config never learns
// that org exists. The host passes one to the other, and the host is the only
// place both names appear.
//
// A nil gate means org honors the flags' declared defaults, which is what a
// host running org without the config module gets.
type FeatureGate interface {
	IsEnabled(ctx context.Context, key string) (bool, error)
}

// FeatureGateFunc adapts a plain function to FeatureGate, the same
// func-to-interface adapter shape http.HandlerFunc popularized, so a host
// need not declare a named type just to wire this seam. The config module's
// lazy Handle satisfies the gate through a method value alone
// (org.FeatureGateFunc(handle.IsEnabled)); a host whose reader needs a
// guard of its own passes a closure instead.
type FeatureGateFunc func(ctx context.Context, key string) (bool, error)

// IsEnabled implements FeatureGate.
func (f FeatureGateFunc) IsEnabled(ctx context.Context, key string) (bool, error) {
	return f(ctx, key)
}

// compile-time check that FeatureGateFunc satisfies FeatureGate.
var _ FeatureGate = FeatureGateFunc(nil)

// The domain events org publishes about its tree. Names follow
// pkgcore.EventDecl's <module>.<entity>.<action> convention.
//
// org.node.moved matters to more subscribers than it looks: a move changes
// every descendant's materialized path, which is the dimension rbac's
// path-prefix policies and every subtree-scoped listing are written against.
// A subscriber that caches anything keyed on a node path must invalidate on
// this event.
const (
	EventNodeCreated = "org.node.created"
	EventNodeMoved   = "org.node.moved"
	EventNodeDeleted = "org.node.deleted"

	// EventNodeRestored announces a mark-deleted node made visible again by
	// TreeService.Restore. It is deliberately NOT published for any
	// descendant a cascading delete soft-deleted alongside the restored
	// node -- see TreeService.Restore's own doc comment for why restore is
	// per-node, never cascading.
	EventNodeRestored = "org.node.restored"
)

// nodeEventDecls is the catalog entry for each of the four tree events.
//
// They are declared here, in the module's single Register call, because
// pkgcore.ComponentRegistry is where the platform's event catalog is assembled --
// observability, compliance and integration enumerate the declarations
// without subscribing to any of them. TreeService publishes all four, on the
// bus the registry hands over during Register.
var nodeEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventNodeCreated,
		PayloadType: "org.NodeCreated",
		Description: "A node was added to a tenant's organization tree.",
	},
	{
		Type:        EventNodeMoved,
		PayloadType: "org.NodeMoved",
		Description: "A node was re-parented, changing the materialized path of its whole subtree.",
	},
	{
		Type:        EventNodeDeleted,
		PayloadType: "org.NodeDeleted",
		Description: "A node, and any subtree beneath it, was removed from a tenant's organization tree.",
	},
	{
		Type:        EventNodeRestored,
		PayloadType: "org.NodeRestored",
		Description: "A previously mark-deleted node was restored and is visible again.",
	},
}

// Module implements the module contract for go/org: a tenant's organization tree,
// the memberships bound to it, and the invitations that create them.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to the assembly.
// Register then attaches the host's registry to the module's runtime, which
// is the only moment org learns about the bus, the mailer, the key-value
// store and the message catalog -- and it attaches the registry itself
// rather than the seams behind it, so every seam is read when it is used.
// That is not a stylistic preference: Registry.Locales() is documented to be
// nil while modules are registering, so a captured catalog would be a nil
// catalog and the first invitation would fail on it.
type Module struct {
	// db is the connection org's tables live in. It is opened and migrated
	// by the host before Register is ever called; the module performs no
	// I/O of its own during registration.
	db *gorm.DB

	// tree, members, invites and scope are the module's runtime. They are
	// built by NewModule -- constructing them opens nothing -- and given
	// their host seams by Register.
	tree    *TreeService
	members *MemberService
	invites *InviteService
	scope   *ScopeService

	// host is the registry Register attached, read by the runtime at call
	// time. Nil before the assembly.
	host hostSeams

	// emailIndexer is the blind indexer for invitation addresses
	// (WithEmailIndexer). Register refuses to proceed without one.
	emailIndexer *dbkit.BlindIndexer

	// subject resolves the HTTP caller's identity for handler.go's two
	// caller-scoped endpoints (WithSubjectResolver). Nil is a legal, if
	// unusable, wiring: every endpoint that needs it fails closed with
	// ErrSubjectUnresolved rather than Register refusing to boot -- a host
	// that has not wired authn yet can still boot org and exercise
	// everything else.
	subject SubjectResolver

	// handler serves org's HTTP surface. Built by Register, once every
	// Option has already run -- see Register's own doc comment for why
	// this cannot happen in NewModule.
	handler *Handler
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithEmailIndexer injects the blind indexer that makes an invitation's
// encrypted address queryable by exact match.
//
// It is REQUIRED: Register returns ErrEmailIndexerRequired without one, in
// deliberate imitation of config.Attach's ErrCipherRequired. An invitation
// org could store but never find again is worse than a boot failure.
//
// Build it with dbkit.NewBlindIndexer(EmailIndexColumn, key, dbkit.NormalizeEmail)
// -- the column argument must be the module's exported EmailIndexColumn, the
// blind-index column's exact SQL name: dbkit refuses an empty name, not a
// non-empty wrong one, so the name travels as a referenced constant rather
// than a hand-typed string (see EmailIndexColumn's own doc comment) -- and
// give it a key that is NOT the encryption key registered for
// EmailSerializerName: an AES key reused as an HMAC key weakens both.
func WithEmailIndexer(indexer *dbkit.BlindIndexer) Option {
	return func(m *Module) {
		m.emailIndexer = indexer
		m.invites.indexer = indexer
	}
}

// WithFeatureGate injects the feature-flag reader org asks about its own
// flags. *config.Service satisfies it structurally; see FeatureGate. Without
// one, the flags' declared defaults apply.
func WithFeatureGate(gate FeatureGate) Option {
	return func(m *Module) { m.invites.gate = gate }
}

// WithMaxDepth bounds how deep this host's organization trees may go, the
// tenant root counting as depth 0. Values below 1 are ignored: a tree that
// cannot hold a single child is not a tree. Values above maxDepthCeiling are
// ignored too: that ceiling is the deepest tree the path column's
// VARCHAR(1024) width can hold on EITHER dialect, and a value beyond it
// would make the standalone deployment mode's SQLite silently accept a tree
// the distributed mode's PostgreSQL would reject at the database with
// "value too long for character varying(1024)" -- see maxDepthCeiling's own
// doc comment in path.go for the arithmetic. Either kind of rejected value
// leaves maxDepth's package default in place, exactly like the below-1 case.
func WithMaxDepth(depth int) Option {
	return func(m *Module) {
		if depth >= 1 && depth <= maxDepthCeiling {
			m.tree.maxDepth = depth
		}
	}
}

// WithInvitationTTL sets how long a new invitation stays acceptable.
// Non-positive values are ignored.
func WithInvitationTTL(ttl time.Duration) Option {
	return func(m *Module) {
		if ttl > 0 {
			m.invites.ttl = ttl
		}
	}
}

// WithMailFrom sets the sender address of the invitation email. It is
// required whenever the invitation email is enabled -- pkgcore.Mailer rejects
// an empty From outright -- so Register reports ErrInvitationMailRequired
// when it is missing and WithInvitationEmailDisabled was not used.
func WithMailFrom(from string) Option {
	return func(m *Module) { m.invites.from = from }
}

// WithReplyTo sets the Reply-To address of the invitation email, so a reply
// to the invitation lands in an inbox the host reads instead of the no-reply
// sender. It is OPTIONAL and independent of WithMailFrom: without it (or
// with an empty value) the invitation carries no Reply-To header.
//
// The value is module-level configuration, not per-invitation: the natural
// reply target a person would name -- the inviter -- is not reachable from
// here. Invitation carries only InviterUserID, no address, and org resolves
// addresses through no cross-module read (go/org never imports authn), so
// the host names the address instead.
func WithReplyTo(replyTo string) Option {
	return func(m *Module) { m.invites.replyTo = replyTo }
}

// WithInvitationLinkBuilder injects the function that turns an invitation
// token into the URL the invitee clicks. Only the host knows its own public
// address, and in a multi-tenant deployment which host name belongs to the
// tenant in ctx -- so the link can point the invitee at the tenant's own
// address rather than a shared one.
//
// That per-tenant host name is desirable, not a requirement of acceptance:
// acceptance is tenantless. Accept on a request context carrying no tenant
// -- the freshly invited person's path -- resolves the invitation's own
// tenant from the token's hash through org_invitation_token_index, attaches
// it with pkgcore.WithTenant, and runs the ordinary tenant-scoped flow
// unchanged, so a link to any host entry point that serves the accept path
// is acceptable. The per-tenant address buys the recipient-facing URL --
// branding, and the tenant's own address in the invitee's inbox -- not
// acceptability.
//
// Required on the same terms as WithMailFrom.
func WithInvitationLinkBuilder(build InvitationLinkBuilder) Option {
	return func(m *Module) { m.invites.link = build }
}

// WithSubjectResolver injects the seam Handler uses to identify the HTTP
// caller for org's two caller-scoped endpoints (creating and accepting an
// invitation). See SubjectResolver's own doc comment for why an unwired
// resolver fails those two endpoints closed instead of Register refusing to
// boot.
func WithSubjectResolver(resolver SubjectResolver) Option {
	return func(m *Module) { m.subject = resolver }
}

// WithInvitationEmailDisabled flips the org.invitation_email flag's declared
// default to off, for a host that lets something else deliver the invitation
// -- the M2 notification module subscribing to org.member.invited, say.
//
// Such a host needs neither WithMailFrom nor WithInvitationLinkBuilder, and
// Register stops requiring them. Invitations are still created and still
// announced; only org's own delivery leg goes quiet.
func WithInvitationEmailDisabled() Option {
	return func(m *Module) { m.invites.emailEnabled = false }
}

// NewModule returns a Module whose tables live in db. Constructing a Module
// performs no I/O: opening and migrating db is the host's responsibility,
// done once at startup before the assembly ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	tree := NewTreeService(db)
	members := NewMemberService(db, tree)
	m := &Module{
		db:      db,
		tree:    tree,
		members: members,
		invites: NewInviteService(db, tree, members, nil),
		scope:   NewScopeService(tree, members.Repository()),
	}
	// The tree asks the roster whether a subtree is occupied before deleting
	// it, so a delete can never orphan a membership.
	tree.members = members.Repository()
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Tree returns the module's TreeService, the only sanctioned way to change a
// tenant's organization tree.
func (m *Module) Tree() *TreeService { return m.tree }

// Members returns the module's MemberService: the tenant's roster.
func (m *Module) Members() *MemberService { return m.members }

// Invitations returns the module's InviteService.
func (m *Module) Invitations() *InviteService { return m.invites }

// Scope returns the module's read-only Scope implementation, the seam
// authorization and data-visibility consumers accept structurally without
// importing org. See Scope's own doc comment for why that works.
func (m *Module) Scope() Scope { return m.scope }

// Name implements the module contract.
func (m *Module) Name() string { return moduleName }

// DependsOn implements the module contract: nothing.
//
// This is a real answer, not a stub. org sits above dbkit and tenancy in
// the module graph, but neither is a
// module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set. org must also NOT depend on
// authn: it learns about users from a domain event and an id, never from
// authn's Go types -- the canonical module-boundary example. Naming authn
// here would make org unbootable in a host
// that does not run authn, and would invert the direction that example
// exists to protect.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements the module contract: the descriptions of org's error codes
// and the invitation message, in both supported languages with identical id
// sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements the module contract: it returns org's own OpenAPI
// fragment, embedded from api/openapi.yaml. That fragment is the single
// source of this module's API surface -- the api package's generated types
// and ServerInterface (api/org-server.gen.go, regenerated by task api:gen)
// derive from it, and Handler implements that interface (see handler.go) --
// the spec-first decision.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements the module contract. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db.
//
// It contributes org's permissions, its audit vocabulary, its feature flags
// and its event catalog, subscribes to authn's user-created event, and
// attaches the registry to the module's runtime.
//
// It deliberately does NOT declare EventUserCreated. That event is authn's
// to declare; declaring it here as well would collide with authn's own
// registration (ErrDuplicateEventType) the moment both modules boot in one
// host. Subscribing is not declaring, and Subscribe cannot fail.
//
// Two wirings are validated rather than assumed, and both fail the boot:
//
//   - no blind indexer (ErrEmailIndexerRequired), because an invitation
//     whose address cannot be indexed can never be found again;
//   - the invitation email enabled with no sender address or no link builder
//     (ErrInvitationMailRequired), because the message could not be rendered
//     into anything a recipient could act on.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if m.emailIndexer == nil {
		return ErrEmailIndexerRequired
	}
	if m.invites.emailEnabled && (m.invites.from == "" || m.invites.link == nil) {
		return ErrInvitationMailRequired
	}

	if err := reg.PermissionsSeat().Add(
		PermissionRead,
		PermissionManage,
		PermissionInviteMember,
		PermissionRemoveMember,
	); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(
		AuditActionNodeCreate,
		AuditActionNodeUpdate,
		AuditActionMemberCreate,
		AuditActionMemberUpdate,
		AuditActionInvitationCreate,
		AuditActionInvitationUpdate,
	); err != nil {
		return err
	}
	if err := reg.FeaturesSeat().Add(featureFlagDecls...); err != nil {
		return err
	}
	// The process-start key material (bootstrapKeyDecl) is descriptor data:
	// the component descriptor carries it as BootstrapKeys, which the loader
	// resolves before anything is constructed.
	if err := reg.EventsSeat().Publishes(append(append([]pkgcore.EventDecl{}, nodeEventDecls...), memberEventDecls...)...); err != nil {
		return err
	}

	m.attach(reg)
	reg.EventsSeat().Subscribe(EventUserCreated, m.handleUserCreated)

	// Handler is built here, not in NewModule, deliberately: every Option a
	// caller passed to NewModule -- WithSubjectResolver above all -- has
	// already run by the time Register is called (the assembly calls Register
	// only after NewModule has returned), so the resolver Handler is given
	// is whichever one the host actually configured, never a nil one
	// captured before the option ran.
	m.handler = NewHandler(m.tree, m.members, m.invites, m.subject)
	reg.RoutesSeat().Mount(apiPath, m.handler)
	return nil
}

// attach hands the host's registry to every part of the runtime that needs a
// host seam. It stores the registry, never the seams behind it -- see the
// Module type's own doc comment for why that distinction is load-bearing.
func (m *Module) attach(host hostSeams) {
	m.host = host
	m.tree.host = host
	m.members.host = host
	m.invites.host = host
}
