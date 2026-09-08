package rbac

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// The three repositories below are this module's only data-access types.
// Each embeds dbkit.Repository[T] rather than holding a bare *gorm.DB, so
// Create, FindByID, Update, Delete and List come from the
// generic base with the tenant check already fail-closed.
//
// All three tables are tenant-owned -- rbac_roles and rbac_role_permissions
// are tenant data, rbac_role_bindings is link data -- so all three run
// tenancytest.AssertIsolated, and this module runs
// tenancytest.AssertNotTenantScoped exactly zero times. That is not an
// omission: there is no identity-domain or platform-domain table in rbac
// for the reverse assertion to assert. The permission catalog, which IS
// platform-scoped, has no table at all -- it is the in-memory snapshot of
// what the modules declared (catalog.go).
//
// Each repository additionally carries the *gorm.DB it was built on, for
// the filtered reads Repository[T]'s deliberately minimal surface cannot
// express (it has List-all and nothing else). Those reads are the
// sanctioned filtered-read path: the query is built on the
// same *gorm.DB layer 1 already protects, still against a TenantScoped
// model, so the isolation plugin's own WHERE tenant_id = ? is appended for
// us -- no tenant filter is ever hand-written here -- and the whole call
// runs inside dbkit.WithTenantSession so isolation layer 3 (PostgreSQL
// row-level security) is engaged as well. findWithinTenant then re-checks
// every returned row's tenant in Go, the same defense-in-depth check
// Repository[T].FindByID performs on its single row.

// RoleRepository is the data-access type for rbac_roles.
type RoleRepository struct {
	*dbkit.Repository[Role]

	// db is the same connection the embedded Repository was built on, kept
	// for the filtered reads below. See the file comment above for why
	// that is the sanctioned path rather than a bypass.
	db *gorm.DB
}

// NewRoleRepository returns a RoleRepository backed by db. db is expected
// to come from dbkit.Open, already migrated with this module's own
// Migrations().
func NewRoleRepository(db *gorm.DB) *RoleRepository {
	return &RoleRepository{Repository: dbkit.NewRepository[Role](db), db: db}
}

// ByKey returns the role with the given key inside the tenant ctx carries.
//
// It returns ErrRoleNotFound both when no such role exists and when one
// exists under a different tenant -- deliberately indistinguishable, for
// the same reason dbkit.ErrRecordNotFound is: distinguishing them would
// let a caller learn that a role key exists in a tenant it cannot see.
func (r *RoleRepository) ByKey(ctx context.Context, key string) (*Role, error) {
	roles, err := findWithinTenant[Role](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("key = ?", key)
	})
	if err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		return nil, ErrRoleNotFound.WithParam("key", key)
	}
	// uq_rbac_roles_tenant_key makes more than one row impossible; taking
	// the first is not a silent "pick any", it is the only row there can be.
	role := roles[0]
	return &role, nil
}

// RolePermissionRepository is the data-access type for
// rbac_role_permissions.
type RolePermissionRepository struct {
	*dbkit.Repository[RolePermission]

	// db is the same connection the embedded Repository was built on.
	db *gorm.DB
}

// NewRolePermissionRepository returns a RolePermissionRepository backed by
// db (see NewRoleRepository for what db is expected to be).
func NewRolePermissionRepository(db *gorm.DB) *RolePermissionRepository {
	return &RolePermissionRepository{Repository: dbkit.NewRepository[RolePermission](db), db: db}
}

// ByRole returns every permission granted to roleID inside the tenant ctx
// carries. A role with no permissions, and a roleID belonging to another
// tenant, both yield an empty slice and no error: "grants nothing" is the
// correct, fail-closed answer to both.
func (r *RolePermissionRepository) ByRole(ctx context.Context, roleID string) ([]RolePermission, error) {
	return findWithinTenant[RolePermission](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id = ?", roleID)
	})
}

// ByRoles returns every permission granted to any of roleIDs inside the
// tenant ctx carries -- the evaluation path's one permission read.
//
// It exists so that flattening a subject's bindings into a grant set costs
// ONE query rather than one per role. The alternative, calling ByRole in a
// loop, is the N+1 pattern on the hottest read in the product; the IN
// clause is expressed through the same tenant-filtered builder as every
// other read here, so it buys that without giving up any isolation layer.
//
// An empty roleIDs yields an empty slice without touching the database: an
// "IN ()" predicate is a dialect-specific edge (and a syntax error on some
// of them) with no useful answer.
func (r *RolePermissionRepository) ByRoles(ctx context.Context, roleIDs []string) ([]RolePermission, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	return findWithinTenant[RolePermission](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id IN ?", roleIDs)
	})
}

// RoleBindingRepository is the data-access type for rbac_role_bindings.
type RoleBindingRepository struct {
	*dbkit.Repository[RoleBinding]

	// db is the same connection the embedded Repository was built on.
	db *gorm.DB
}

// NewRoleBindingRepository returns a RoleBindingRepository backed by db
// (see NewRoleRepository for what db is expected to be).
func NewRoleBindingRepository(db *gorm.DB) *RoleBindingRepository {
	return &RoleBindingRepository{Repository: dbkit.NewRepository[RoleBinding](db), db: db}
}

// ByUser returns every binding held by userID inside the tenant ctx
// carries. It is the hot read of the evaluation path: a subject's whole
// grant set starts here.
//
// A userID with no bindings, and a userID whose bindings all live in
// another tenant, both yield an empty slice and no error -- the same
// fail-closed "grants nothing" both cases deserve.
func (r *RoleBindingRepository) ByUser(ctx context.Context, userID string) ([]RoleBinding, error) {
	return findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("user_id = ?", userID)
	})
}

// ByRole returns every binding that references roleID inside the tenant
// ctx carries -- the reverse question ByUser answers, which changing or
// deleting a role needs.
func (r *RoleBindingRepository) ByRole(ctx context.Context, roleID string) ([]RoleBinding, error) {
	return findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id = ?", roleID)
	})
}

// ByNodes returns every binding scoped to one of nodeIDs inside the tenant
// ctx carries -- a third enumeration alongside ByUser and ByRole, for the
// question org's org.node.deleted reap (reap.go's onNodeDeleted) needs
// answered: a deleted node carries no user and no role, only the id (or,
// for a cascade, ids) of what just disappeared from org's own tree, and
// every binding scoped to any of them is what the reap must find
// regardless of who holds it or which role it names.
//
// An empty nodeIDs returns an empty result with no query at all: node_id
// IN () is not a clause this method exists to render, and it would be a
// silent full-tenant no-op-turned-something-else on some dialects rather
// than the trivially-correct "nothing named, nothing found" answer.
func (r *RoleBindingRepository) ByNodes(ctx context.Context, nodeIDs []string) ([]RoleBinding, error) {
	if len(nodeIDs) == 0 {
		// Still validate the tenant even on the trivial path, so a caller
		// that mistakenly holds no tenant context gets the same
		// pkgcore.ErrNoTenant every other method here reports, rather than
		// a silently successful empty answer that could mask the mistake.
		if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("node_id IN ?", nodeIDs)
	})
}

// RevokedByUser returns every currently mark-deleted binding held by
// userID inside the tenant ctx carries -- the enumeration
// Service.onMemberRestored needs: org restored a removed member, and the
// bindings the removal reap revoked (reap.go's reapRoleBindings) are
// among the soft-deleted rows of that (tenant, user). The caller then
// filters the returned rows by their revoke_origin marker, keeping only
// the member-removal rows this removal owns (deliberate and node-deletion
// rows share the enumeration but must stay revoked -- reap.go's
// reinstateRoleBindings). ByUser answers the
// mirror question for live rows only (the auto-scope plugin hides the
// rest); this read goes through db.Unscoped() -- GORM's own general
// query-scope bypass, which the tenant-scope plugin does not consult
// (tenant_scope.go), so this read stays fully tenant-scoped exactly like
// every other method in this file -- and keeps the rows the plugin would
// otherwise hide by requiring deleted_at IS NOT NULL itself, the identical
// technique findMostRecentlyRevoked uses to recover one revoked row.
// Every returned row is re-verified to belong to ctx's tenant in Go
// afterwards, the same defense-in-depth check findWithinTenant applies to
// its own rows.
//
// A userID with no revoked bindings, and a userID whose revoked bindings
// all live in another tenant, both yield an empty slice and no error.
func (r *RoleBindingRepository) RevokedByUser(ctx context.Context, userID string) ([]RoleBinding, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var rows []RoleBinding
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Unscoped().
			Where("user_id = ?", userID).
			Where("deleted_at IS NOT NULL").
			Find(&rows).Error
	})
	switch {
	case errors.Is(err, pkgcore.ErrNoTenant):
		return nil, err
	case err != nil:
		return nil, ErrStorage.WithCause(err)
	}

	owned := make([]RoleBinding, 0, len(rows))
	for _, row := range rows {
		if row.GetTenantID() != tenant {
			continue
		}
		owned = append(owned, row)
	}
	return owned, nil
}

// RevokedByNodes returns every currently mark-deleted binding scoped to
// one of nodeIDs inside the tenant ctx carries -- the enumeration
// Service.onNodeRestored needs: org restored one node, and the bindings
// the node-deletion reap revoked at it (reap.go's reapRoleBindingsForNodes)
// are among the soft-deleted rows scoped to that node. The caller then
// filters the returned rows by their revoke_origin marker, keeping only
// the node-deletion rows this deletion owns (deliberate rows, and rows
// the member-removal claimed for a departed member, share the enumeration
// but must stay revoked -- reap.go's reinstateRoleBindingsForNode). It is
// RevokedByUser's per-node sibling, sharing its Unscoped, tenant-scoped
// technique and its Go-side tenant re-verification; see that method's own
// doc comment for the full reasoning.
//
// An empty nodeIDs returns an empty result with no query at all, the same
// choice ByNodes documents for its own empty-id case.
func (r *RoleBindingRepository) RevokedByNodes(ctx context.Context, nodeIDs []string) ([]RoleBinding, error) {
	if len(nodeIDs) == 0 {
		if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var rows []RoleBinding
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Unscoped().
			Where("node_id IN ?", nodeIDs).
			Where("deleted_at IS NOT NULL").
			Find(&rows).Error
	})
	switch {
	case errors.Is(err, pkgcore.ErrNoTenant):
		return nil, err
	case err != nil:
		return nil, ErrStorage.WithCause(err)
	}

	owned := make([]RoleBinding, 0, len(rows))
	for _, row := range rows {
		if row.GetTenantID() != tenant {
			continue
		}
		owned = append(owned, row)
	}
	return owned, nil
}

// The revoke-origin vocabulary (the 0003_add_revoke_origin.sql migration
// adds the column; model.go's RevokeOrigin field comment and reap.go's
// file header carry the full story):
// every mark-delete of a RoleBinding row records which writer wrote it, so
// the org restore-side subscribers re-instate exactly the rows the matching
// removal or deletion reaped. A deliberate RevokeRole revocation is never
// auto-reinstated by an org event.
const (
	// revokeOriginDeliberate is the value a Service.RevokeRole mark-delete
	// writes: a deliberate revocation, restorable only by an explicit,
	// affirmative Service.RestoreRole -- never by an org member-restore or
	// node-restore event. The empty string is also the value the 0003
	// migration's backfill wrote for every row soft-deleted before the
	// column existed, and the two are deliberately the
	// same value: both are "unknown or deliberate", and both must fail
	// closed the same way.
	revokeOriginDeliberate = ""

	// revokeOriginMemberRemoval is the value the org.member.removed reap
	// writes when it revokes a removed member's live binding, and the value
	// claimByUser re-attributes a removed member's node-deletion rows to.
	// Rows carrying it are the ones onMemberRestored re-instates.
	revokeOriginMemberRemoval = "member-removal"

	// revokeOriginNodeDeletion is the value the org.node.deleted reap writes
	// when it revokes a binding scoped to a deleted node. Rows carrying it
	// are the ones onNodeRestored re-instates -- unless claimByUser has
	// re-attributed them to a later member removal.
	revokeOriginNodeDeletion = "node-deletion"
)

// Delete marks the binding with the given id soft-deleted and records in
// revoke_origin which writer performed the revoke -- the mark-delete
// dbkit.Repository[RoleBinding].Delete performs, extended by the one
// column. It SHADOWS the promoted dbkit Delete of the same name on
// purpose: this
// repository's callers -- Service.RevokeRole and the two reaps -- must not
// be able to reach a mark-delete that leaves the origin column unwritten,
// because a revoked row whose writer is unknown is indistinguishable from a
// deliberate revocation and would silently become unrestorable-by-org-event
// (or worse, resurrectable by the wrong org event) the next time a member
// or node comes back. A compile error at every call site is the forcing
// function that keeps the three writers honest; the embedded dbkit Delete
// remains reachable only through the embedded field itself
// (r.Repository.Delete), which nothing in this module does.
//
// The UPDATE itself mirrors dbkit's softDelete conditional-write shape
// (go/dbkit/repository.go) extended by the one extra column: a single
// UPDATE setting deleted_at to now, deleted_by to the acting identity
// resolved from ctx (the empty string when ctx carries no actor, exactly as
// dbkit treats an absent actor), and revoke_origin to origin, guarded by a
// deleted_at IS NULL predicate so a row already soft-deleted is not
// re-marked over its original attribution. Like every write in this file it
// runs inside dbkit.WithTenantSession -- layer 3 (PostgreSQL row-level
// security) engaged -- and the tenant filter is injected by the isolation
// plugin itself, never hand-written here: the plugin's update callback
// scopes the statement to ctx's tenant exactly as it scopes every other
// update this module issues through dbkit's repositories (the identical
// reliance org's TreeService places on it for its own conditional writes).
//
// It reports dbkit.ErrRecordNotFound when no live row matches, the same
// zero-rows-affected outcome dbkit's own Delete reports: a concurrent
// revoke already withdrew the row, which the callers' hasCode
// classification (RevokeRole's concurrent-double-revoke branch,
// revokeReapedBindings' concurrent-revoke skip) treats as success.
func (r *RoleBindingRepository) Delete(ctx context.Context, id string, origin string) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}

	deletedBy := ""
	if actor, ok := pkgcore.ActorFromContext(ctx); ok {
		deletedBy = actor.ID
	}
	now := time.Now()

	m := RoleBinding{DeletedAt: &now, DeletedBy: deletedBy, RevokeOrigin: origin}
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("deleted_at IS NULL").
			Select("DeletedAt", "DeletedBy", "RevokeOrigin").
			Updates(&m)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return dbkit.ErrRecordNotFound.WithParam("id", id)
	}
	return nil
}

// Restore clears the mark-delete Delete wrote, making the binding visible
// to ordinary reads again -- dbkit.Repository[RoleBinding].Restore's
// contract -- and additionally clears revoke_origin back to the empty
// default, so a live row never carries a stale origin from a previous
// revoked life (the marker is meaningful only while deleted_at IS NOT NULL,
// and clearing it on restore keeps that invariant true for every row, not
// just the ones a later mark-delete happens to rewrite).
//
// It SHADOWS the promoted dbkit Restore of the identical signature for the
// same reason Delete shadows dbkit's Delete: every un-marking of a row must
// reset the marker with it, and the two operations belong to one writer.
// The UPDATE mirrors dbkit's Restore conditional-write shape (go/dbkit/
// repository.go) -- deleted_at and deleted_by back to NULL and the empty
// string, guarded by deleted_at IS NOT NULL so a live row is never
// "restored" -- extended by the third column, inside the same
// WithTenantSession-plus-plugin isolation every write in this file uses
// (the tenant filter is injected by the plugin, never hand-written; the
// Unscoped() is the same defensive no-op dbkit's own Restore carries, kept
// here in case the soft-delete auto-scope is ever broadened to updates).
//
// It reports dbkit.ErrRecordNotFound when no currently soft-deleted row
// matches, the identical zero-rows outcome dbkit's own Restore reports:
// nothing was left to un-mark. A restore
// that collides with uq_rbac_role_bindings_tenant_user_role_node -- a live
// row already occupies the tuple -- surfaces the driver-agnostic
// gorm.ErrDuplicatedKey sentinel dbkit.Open wires TranslateError to, the
// signal Service.RestoreRole classifies as the grant already existing.
func (r *RoleBindingRepository) Restore(ctx context.Context, id string) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}

	var m RoleBinding
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("deleted_at IS NOT NULL").
			Unscoped().
			Select("DeletedAt", "DeletedBy", "RevokeOrigin").
			Updates(&m)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return dbkit.ErrRecordNotFound.WithParam("id", id)
	}
	return nil
}

// claimByUser re-attributes every currently soft-deleted binding of userID
// that the org.node.deletion reap wrote (revoke_origin = 'node-deletion') to
// the member-removal origin. Service.onMemberRemoved runs it after reaping
// the removed member's live bindings, and it closes the hazard of a
// node-reaped row of a user who is removed from the tenant later: the row
// must not
// be resurrectable by a node restore -- the node restore would rebuild
// authorization for a holder who is no longer a member -- so the removal
// claims the row for itself, making the user's own org.member.restored
// event (which by definition arrives only when the membership is live
// again) the row's only possible resurrection path.
//
// Rows the reap itself revoked just above this call already carry the
// member-removal origin; deliberate RevokeRole rows are deliberately left
// alone -- no org event may ever resurrect them, so there is nothing to
// claim. The UPDATE is a plain re-attribution: no row's deleted state, no
// decision cache and no event changes, because no decision any replica
// could serve changes with it -- the durable re-attribution alone is what
// the later node-restored handler reads. (That is also why the reaps'
// at-least-once redelivery is safe here: a second delivery finds no
// node-deletion rows left to claim and rewrites nothing.)
//
// An error is reported rather than swallowed; the handler decides how to
// log it. The UPDATE is scoped to ctx's tenant by the isolation plugin's
// update callback -- this module never hand-writes a tenant predicate --
// and runs inside dbkit.WithTenantSession so layer 3 (PostgreSQL row-level
// security) is engaged.
func (r *RoleBindingRepository) claimByUser(ctx context.Context, userID string) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}

	m := RoleBinding{RevokeOrigin: revokeOriginMemberRemoval}
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("user_id = ?", userID).
			Where("deleted_at IS NOT NULL").
			Where("revoke_origin = ?", revokeOriginNodeDeletion).
			Select("RevokeOrigin").
			Updates(&m).Error
	})
}

// reattributeToNodeDeletion is the member-restored side's mirror inverse of
// claimByUser: ONE currently soft-deleted binding -- reaped or claimed by
// the member-removal, revoke_origin = 'member-removal' -- is re-attributed
// back to the node-deletion origin. Service.onMemberRestored runs it for
// each of a restored member's rows whose node does not resolve at
// re-instatement time (see bindingNodeLivesAtMemberRestore in reap.go), and
// it exists because the member's own return cannot re-open a grant whose
// OTHER structural precondition -- the node it is scoped to -- is still
// missing: re-attributing the row to the node-deletion origin hands it back
// to the node's own fate, where only a future org.node.restored (which by
// definition fires only when the node is genuinely visible again) can lift
// it, exactly like every other grant the deletion reaped. Without the
// reverse claim the row would stay stranded under the member-removal origin
// forever -- no node restore ever touches that origin -- or come back live
// with the member while the node was still gone (see that method's doc
// comment).
//
// The UPDATE is deliberately guarded to the row's id AND its current
// member-removal origin, so a deliberate RevokeRole row (which the caller's
// origin filter never admits in the first place) and a row a concurrent
// writer already moved -- restored, or re-claimed by a later removal, or
// re-attributed by a redelivered restore event -- are never touched by this
// write: the guard is the row's own say over who may re-own it. Like the
// forward claim it is a plain re-attribution with no event of its own -- no
// decision any replica could serve changes with it (the row stays revoked
// either way), so nothing is published and no cache entry is dropped -- and
// it stays idempotent under redelivery. changed reports whether the UPDATE
// matched the row: false with a nil error means a concurrent writer already
// moved the row, the caller's goal achieved or mooted by that writer, which
// the restore-side loop classifies silent exactly as it classifies
// Restore's own ErrRecordNotFound race.
//
// An error is reported rather than swallowed; the handler decides how to
// log it. The UPDATE is scoped to ctx's tenant by the isolation plugin's
// update callback and runs inside dbkit.WithTenantSession so layer 3
// (PostgreSQL row-level security) is engaged, exactly like claimByUser.
func (r *RoleBindingRepository) reattributeToNodeDeletion(ctx context.Context, id string) (bool, error) {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return false, err
	}

	m := RoleBinding{RevokeOrigin: revokeOriginNodeDeletion}
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("deleted_at IS NOT NULL").
			Where("revoke_origin = ?", revokeOriginMemberRemoval).
			Select("RevokeOrigin").
			Updates(&m)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}

// Find returns the one binding that grants userID the role roleID at
// exactly nodeID (empty nodeID meaning tenant-wide), inside the tenant ctx
// carries. It reports ErrBindingNotFound when there is none.
//
// The scope is matched EXACTLY rather than "at or above": assigning and
// revoking address one row, and treating a revoke of a tenant-wide grant as
// covering a node-scoped one would delete a grant the caller did not name.
// uq_rbac_role_bindings makes the four columns unique, so at most one row
// can match.
func (r *RoleBindingRepository) Find(ctx context.Context, userID, roleID, nodeID string) (*RoleBinding, error) {
	rows, err := findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.
			Where("user_id = ?", userID).
			Where("role_id = ?", roleID).
			Where("node_id = ?", nodeID)
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrBindingNotFound.
			WithParam("role_id", roleID).
			WithParam("node_id", nodeID)
	}
	binding := rows[0]
	return &binding, nil
}

// findMostRecentlyRevoked returns the most recently mark-deleted binding
// matching (userID, roleID, nodeID) inside the tenant ctx carries -- the
// "undo my last revoke" row Service.RestoreRole needs.
//
// RoleBinding's soft-delete auto-scope plugin hides every mark-deleted row
// from an ordinary Find (go/dbkit/soft_delete.go), so recovering one at all
// needs db.Unscoped() -- GORM's own general query-scope bypass, which the
// tenant-scope plugin does not consult (tenant_scope.go), so this read
// stays fully tenant-scoped exactly like every other method in this file,
// re-verified in Go afterwards the same defense-in-depth way
// findWithinTenant is.
//
// More than one soft-deleted row can share this exact tuple: the partial
// unique index (migrations/{postgres,sqlite}/0002_add_soft_delete.sql)
// frees a revoked binding's slot immediately, so a
// revoke-then-reassign-then-revoke-again sequence leaves TWO soft-deleted
// rows under the identical (tenant, user, role, node) tuple, one from each
// revoke. Ordering by deleted_at DESC picks the most recent one --
// "restore exactly what I just revoked," never an earlier occupant of the
// same scope -- with id DESC as a stable tiebreak for two rows revoked in
// the same instant, since deleted_at alone gives no meaningful order there.
//
// It reports ErrBindingNotFound when no soft-deleted row matches: the tuple
// was never granted, or every past grant at it is still live. That is the
// same collapsed "nothing to restore" signal RevokeRole's own Find already
// gives for "nothing to revoke".
func (r *RoleBindingRepository) findMostRecentlyRevoked(ctx context.Context, userID, roleID, nodeID string) (*RoleBinding, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var rows []RoleBinding
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Unscoped().
			Where("user_id = ?", userID).
			Where("role_id = ?", roleID).
			Where("node_id = ?", nodeID).
			Where("deleted_at IS NOT NULL").
			Order("deleted_at DESC, id DESC").
			Limit(1).
			Find(&rows).Error
	})
	switch {
	case errors.Is(err, pkgcore.ErrNoTenant):
		return nil, err
	case err != nil:
		return nil, ErrStorage.WithCause(err)
	}
	if len(rows) == 0 || rows[0].GetTenantID() != tenant {
		return nil, ErrBindingNotFound.
			WithParam("role_id", roleID).
			WithParam("node_id", nodeID)
	}
	binding := rows[0]
	return &binding, nil
}

// CreateWithPermissions inserts a role and the permission rows it grants
// in ONE tenant-scoped transaction, so a role never becomes visible
// holding a partial permission set.
//
// Atomicity matters more here than the two-statement cost suggests: a role
// that materialized with three of its five permissions would be a silently
// under-powered grant that no error told anyone about, and the retry that
// followed would hit the role key's unique index rather than completing the
// set. dbkit.WithTenantSession already opens the transaction (it must, to
// scope the PostgreSQL session GUC), so the write rides inside the same
// boundary every read here uses, with the isolation plugin's create
// callback forcing tenant_id on every row -- including the batch insert,
// which it covers row by row.
func (r *RoleRepository) CreateWithPermissions(ctx context.Context, role *Role, permissions []RolePermission) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}
	if err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(role).Error; err != nil {
			return err
		}
		if len(permissions) == 0 {
			return nil
		}
		return tx.Create(&permissions).Error
	}); err != nil {
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return err
		}
		return ErrStorage.WithCause(err)
	}
	return nil
}

// ReplaceForRole makes roleID's permission rows exactly permissions,
// deleting what is no longer granted and inserting what is new, in one
// tenant-scoped transaction.
//
// It is a replace rather than a merge because the caller (built-in role
// reconciliation) knows the whole desired set, and a merge would leave a
// permission that was removed from the definition still granted -- the
// failure mode where a role quietly keeps authority someone deliberately
// took away from it.
func (r *RolePermissionRepository) ReplaceForRole(ctx context.Context, roleID string, permissions []RolePermission) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}
	if err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", roleID).Delete(&RolePermission{}).Error; err != nil {
			return err
		}
		if len(permissions) == 0 {
			return nil
		}
		return tx.Create(&permissions).Error
	}); err != nil {
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return err
		}
		return ErrStorage.WithCause(err)
	}
	return nil
}

// findWithinTenant runs a filtered read for one tenant-scoped model.
//
// It is a package-level generic function rather than a method because Go
// has no generic methods, and one function rather than four copies because
// every one of the filtered reads above needs the identical three-part
// protection and the identical error mapping:
//
//  1. Resolve the tenant from ctx first and fail closed before touching the
//     database, exactly as every dbkit.Repository[T] method does.
//  2. Run the query inside dbkit.WithTenantSession, so isolation layer 3
//     (the PostgreSQL app.current_tenant GUC a row-level security policy
//     reads) is engaged for this statement, and let the isolation plugin
//     append the tenant filter itself -- apply only ever adds the module's
//     own business condition, never a tenant one.
//  3. Re-verify every returned row's tenant in Go afterwards, the same
//     defense-in-depth check Repository[T].FindByID makes on its single
//     row, so isolation still holds if layer 1 were ever absent (a
//     *gorm.DB not built through dbkit.Open) rather than silently leaking.
//
// A row that fails step 3 is dropped rather than turned into an error, for
// the same reason FindByID reports a cross-tenant hit as "not found": the
// caller learns nothing about another tenant's data either way, and an
// error here would make a plugin-less connection fail loudly in a place
// that has nothing to do with the caller's request.
func findWithinTenant[T dbkit.TenantScoped](
	ctx context.Context,
	db *gorm.DB,
	apply func(tx *gorm.DB) *gorm.DB,
) ([]T, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var rows []T
	if err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return apply(tx).Find(&rows).Error
	}); err != nil {
		// A caller that classifies "no tenant" must keep seeing pkgcore's
		// own sentinel rather than a storage error wrapping it; everything
		// else is genuinely a storage failure.
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return nil, err
		}
		return nil, ErrStorage.WithCause(err)
	}

	owned := make([]T, 0, len(rows))
	for _, row := range rows {
		if row.GetTenantID() != tenant {
			continue
		}
		owned = append(owned, row)
	}
	return owned, nil
}
