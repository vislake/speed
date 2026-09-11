package org

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
// Link data, and link data is tenant-scoped: a bridging row is isolated by
// tenant_id exactly like tenant data, so AssertIsolated is mandatory for it.
// Membership implements
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
// Cross-module foreign keys are forbidden: they
// make independently released migrations and cascading deletes unmanageable.
// org learns a user id from an authenticated caller or from a domain event,
// and never imports an authn type to hold it -- the canonical
// module-boundary example.
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
// Roles. The design sketches a Roles []string
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

	// DeletedAt and DeletedBy are dbkit.SoftDeletable's required pair
	// (go/dbkit/soft_delete.go): implementing that interface below is what
	// makes dbkit.Repository[Membership].Delete -- promoted unchanged and
	// called by MemberService.Remove -- a mark-delete instead of a
	// physical DELETE, and what makes dbkit.Repository[Membership].Restore
	// -- and MemberService.Restore, which wraps it -- meaningful for this
	// model. Neither field is ever set by application code directly: both
	// writes go through dbkit's own reflection-based field access, exactly
	// as TenantID does.
	//
	// uq_memberships_tenant_user became a partial index scoped
	// WHERE deleted_at IS NULL in the same migration that adds these two
	// columns (migrations/{postgres,sqlite}/0004_add_soft_delete.sql), so a
	// removed member's seat frees up immediately for a fresh Add, instead
	// of staying reserved by a row nobody can see.
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

// TableName names the memberships table.
func (Membership) TableName() string { return tableMemberships }

// IsActive reports whether the membership grants visibility right now.
func (m Membership) IsActive() bool { return m.Status == MembershipStatusActive }

// GetDeletedAt returns Membership's soft-delete marker, satisfying
// dbkit.SoftDeletable. Like GetTenantID, this is never called by dbkit's
// soft-delete auto-scope plugin or by Repository[Membership] itself -- it is
// a pure marker used only for the capability check that routes
// dbkit.Repository[Membership].Delete onto the mark-delete path; the actual
// field writes go through reflection on fixed field names.
func (m Membership) GetDeletedAt() *time.Time { return m.DeletedAt }

// AuditResourceType implements dbkit.Auditable: it names org's audit
// resource kind "org.member", the label dbkit's automatic GORM write-capture
// plugin attaches to every Membership write's WriteCapturedEvent -- from
// which go/dbkit/audit's persister derives the declared "org.member.create"
// and "org.member.update" actions (module.go's audit-action block, composed
// from the labels there). A member addition records as "org.member.create";
// a removal and a restore are mark-delete/mark-clear UPDATEs underneath and
// both record as "org.member.update", distinguishable by the written
// columns in the changes diff (deleted_at set vs. cleared). Captured only
// when a host wires dbkit.Options.AuditBus on org's connection AND sets
// that connection's Options.AuditModels scope from org.AuditableModels()
// -- the reference app is the first host fulfilling the contract.
func (Membership) AuditResourceType() string { return AuditResourceTypeMember }

// compile-time check that Membership satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Membership{}

// compile-time check that Membership satisfies dbkit.SoftDeletable.
var _ dbkit.SoftDeletable = Membership{}

// compile-time check that Membership satisfies dbkit.Auditable.
var _ dbkit.Auditable = Membership{}

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

// errLastActiveMember and errMembershipAlreadyGone are removeIfNotLastActive's
// internal signals, translated by its caller. Neither escapes this file.
var (
	errLastActiveMember      = errors.New("org: removing this membership would leave the tenant with no active member")
	errMembershipAlreadyGone = errors.New("org: membership already removed")
)

// removeIfNotLastActive mark-deletes membershipID -- the caller's own
// byUser read already confirmed it is currently active -- unless doing so
// would leave the tenant with no active member at all, in which case it
// refuses (errLastActiveMember) and changes nothing.
//
// # The race this closes
//
// A split check and write -- activeSample(ctx, 2) in its own transaction (a
// plain read) followed by a separate Delete call in another -- leaves this
// window open: with a tenant at exactly two active members, two goroutines
// each removing one of them could both read "2 active" before either
// committed its own delete, both pass the "at least 2" check, and both
// proceed -- leaving zero active members, which is permanently unrecoverable
// (invitations require an authenticated member; getting a token requires
// active membership). ErrMemberNotRemovable's guarantee, split that way,
// holds only for a single, serial caller.
//
// # One arbitrated lock, then the write, both in one transaction
//
// Two writes, and deliberately NO read of anything in between them, so
// this whole transaction's first database statement is a write -- the
// shape that keeps it out of SQLite's read-then-write lock-upgrade hazard
// (lockLiveNode's own
// doc comment in repository.go explains the identical reasoning for the
// tree writes).
//
//  1. A blind, no-op bulk touch-update of EVERY currently-active
//     membership row in the tenant (status = 'active' AND deleted_at IS
//     NULL, rewriting DeletedBy to the empty string it already holds for
//     any live row -- the same no-op-write-as-lock trick lockLiveNode
//     uses). Its RowsAffected is the tenant's current active-member count,
//     read for free from the very statement that also LOCKS every one of
//     those rows: on PostgreSQL each matched row's write lock is held
//     until this transaction ends, so a concurrent Remove of any OTHER
//     active member -- which touches this SAME statement's row set,
//     since it too is bulk-touching every active row -- blocks behind
//     this transaction rather than reading a stale, pre-commit count; on
//     SQLite, being the transaction's first statement, it is an ordinary
//     contending writer that waits out busy_timeout rather than one that
//     can be refused immediately. Two concurrent Removes at a
//     two-active-member tenant are therefore genuinely serialized by this
//     one bulk statement, not merely by accident of timing.
//  2. If the count from step 1 is less than 2, the removal is refused
//     (errLastActiveMember) and the whole transaction rolls back, changing
//     nothing -- exactly as if step 1 had never run. Otherwise, the
//     second write: the actual, targeted soft-delete of membershipID,
//     conditioned on it still being a live row (deleted_at IS NULL) --
//     RowsAffected == 0 here means a concurrent caller already removed
//     this SAME membership (errMembershipAlreadyGone), which is a
//     different, narrower race than the one this method exists to close.
//
// This does mean every Remove of an active member briefly write-locks
// every OTHER active membership row of the tenant too, not just its own
// target -- a real, deliberately accepted cost of getting a correct
// database-arbitrated answer without a version column or a second module.

func (r *MembershipRepository) removeIfNotLastActive(ctx context.Context, membershipID string) error {
	now := time.Now()
	deletedBy := softDeleteActor(ctx)
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		lockRes := tx.
			Where("status = ?", MembershipStatusActive).
			Where("deleted_at IS NULL").
			Select("DeletedBy").
			// Omit UpdatedAt, for the identical reason touchLockByID's own
			// lock does (repository.go): this is a no-op WRITE whose only
			// purpose is the row lock and the free RowsAffected count, and
			// without the exclusion GORM's autoUpdateTime machinery stamps
			// updated_at = now() onto every active member's row on every
			// Remove of any one of them.
			Omit("UpdatedAt").
			Updates(&Membership{DeletedBy: ""})
		if lockRes.Error != nil {
			return lockRes.Error
		}
		if lockRes.RowsAffected < 2 {
			return errLastActiveMember
		}

		delRes := tx.
			Where("id = ?", membershipID).
			Where("deleted_at IS NULL").
			Select("DeletedAt", "DeletedBy").
			Updates(&Membership{DeletedAt: &now, DeletedBy: deletedBy})
		if delRes.Error != nil {
			return delRes.Error
		}
		if delRes.RowsAffected == 0 {
			return errMembershipAlreadyGone
		}
		return nil
	})
	switch {
	case errors.Is(err, errLastActiveMember), errors.Is(err, errMembershipAlreadyGone):
		return err
	case err != nil:
		return ErrInternal.WithCause(err)
	}
	return nil
}

// anyInNodesTx reports whether any membership of the caller's tenant is
// bound to one of nodeIDs, run against an already-open transaction instead
// of opening its own. It reads at most one row: the question is "is anybody
// there", not "how many". It is TreeService's nodeMemberGuard: the delete
// locks the subtree first and calls this INSIDE that same transaction, right
// after the lock succeeds and before the bulk mark-delete statement runs --
// see tree.go's Delete doc comment for why a separate, ctx-bound call
// opening its own transaction would leave the exact TOCTOU window the
// in-transaction guard closes.
func (r *MembershipRepository) anyInNodesTx(tx *gorm.DB, nodeIDs []string) (bool, error) {
	if len(nodeIDs) == 0 {
		return false, nil
	}
	var found []Membership
	if err := tx.Where("node_id IN ?", nodeIDs).Limit(1).Find(&found).Error; err != nil {
		return false, err
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

// errMembershipInsertLostRace is ensure's internal signal that the
// membership insert collided with a concurrent create of the same
// (tenant, user) row on the partial unique index. It is returned from
// the transaction body so the transaction ends the moment the race is
// lost, and translated by ensure's own recovery below; it never escapes
// this file.
var errMembershipInsertLostRace = errors.New("org: membership insert lost its unique-index race")

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

// EnsureRootSeat ensures userID holds an active seat at the caller tenant's
// root, creating the tenant's tree root -- named rootName, kind rootKind --
// when the tenant has none yet.
//
// It is "make this person a member of this tenant" as one idempotent call,
// for the boot-time and provisioning paths that place a person before any
// organization tree exists: repeated calls create nothing twice and move
// nobody. The root comes from TreeService.EnsureRoot, so a tenant that
// already has a tree keeps its stored root whatever name the caller passes;
// the seat comes from the same ensure core Add and the authn.user.created
// subscriber share, so a repeat returns the existing membership untouched
// wherever the person sits -- one seat per person per tenant, and an
// ensure never drags a member out of a deeper node a later flow placed them
// in.
//
// A membership created here is not announced as org.member.joined: like Add,
// this call writes the row silently, and the caller that needs the event
// published is the caller that publishes it (the authn.user.created
// subscriber is that caller for the accounts it provisions).
func (s *MemberService) EnsureRootSeat(ctx context.Context, userID, rootName, rootKind string) (*Membership, error) {
	root, err := s.tree.EnsureRoot(ctx, rootName, rootKind)
	if err != nil {
		return nil, err
	}
	membership, _, err := s.ensure(ctx, userID, root.ID)
	if err != nil {
		return nil, err
	}
	return membership, nil
}

// ensure idempotently gives userID an active membership at nodeID and
// reports whether it created one. An existing membership is returned
// untouched -- it is NOT re-bound to nodeID, because a redelivered event
// must never silently move a person somewhere else in the tree.
//
// It is the shared core of Add and of the authn.user.created subscriber,
// whose whole resilience contract rests on being safely repeatable.
//
// # Locking nodeID before creating the membership
//
// A plain, unlocked s.tree.Get(nodeID) read followed by a separate
// s.repo.Create -- two independent statements with nothing between them
// contending for any lock at all -- would leave a real TOCTOU window
// against TreeService.Delete: Delete's own subtree scan and mark-delete run
// in ONE transaction, but a plain read of nodeID here could observe the
// node as live in the gap before that transaction commits, and this call's
// own Create -- writing to a completely different table (memberships), with
// no lock relationship to org_nodes at all -- would then land regardless of
// what Delete's transaction was doing, leaving a membership bound to a row
// Delete's cascade was already committing as mark-deleted. See tree.go's
// Delete doc comment for the other half of this same guard.
//
// This call takes the identical lockLiveNode lock Delete's own transaction
// takes on nodeID, inside ONE transaction that also creates the membership:
// either this call's lock wins first (forcing a concurrent Delete of the
// same node to wait behind it, so the membership it creates is never
// invisible by the time Delete's own guard re-checks), or Delete's lock won
// first (so this call blocks behind it and, once it resumes, correctly
// finds nodeID mark-deleted and reports ErrNodeNotFound instead of
// completing the insert). withRetry covers the same SQLite-contention and
// PostgreSQL-deadlock cases every other lockLiveNode caller in this module
// already retries through.
//
// # The insert race: the database is the backstop, and the recovery must
// leave the failed transaction behind
//
// The byUser pre-check above and the insert are not atomic: two concurrent
// ensures of the same (tenant, user) can both pass the pre-check before
// either has written, and uq_memberships_tenant_user -- the partial unique
// index on (tenant_id, user_id) WHERE deleted_at IS NULL -- is the backstop
// that admits exactly one of their inserts. The loser of that race reports
// the row that won rather than an error, so ensure stays idempotent under
// concurrency and not only under sequential redelivery. That recovery used
// to re-read the winner on the SAME transaction as the failed insert --
// tolerated by SQLite, where a failed statement does not poison the
// transaction around it, and broken on PostgreSQL, where a unique-violation
// error aborts the whole transaction and the follow-up read died with
// SQLSTATE 25P02 ("current transaction is aborted") in exactly the
// concurrent case the branch exists to absorb. The insert therefore ends
// its transaction the moment it loses the race (errMembershipInsertLostRace,
// which the enclosing WithTenantSession rolls back), and the winner is
// re-read on a FRESH session; when that read finds nothing -- a concurrent
// Remove mark-deleted the winning row between the winner's commit and the
// read -- the seat is free again and the whole insert is re-attempted,
// bounded by the same txRetryBudget the contention retries draw on and
// answering ErrConcurrentUpdate on exhaustion, exactly as withRetry's own
// does.
func (s *MemberService) ensure(ctx context.Context, userID, nodeID string) (*Membership, bool, error) {
	if userID == "" {
		return nil, false, ErrMembershipNotFound.WithParam("user_id", userID)
	}
	switch existing, err := s.repo.byUser(ctx, userID); {
	case err == nil:
		return existing, false, nil
	case !apperr.HasCode(err, ErrMembershipNotFound.Code):
		return nil, false, err
	}

	id := s.newID()
	if err := validateNodeID(id); err != nil {
		return nil, false, ErrInternal.WithCause(err)
	}

	var created *Membership
	for attempt := 0; attempt < txRetryBudget; attempt++ {
		created = nil
		err := withRetry(func() error {
			return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
				node, lockErr := lockLiveNode(tx, nodeID)
				if lockErr != nil {
					if errors.Is(lockErr, gorm.ErrRecordNotFound) {
						return ErrNodeNotFound.WithParam("node_id", nodeID)
					}
					return ErrInternal.WithCause(lockErr)
				}

				m := &Membership{
					ID:     id,
					UserID: userID,
					NodeID: node.ID,
					Status: MembershipStatusActive,
				}
				if createErr := tx.Create(m).Error; createErr != nil {
					if errors.Is(createErr, gorm.ErrDuplicatedKey) {
						// Lost the race against a concurrent create of the
						// same membership. The unique index is the backstop
						// behind the byUser pre-check above. End the
						// transaction HERE: on PostgreSQL the
						// unique-violation error has already aborted it, so
						// any further statement on tx -- a re-read of the
						// winner included -- fails with SQLSTATE 25P02. The
						// winner is resolved below, on a fresh session.
						return errMembershipInsertLostRace
					}
					return ErrInternal.WithCause(createErr)
				}
				created = m
				return nil
			})
		})
		if err == nil {
			return created, true, nil
		}
		if !errors.Is(err, errMembershipInsertLostRace) {
			return nil, false, err
		}
		// The insert collided with a row the winner committed (an
		// uncommitted rival would have made this insert wait, not fail), so
		// this read reports the row that won rather than an error and keeps
		// ensure idempotent under concurrency, not only under sequential
		// redelivery.
		switch existing, readErr := s.repo.byUser(ctx, userID); {
		case readErr == nil:
			return existing, false, nil
		case !apperr.HasCode(readErr, ErrMembershipNotFound.Code):
			return nil, false, readErr
		}
		// The colliding row is already gone -- a concurrent Remove
		// mark-deleted it between the winner's commit and the read above --
		// so the seat is free again and this loop's next attempt re-runs
		// the whole insert from a clean transaction.
	}
	return nil, false, ErrConcurrentUpdate.WithCause(errMembershipInsertLostRace)
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
// emptied this way could never be re-entered through the product. That
// refusal is enforced by removeIfNotLastActive as one database-arbitrated
// transaction -- see its own doc comment in repository.go for why a
// separate read-then-check followed by a separate delete would let two
// concurrent removals of a tenant's last two active members both succeed.
//
// org does NOT invalidate the removed user's sessions -- it publishes the
// event and authn, which owns session state, subscribes. Reaching into
// another module's state is what the event exists to avoid.
func (s *MemberService) Remove(ctx context.Context, userID string) error {
	m, err := s.repo.byUser(ctx, userID)
	if err != nil {
		return err
	}
	if !m.IsActive() {
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

	switch err := s.repo.removeIfNotLastActive(ctx, m.ID); {
	case errors.Is(err, errLastActiveMember):
		return ErrMemberNotRemovable.WithParam("user_id", userID)
	case errors.Is(err, errMembershipAlreadyGone):
		return ErrMembershipNotFound.WithParam("user_id", userID)
	case err != nil:
		return err
	}
	s.publish(ctx, EventMemberRemoved, MemberRemoved{
		MembershipID: m.ID,
		UserID:       m.UserID,
		NodeID:       m.NodeID,
	})
	return nil
}

// Restore makes a previously removed membership visible again, wrapping the
// promoted dbkit.Repository[Membership].Restore and re-reading the row so
// the caller gets its current data back, exactly as MemberService's own
// write methods return the roster row they touched.
//
// It takes the membership's own id, never a user id: Remove hides the row
// from byUser's ordinary, scope-filtered lookup the moment it mark-deletes
// it, so "restore this user's membership" is not answerable by user id alone
// once a person has been removed and possibly re-added since -- each
// removal leaves its own, separately soft-deleted row, and only the id names
// one of them unambiguously. The id is available from the Add/ensure call
// that created the row, or from the org.member.removed event's own
// MembershipID field.
//
// It reports ErrMembershipNotFound both for an id with nothing to restore
// and for an id that exists but is not currently mark-deleted -- the
// identical collapsed signal dbkit.Repository[T].Restore's own doc comment
// describes, so a caller cannot learn which case it hit from the error shape
// alone.
//
// Restore does not re-validate the restored membership against Add's own
// rules: the tenant's node still existing was true when the row was created
// and Restore changes nothing about the row but its two soft-delete columns.
// The one-seat rule is different -- the partial unique index on
// (tenant_id, user_id) WHERE deleted_at IS NULL (0004_add_soft_delete.sql)
// exists precisely so a removed member's seat can be RE-TAKEN by a fresh
// Add, and restoring the earlier row into an occupied seat collides at the
// database. That collision is the seat rule enforced by the database (the
// same race ensure's own duplicate handling covers in the other direction)
// and reports the coded ErrMembershipExists -- never a bare database error.
// A caller wanting the modern invariants re-checked calls Add instead of
// Restore.
//
// Restore is deliberately not exposed over HTTP; it is a Service-level
// call only.
func (s *MemberService) Restore(ctx context.Context, membershipID string) (*Membership, error) {
	if err := s.repo.Restore(ctx, membershipID); err != nil {
		if dbkit.IsRecordNotFound(err) {
			return nil, ErrMembershipNotFound.WithParam("membership_id", membershipID)
		}
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// The removed row's seat was re-taken by a live membership since
			// the removal: restoring would put two live rows on one
			// (tenant, user), and the partial unique index refused.
			return nil, ErrMembershipExists.WithParam("membership_id", membershipID).WithCause(err)
		}
		return nil, err
	}
	m, err := s.repo.FindByID(ctx, membershipID)
	if err != nil {
		if dbkit.IsRecordNotFound(err) {
			return nil, ErrMembershipNotFound.WithParam("membership_id", membershipID)
		}
		return nil, err
	}
	s.publish(ctx, EventMemberRestored, MemberRestored{
		MembershipID: m.ID,
		UserID:       m.UserID,
		NodeID:       m.NodeID,
	})
	return m, nil
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
