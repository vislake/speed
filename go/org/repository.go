package org

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// Repository is org's tenant-scoped data-access type for OrgNode.
//
// It embeds *dbkit.Repository[OrgNode], so Create, FindByID, Update, Delete
// and List are promoted unchanged, with isolation layers 2 and 3 (the
// fail-closed tenant check and the RLS session wiring) already applied. On
// top of that it adds the four query shapes Repository[T]'s deliberately
// minimal surface cannot express -- a subtree prefix scan, a children scan,
// the root lookup and a sibling-name lookup.
//
// Those four are written the way go/dbkit/AGENTS.md's "Known limitations"
// prescribes as option 1: build the query on the same *gorm.DB layer 1
// already protects, against a TenantScoped destination, so the GORM
// isolation plugin still injects WHERE tenant_id = ? even though
// Repository[T]'s own re-verification does not run for that call -- and run
// it inside dbkit.WithTenantSession, so isolation layer 3 (the PostgreSQL
// RLS session variable) is set for it exactly as it is for every promoted
// method. What this file must never do, and does not:
//
//   - hand-write a tenant_id filter. There is no "tenant_id = ?" string in
//     this package; the plugin supplies it. go/org has no allowlist entry in
//     tools/semgrep_rules/handwritten-tenant-id-filter.yml, deliberately.
//   - reach for db.Table, db.Model or db.Raw. Those bypass the plugin
//     entirely; go/org has no allowlist entry in
//     tools/semgrep_rules/raw-gorm-bypass.yml either.
type Repository struct {
	*dbkit.Repository[OrgNode]

	// db is the same connection the embedded Repository[OrgNode] was built
	// on, kept only so the four extra query shapes above can be composed on
	// it. Every use routes through WithTenantSession and a TenantScoped
	// destination; nothing in this file issues an unprotected statement.
	db *gorm.DB
}

// NewRepository returns a Repository backed by db. db is expected to come
// from dbkit.Open, already migrated with this module's Migrations() -- see
// dbkit.Repository's own doc comment for why Open specifically.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[OrgNode](db), db: db}
}

// subtree returns the node whose path is exactly prefix together with every
// node beneath it, ordered by (depth, id) so the result is stable across
// engines and across runs.
//
// prefix must come from subtreePrefix: it ends at an id boundary, so "/a/"
// can never match "/ab/". The LIKE pattern is prefix + "%" built in Go and
// bound as one parameter; see path.go for why no escaping and no ESCAPE
// clause is needed, and why both dialects select identical rows.
func (r *Repository) subtree(ctx context.Context, prefix string) ([]OrgNode, error) {
	var nodes []OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("path LIKE ?", prefix+"%").
			Order("depth, id").
			Find(&nodes).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return nodes, nil
}

// children returns the direct children of parentID, ordered by name so a
// listing is stable. An empty parentID would name the tenant root's own
// parent slot and is never a meaningful query; callers pass a real node id.
func (r *Repository) children(ctx context.Context, parentID string) ([]OrgNode, error) {
	var nodes []OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("parent_id = ?", parentID).
			Order("name, id").
			Find(&nodes).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return nodes, nil
}

// findRoot returns the caller tenant's root node, or ErrNodeNotFound when
// the tenant has no tree yet. The root is the one node whose parent_id is
// the empty-string sentinel; CreateRoot's ErrRootAlreadyExists check is what
// keeps it unique.
func (r *Repository) findRoot(ctx context.Context) (*OrgNode, error) {
	var node OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("parent_id = ?", "").First(&node).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrNodeNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &node, nil
}

// bySiblingName returns the child of parentID named name, or
// ErrNodeNotFound when there is none. It is the pre-check behind
// ErrDuplicateSiblingName; the database's UNIQUE(tenant_id, parent_id, name)
// index is the backstop for the race this pre-check cannot close.
func (r *Repository) bySiblingName(ctx context.Context, parentID, name string) (*OrgNode, error) {
	var node OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("parent_id = ?", parentID).
			Where("name = ?", name).
			First(&node).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrNodeNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &node, nil
}

// byIDs returns the nodes with the given ids, ordered by depth so an
// ancestor chain comes back root-first. It is the ancestor query: the ids
// come from splitting a node's own materialized path, so no recursion and no
// second round trip per level is needed. An empty ids slice returns no rows
// without touching the database.
func (r *Repository) byIDs(ctx context.Context, ids []string) ([]OrgNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var nodes []OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("id IN ?", ids).
			Order("depth, id").
			Find(&nodes).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return nodes, nil
}

// errSubtreeSizeUnexpected aborts deleteLeaf's transaction when the prefix
// matched a number of rows other than the single node the caller meant to
// remove. It never escapes this file: deleteLeaf translates it into the
// org-level error the row count implies.
var errSubtreeSizeUnexpected = errors.New("org: subtree delete matched an unexpected number of rows")

// softDeleteActor resolves the acting identity from ctx for a bulk
// mark-delete's deleted_by column, mirroring dbkit.Repository[T]'s own
// unexported softDelete exactly: pkgcore.ActorFromContext's ID when ctx
// carries one, the empty string otherwise -- which is not itself an error,
// the same convention audit_capture.go uses for a missing Actor.
//
// deleteLeaf and deleteSubtree below need their own copy of this rather than
// reaching dbkit.Repository[OrgNode].Delete's promoted, single-row
// mark-delete: both are a bulk write over a whole subtree, in one statement
// inside one transaction, a shape the promoted single-row Delete cannot
// express -- see deleteSubtree's own doc comment for why row-by-row is not
// an acceptable substitute.
func softDeleteActor(ctx context.Context) string {
	if actor, ok := pkgcore.ActorFromContext(ctx); ok {
		return actor.ID
	}
	return ""
}

// deleteLeaf mark-deletes the single node whose path is exactly prefix, and
// refuses -- rolling the whole statement back -- if that prefix turns out to
// match more than one currently-live row.
//
// It is a bulk write, not the single-row dbkit.Repository[OrgNode].Delete
// promoted onto Repository: it follows the exact shape dbkit's own
// unexported softDelete uses (see dbkit's repository.go and AGENTS.md's
// "Soft deletion" section) -- a real *OrgNode built and written through
// tx.Where(...).Select(...).Updates(&m), never a map payload, so gorm's
// SetupUpdateReflectValue resolves Model == Dest == &m and any audit
// capture a host wires reads the real written values rather than a
// zero-valued struct.
//
// The row count is checked INSIDE the transaction on purpose, exactly as it
// was before this round switched the statement from DELETE to UPDATE. A
// check-then-update pair would leave a window in which another request adds
// a child to the node between the two statements, and this call would then
// orphan that child: its parent_id would point at a soft-deleted row and its
// path would name a node no ordinary read can see. There is no foreign key
// to catch that (a self-referencing FK is unenforced on SQLite unless
// foreign_keys is turned on, which would make the two dialects behave
// differently), so the guard has to be the transaction itself.
//
// The WHERE clause explicitly requires deleted_at IS NULL: only currently-
// live rows count toward "does this node have children". A node whose only
// remaining "descendant" is itself already soft-deleted (from some earlier,
// independent mark-delete) is correctly treated as a leaf -- its hidden
// descendant is not resurrected or re-touched by this call, and does not
// block the delete the way a live one would.
//
// It reports the number of rows the prefix matched, so the caller can turn
// "more than one" into org.node_has_children with a real count.
func (r *Repository) deleteLeaf(ctx context.Context, prefix string) (matched int64, err error) {
	now := time.Now()
	deletedBy := softDeleteActor(ctx)
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		m := OrgNode{DeletedAt: &now, DeletedBy: deletedBy}
		res := tx.
			Where("path LIKE ?", prefix+"%").
			Where("deleted_at IS NULL").
			Select("DeletedAt", "DeletedBy").
			Updates(&m)
		if res.Error != nil {
			return res.Error
		}
		matched = res.RowsAffected
		if matched != 1 {
			return errSubtreeSizeUnexpected
		}
		return nil
	})
	switch {
	case errors.Is(err, errSubtreeSizeUnexpected):
		return matched, nil
	case err != nil:
		return 0, ErrInternal.WithCause(err)
	}
	return matched, nil
}

// deleteSubtree mark-deletes the node whose path is exactly prefix and every
// currently-live node beneath it, in one statement inside one transaction,
// and reports how many rows it touched. Mark-deleting a subtree row by row
// -- for instance by calling the promoted, single-row
// dbkit.Repository[OrgNode].Delete once per descendant -- would leave a
// partially soft-deleted tree behind on any mid-loop failure, and would
// abandon the very atomicity go/org/AGENTS.md's "Known limitations" already
// flags as missing for Move; one UPDATE cannot leave that window.
//
// It follows dbkit's own unexported softDelete shape exactly, the same way
// deleteLeaf's doc comment describes: a real *OrgNode, Model == Dest == &m,
// never a map payload.
//
// The statement is a plain Updates against a TenantScoped model, so the
// isolation plugin injects the tenant filter here exactly as it did for the
// Delete this replaces. The added "deleted_at IS NULL" leaves an
// already-soft-deleted descendant (from some earlier, independent
// mark-delete) untouched rather than re-stamping its deleted_at/deleted_by
// with this call's own attribution.
func (r *Repository) deleteSubtree(ctx context.Context, prefix string) (int64, error) {
	now := time.Now()
	deletedBy := softDeleteActor(ctx)
	var affected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		m := OrgNode{DeletedAt: &now, DeletedBy: deletedBy}
		res := tx.
			Where("path LIKE ?", prefix+"%").
			Where("deleted_at IS NULL").
			Select("DeletedAt", "DeletedBy").
			Updates(&m)
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return 0, ErrInternal.WithCause(err)
	}
	return affected, nil
}
