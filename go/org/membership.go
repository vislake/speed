package org

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// tableMemberships is the memberships table name, shared by the model's
// TableName and by the migrations' header comments.
const tableMemberships = "memberships"

// The lifecycle states a Membership can be in.
//
// The set is closed and lives here rather than in a database enum type,
// which PostgreSQL has and SQLite does not: the column is a plain VARCHAR on
// both engines and this package is what constrains its contents.
const (
	// MembershipStatusActive is a member who is in the tenant right now. It
	// is the only status Scope.MemberNodeIDs grants data visibility for.
	MembershipStatusActive = "active"

	// MembershipStatusInvited is a placeholder for a person who has been
	// invited but has not accepted yet. org does not write it today --
	// InviteService keeps a pending invitation in org_invitations and creates
	// the membership only on acceptance, so an unaccepted invitee occupies no
	// seat -- and the constant exists so that a host reading the column knows
	// the full vocabulary it may one day hold.
	MembershipStatusInvited = "invited"

	// MembershipStatusSuspended is a member kept on the roster but denied
	// visibility, the state an administrator parks somebody in instead of
	// removing them. Scope.MemberNodeIDs returns nothing for them.
	MembershipStatusSuspended = "suspended"
)

// Membership binds one person to one place in one tenant's organization
// tree. It is the row that makes "which tenants do I belong to" answerable
// without any table knowing about both sides.
//
// # Data domain
//
// Link data (docs/internal/04-data-and-tenancy.md), and link data is
// tenant-scoped: that document's data-domain table classifies it as
// isolated by tenant_id and states outright that AssertIsolated is mandatory
// for tenant data AND link data both. So Membership implements
// dbkit.TenantScoped, is reached only through MembershipRepository, and its
// isolation is proven by tenancytest.AssertIsolated -- never by
// AssertNotTenantScoped, which would assert the opposite of the requirement.
//
// The confusion this note exists to prevent: `users` is identity data and
// deliberately NOT tenant-scoped, because one person belongs to several
// tenants. That is precisely why this bridging row must be tenant-scoped --
// it is the per-tenant half of that relationship, and a membership visible
// across tenants would expose one tenant's roster to another.
//
// # Cross-module references
//
// UserID names a row in authn's users table and carries NO foreign key.
// Cross-module foreign keys are forbidden (docs/internal/04, rule 4): they
// make independently released migrations and cascading deletes unmanageable.
// org learns a user id from an authenticated caller or from a domain event,
// and never imports an authn type to hold it -- the canonical example the
// root CLAUDE.md gives for the module-boundary rule.
//
// NodeID names an OrgNode of the same tenant and likewise carries no
// database-level foreign key, for the same dual-dialect reason
// Repository.deleteLeaf documents: SQLite leaves foreign keys unenforced
// unless the connection turns them on, so a constraint present on one engine
// and absent on the other would make the two deployment modes behave
// differently. TreeService.Delete is what keeps the reference honest.
//
// # What is deliberately absent
//
// Roles. docs/internal/05-identity-and-access.md sketches a Roles []string
// field here; org does not store one. Native arrays are banned dual-dialect,
// and more importantly role state belongs to rbac's policy store keyed by
// tenant, user and node path. org answers "where in the tree is this
// person"; rbac answers "what may they do there".
type Membership struct {
	// ID is an application-generated UUID, drawn from the same lowercase
	// alphabet path.go pins for node ids.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// UserID is the authn user this membership belongs to. It is an opaque
	// string to org: no format is assumed, nothing is parsed out of it, and
	// it is never used to build a path.
	UserID string `gorm:"column:user_id;size:64;not null"`

	// NodeID is the OrgNode this member sits at. Their data scope is that
	// node's whole subtree -- see ScopeService.MemberNodeIDs.
	NodeID string `gorm:"column:node_id;size:36;not null"`

	// Status is one of the MembershipStatus* constants above.
	Status string `gorm:"column:status;size:16;not null"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime, never by application code and never by a NOW() default.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the memberships table.
func (Membership) TableName() string { return tableMemberships }

// IsActive reports whether the membership grants visibility right now.
func (m Membership) IsActive() bool { return m.Status == MembershipStatusActive }

// compile-time check that Membership satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Membership{}

// MembershipRepository is org's tenant-scoped data-access type for
// Membership.
//
// It embeds *dbkit.Repository[Membership] and adds the three query shapes
// that surface cannot express, written exactly the way Repository (for
// OrgNode) documents: composed on the same *gorm.DB the isolation plugin
// protects, against a TenantScoped destination, inside
// dbkit.WithTenantSession. No hand-written tenant_id predicate exists in
// this file, and no db.Table / db.Model / db.Raw call does either -- go/org
// carries no allowlist entry in either semgrep rule, deliberately.
type MembershipRepository struct {
	*dbkit.Repository[Membership]

	// db is the same connection the embedded Repository was built on, kept
	// only so the extra query shapes can be composed on it.
	db *gorm.DB
}

// NewMembershipRepository returns a MembershipRepository backed by db, which
// is expected to come from dbkit.Open with this module's migrations applied.
func NewMembershipRepository(db *gorm.DB) *MembershipRepository {
	return &MembershipRepository{Repository: dbkit.NewRepository[Membership](db), db: db}
}

// byUser returns userID's membership in the caller's tenant, or
// ErrMembershipNotFound. At most one row can match: the table carries
// UNIQUE(tenant_id, user_id).
func (r *MembershipRepository) byUser(ctx context.Context, userID string) (*Membership, error) {
	var m Membership
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).First(&m).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrMembershipNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &m, nil
}

// byNodeIDs returns every membership bound to one of nodeIDs, ordered by
// (node_id, user_id) so a listing is stable across engines and runs. An
// empty nodeIDs returns no rows without touching the database.
//
// This is how a subtree roster is read: the caller resolves the subtree's
// node ids first (ScopeService.DescendantIDs, one indexed prefix scan) and
// passes them here. Deliberately NOT a join against org_nodes: the isolation
// plugin injects its tenant predicate for the statement's primary model, so
// a joined table's own tenant filter would have to be hand-written -- which
// is exactly the bypass the multi-tenancy rules forbid. Two indexed queries
// that are each fully protected beat one join that is not.
func (r *MembershipRepository) byNodeIDs(ctx context.Context, nodeIDs []string) ([]Membership, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var out []Membership
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("node_id IN ?", nodeIDs).
			Order("node_id, user_id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// activeSample returns at most limit active memberships of the caller's
// tenant. It exists so the "is this the last member?" question can be
// answered by reading two rows instead of counting a whole roster -- and
// without a db.Model call, which Count would require and which the raw-GORM
// bypass rule forbids this package.
func (r *MembershipRepository) activeSample(ctx context.Context, limit int) ([]Membership, error) {
	var out []Membership
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("status = ?", MembershipStatusActive).
			Order("user_id").
			Limit(limit).
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// anyInNodes reports whether any membership of the caller's tenant is bound
// to one of nodeIDs. It reads at most one row: the question is "is anybody
// there", not "how many".
//
// It is TreeService's nodeMemberGuard, which is why the signature is the
// stdlib-typed one that interface declares.
func (r *MembershipRepository) anyInNodes(ctx context.Context, nodeIDs []string) (bool, error) {
	if len(nodeIDs) == 0 {
		return false, nil
	}
	var found []Membership
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("node_id IN ?", nodeIDs).Limit(1).Find(&found).Error
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return len(found) > 0, nil
}

// compile-time check that the roster answers the question the tree asks
// before a delete.
var _ nodeMemberGuard = (*MembershipRepository)(nil)

// MemberService is the membership half of org's runtime: who belongs to the
// tenant, where in its tree they sit, and how they leave.
//
// Like TreeService it takes no tenant parameter anywhere; the tenant comes
// from the context and nothing else. Every method that changes the roster
// publishes the matching domain event, so authn can drop the removed user's
// tokens and notification can tell somebody -- neither of which org calls
// into, and neither of which org imports.
type MemberService struct {
	// repo is the tenant-scoped data-access type for memberships.
	repo *MembershipRepository

	// tree resolves node ids and subtrees. It is the same TreeService the
	// module exposes, so a membership can never be bound to a node of
	// another tenant: the lookup that validates the node is itself
	// tenant-scoped.
	tree *TreeService

	// host is the lazily-read view of the host's Registry. It is read at
	// call time, never captured at Register time -- see hostSeams.
	host hostSeams

	// newID generates membership ids, a field for the same reason
	// TreeService.newID is one: tests pin it, and the alphabet matters.
	newID func() string
}

// NewMemberService returns a MemberService over db.
//
// The returned service publishes no events until a host wires it through
// Module.Register: event publishing needs the bus the registry owns, and
// reading it at construction time would capture whatever the host had not
// installed yet.
func NewMemberService(db *gorm.DB, tree *TreeService) *MemberService {
	return &MemberService{repo: NewMembershipRepository(db), tree: tree, newID: uuid.NewString}
}

// Repository returns the service's data-access type, for callers that need
// the promoted dbkit.Repository[Membership] surface -- a host's own
// isolation test, for one -- rather than a roster operation.
func (s *MemberService) Repository() *MembershipRepository { return s.repo }

// Get returns userID's membership in the caller's tenant, or
// ErrMembershipNotFound.
func (s *MemberService) Get(ctx context.Context, userID string) (*Membership, error) {
	return s.repo.byUser(ctx, userID)
}

// Add binds userID to nodeID as an active member of the caller's tenant.
//
// It reports ErrMembershipExists when the user already has a membership in
// this tenant -- one seat per person per tenant, which is what makes "where
// does this person sit" a single answer -- and ErrNodeNotFound when nodeID
// is not a node of this tenant. A node id belonging to another tenant
// reports ErrNodeNotFound as well, because the lookup is tenant-scoped.
func (s *MemberService) Add(ctx context.Context, userID, nodeID string) (*Membership, error) {
	m, created, err := s.ensure(ctx, userID, nodeID)
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, ErrMembershipExists.WithParam("user_id", userID)
	}
	return m, nil
}

// ensure idempotently gives userID an active membership at nodeID and
// reports whether it created one. An existing membership is returned
// untouched -- it is NOT re-bound to nodeID, because a redelivered event
// must never silently move a person somewhere else in the tree.
//
// It is the shared core of Add and of the authn.user.created subscriber,
// whose whole resilience contract rests on being safely repeatable.
func (s *MemberService) ensure(ctx context.Context, userID, nodeID string) (*Membership, bool, error) {
	if userID == "" {
		return nil, false, ErrMembershipNotFound.WithParam("user_id", userID)
	}
	switch existing, err := s.repo.byUser(ctx, userID); {
	case err == nil:
		return existing, false, nil
	case !hasCode(err, ErrMembershipNotFound.Code):
		return nil, false, err
	}

	node, err := s.tree.Get(ctx, nodeID)
	if err != nil {
		return nil, false, err
	}

	id := s.newID()
	if err := validateNodeID(id); err != nil {
		return nil, false, ErrInternal.WithCause(err)
	}
	m := &Membership{
		ID:     id,
		UserID: userID,
		NodeID: node.ID,
		Status: MembershipStatusActive,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// Lost the race against a concurrent create of the same
			// membership. The unique index is the backstop behind the
			// byUser pre-check above; report the row that won rather than
			// an error, so ensure stays idempotent under concurrency and
			// not only under sequential redelivery.
			winner, findErr := s.repo.byUser(ctx, userID)
			if findErr != nil {
				return nil, false, findErr
			}
			return winner, false, nil
		}
		return nil, false, err
	}
	return m, true, nil
}

// List returns every membership bound to nodeID or to any node beneath it,
// ordered by (node_id, user_id).
//
// This is the subtree roster a manager sees: standing at a group node
// returns the members of every store under it, standing at one store returns
// that store's members alone. It reports ErrNodeNotFound when nodeID is not
// a node of the caller's tenant.
func (s *MemberService) List(ctx context.Context, nodeID string) ([]Membership, error) {
	subtree, err := s.tree.Subtree(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(subtree))
	for _, n := range subtree {
		ids = append(ids, n.ID)
	}
	return s.repo.byNodeIDs(ctx, ids)
}

// Remove deletes userID's membership from the caller's tenant and publishes
// org.member.removed.
//
// It refuses (ErrMemberNotRemovable) to remove the tenant's last active
// member: inviting somebody requires an authenticated member, so a tenant
// emptied this way could never be re-entered through the product.
//
// org does NOT invalidate the removed user's sessions -- it publishes the
// event and authn, which owns session state, subscribes. Reaching into
// another module's state is what the event exists to avoid.
func (s *MemberService) Remove(ctx context.Context, userID string) error {
	m, err := s.repo.byUser(ctx, userID)
	if err != nil {
		return err
	}
	if m.IsActive() {
		// Two rows are enough to answer "is anybody else still active?".
		active, sampleErr := s.repo.activeSample(ctx, 2)
		if sampleErr != nil {
			return sampleErr
		}
		if len(active) < 2 {
			return ErrMemberNotRemovable.WithParam("user_id", userID)
		}
	}
	if err := s.repo.Delete(ctx, m.ID); err != nil {
		return err
	}
	s.publish(ctx, EventMemberRemoved, MemberRemoved{
		MembershipID: m.ID,
		UserID:       m.UserID,
		NodeID:       m.NodeID,
	})
	return nil
}

// publish emits one member event on the host's bus, if a host is wired.
//
// A failed publish is logged and swallowed on purpose: the roster change is
// already committed, so returning the bus error would tell the caller their
// write failed when it did not. The log line is the operator's signal that a
// subscriber missed a fact.
func (s *MemberService) publish(ctx context.Context, eventType string, payload any) {
	publishEvent(ctx, s.host, eventType, payload)
}

// tenantOf returns the tenant the context carries, for an event payload's own
// TenantID. Every path into publish has already made a tenant-scoped database
// call, so the tenant is present; a missing one is reported as a failure to
// publish rather than being papered over with an empty string.
func tenantOf(ctx context.Context) (pkgcore.TenantID, error) {
	return pkgcore.MustTenantFromContext(ctx)
}
