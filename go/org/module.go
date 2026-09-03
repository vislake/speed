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

// moduleName is org's pkgcore.Module.Name(). It is also the key
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

// The audit actions org contributes to the audit vocabulary. Every
// structural change to a tenant's tree is auditable, because a node move
// silently changes what its members can see.
const (
	AuditActionNodeCreate = "org.node.create"
	AuditActionNodeRename = "org.node.rename"
	AuditActionNodeMove   = "org.node.move"
	AuditActionNodeDelete = "org.node.delete"

	AuditActionMemberInvite = "org.member.invite"
	AuditActionMemberAccept = "org.member.accept"
	AuditActionMemberRemove = "org.member.remove"
)

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

// FeatureGate reports whether a feature flag is enabled for the tenant in
// ctx.
//
// It is the same no-import technique as Scope, in the other direction: the
// signature is built from stdlib types only, so *config.Service satisfies it
// structurally through its own IsEnabled method. org never imports config --
// docs/internal/01-architecture.md's graph has no org -> config edge, and
// config sits beside org rather than beneath it -- and config never learns
// that org exists. The host passes one to the other, and the host is the only
// place both names appear.
//
// A nil gate means org honors the flags' declared defaults, which is what a
// host running org without the config module gets.
type FeatureGate interface {
	IsEnabled(ctx context.Context, key string) (bool, error)
}

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
)

// nodeEventDecls is the catalog entry for each of the three tree events.
//
// They are declared here, in the module's single Register call, because
// pkgcore.Registry is where the platform's event catalog is assembled --
// observability, compliance and integration enumerate the declarations
// without subscribing to any of them. TreeService publishes all three, on the
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
}

// Module implements pkgcore.Module for go/org: a tenant's organization tree,
// the memberships bound to it, and the invitations that create them.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
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
	// time. Nil before Bootstrap.
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
// Build it with dbkit.NewBlindIndexer("email_index", key, dbkit.NormalizeEmail),
// and give it a key that is NOT the encryption key registered for
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

// WithInvitationLinkBuilder injects the function that turns an invitation
// token into the URL the invitee clicks. Only the host knows its own public
// address, and in a multi-tenant deployment which host name belongs to the
// tenant in context -- which matters, because acceptance is resolved inside
// the tenant of the request and never from the token.
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
// done once at startup before Bootstrap ever calls Register.
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

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing.
//
// This is a real answer, not a stub. org sits above dbkit and tenancy in
// docs/internal/01-architecture.md's graph, but neither is a
// pkgcore.Module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set. org must also NOT depend on
// authn: it learns about users from a domain event and an id, never from
// authn's Go types, which is the canonical module-boundary example the root
// CLAUDE.md gives. Naming authn here would make org unbootable in a host
// that does not run authn, and would invert the direction that example
// exists to protect.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of org's error codes
// and the invitation message, in both supported languages with identical id
// sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: it returns org's own OpenAPI
// fragment, embedded from api/openapi.yaml. That fragment is the single
// source of this module's API surface -- the api package's generated types
// and ServerInterface (api/org-server.gen.go, regenerated by task api:gen)
// derive from it, and Handler implements that interface (see handler.go) --
// per docs/internal/21-api-contract.md's spec-first decision. org is the
// second module to ship a fragment, after notes.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's contract it only
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
func (m *Module) Register(reg *pkgcore.Registry) error {
	if m.emailIndexer == nil {
		return ErrEmailIndexerRequired
	}
	if m.invites.emailEnabled && (m.invites.from == "" || m.invites.link == nil) {
		return ErrInvitationMailRequired
	}

	if err := reg.Permissions.Add(
		PermissionRead,
		PermissionManage,
		PermissionInviteMember,
		PermissionRemoveMember,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionNodeCreate,
		AuditActionNodeRename,
		AuditActionNodeMove,
		AuditActionNodeDelete,
		AuditActionMemberInvite,
		AuditActionMemberAccept,
		AuditActionMemberRemove,
	); err != nil {
		return err
	}
	if err := reg.Features.Add(featureFlagDecls...); err != nil {
		return err
	}
	if err := reg.Events.Publishes(append(append([]pkgcore.EventDecl{}, nodeEventDecls...), memberEventDecls...)...); err != nil {
		return err
	}

	m.attach(reg)
	reg.Events.Subscribe(EventUserCreated, m.handleUserCreated)

	// Handler is built here, not in NewModule, deliberately: every Option a
	// caller passed to NewModule -- WithSubjectResolver above all -- has
	// already run by the time Register is called (Bootstrap calls Register
	// only after NewModule has returned), so the resolver Handler is given
	// is whichever one the host actually configured, never a nil one
	// captured before the option ran.
	m.handler = NewHandler(m.tree, m.members, m.invites, m.subject)
	reg.Routes.Mount(apiPath, m.handler)
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

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
