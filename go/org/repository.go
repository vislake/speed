package org

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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

// findByIDIncludingDeleted returns the node with the given id within ctx's
// tenant, whether it is currently live or mark-deleted -- unlike the
// promoted FindByID and every other read in this file, which the soft-delete
// auto-scope plugin silently hides a mark-deleted row from.
//
// TreeService.Restore needs exactly this: to read a node's stored ParentID
// BEFORE deciding whether restoring it is safe, and a mark-deleted node's
// own row is precisely the one an ordinary, scoped read cannot see.
//
// db.Unscoped() is GORM's own general query-scope bypass (the same one
// soft_delete.go's plugin checks and skips); it disables only the
// soft-delete scope here, never tenant isolation -- the tenant-scope plugin
// does not consult it (tenant_scope.go), so this call is fully tenant-scoped
// exactly like every other method in this file. It reports ErrNodeNotFound
// for an id with no row at all in this tenant, live or mark-deleted,
// collapsing "never existed" and "belongs to another tenant" the same way
// FindByID already does.
func (r *Repository) findByIDIncludingDeleted(ctx context.Context, id string) (*OrgNode, error) {
	var node OrgNode
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("id = ?", id).First(&node).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrNodeNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &node, nil
}

// touchLockByID takes the dialect-neutral row lock on the live node
// identified by id, inside an already-open transaction (tx, as handed to a
// dbkit.WithTenantSession callback), and reports whether a live row existed
// to lock.
//
// # The lock is a no-op write, and that is the point
//
// DeletedBy is dbkit's own soft-delete column, and a currently-live row
// always carries the empty string in it: deleteLeaf and deleteSubtree below
// only ever set DeletedBy together with DeletedAt, in the same statement, so
// there is no path that leaves a live row (deleted_at IS NULL) with a
// non-empty DeletedBy. Writing "" back is therefore a genuine no-op for the
// data on any row this call is allowed to succeed against, but a genuine
// WRITE for the database engine:
//
//   - On PostgreSQL it takes the row's write lock, held until this
//     transaction commits or rolls back. A concurrent writer of the SAME
//     row (deleteLeaf soft-deleting this exact node, say, or another call
//     to this function racing to move it) either already committed --
//     in which case this UPDATE's WHERE clause is re-evaluated against the
//     now-current row and correctly fails to match a dead one -- or is
//     still open, in which case this UPDATE blocks until it resolves
//     rather than reading a stale, about-to-be-invalidated snapshot the
//     way a plain SELECT would.
//   - On SQLite it is the caller's transaction's FIRST database statement
//     -- never preceded by a read -- which is exactly what keeps the whole
//     transaction out of the read-then-write lock-upgrade hazard
//     go/dbkit/AGENTS.md's "SQLite busy timeout" section documents: an
//     ordinary contending writer waits out busy_timeout and succeeds once
//     the holder commits, and only a transaction that read FIRST is
//     refused immediately instead. Every caller of this function MUST
//     call it before issuing any other statement on tx, or this guarantee
//     is void and a contended call can fail immediately instead of
//     waiting -- see withRetry in tree.go for the bounded-retry backstop
//     every caller wraps itself in regardless, for the cases (PostgreSQL
//     deadlocks between two such calls locking two different rows in
//     opposite order, most notably) no single call's own lock order can
//     rule out.
func touchLockByID(tx *gorm.DB, id string) (bool, error) {
	res := tx.
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		Select("DeletedBy").
		Updates(&OrgNode{DeletedBy: ""})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected != 0, nil
}

// lockLiveNode wraps touchLockByID and reads the now-locked row back,
// for callers (CreateChild, Move, Restore) that need the row's own current
// data -- Path and Depth above all -- rather than merely a lock over some
// OTHER statement.
//
// It reports gorm.ErrRecordNotFound -- never wrapped, so a caller matches it
// with errors.Is directly -- for an id with no live row under ctx's tenant:
// never existed, belongs to another tenant (the isolation plugin's own
// filter), or is currently mark-deleted. The caller decides which org-level
// not-found error that maps to (ErrParentNotFound, ErrNodeNotFound,
// ErrRestoreParentNotLive), since an identical gap means a different thing
// at each call site.
//
// The read after the lock is safe to issue -- unlike a read before it would
// have been -- precisely because the write above already happened: this
// transaction already holds whatever this dialect's write-lock shape is for
// this row, so nothing else can change it before this read, or before this
// transaction ends.
func lockLiveNode(tx *gorm.DB, id string) (*OrgNode, error) {
	locked, err := touchLockByID(tx, id)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, gorm.ErrRecordNotFound
	}
	var node OrgNode
	if err := tx.Where("id = ?", id).First(&node).Error; err != nil {
		return nil, err
	}
	return &node, nil
}

// lockSubtree locks every currently-live node matching prefix (a
// materialized-path subtree, per subtreePrefix) and returns them, ordered by
// (depth, id) -- the moved node itself first when prefix names it. Every
// caller other than Move's own helper (all of which start with the subtree's
// top node already locked via lockLiveNode) is expected to have taken that
// lock first; lockSubtree re-locks it too, harmlessly, since a transaction
// re-touching a row it already holds does not block against itself.
//
// # Why a single "path LIKE prefix%" Find is not enough (the Move interior-
// descendant finding)
//
// Move used to read the subtree with one plain, unlocked Find and then
// rewrite each returned row's own Path via its own conditioned UPDATE. That
// leaves every row the scan returns other than the two endpoints Move
// already locks (the moved node, the new parent) completely exposed for the
// whole rewrite: a concurrent CreateChild targeting an INTERIOR descendant
// (never the moved node itself) calls lockLiveNode on THAT row, which Move
// never touches, so the two calls never serialize on anything. Confirmed
// against a real PostgreSQL server: CreateChild's insert can land, and
// commit, entirely within the gap between Move's initial unlocked scan and
// that scan's own row's eventual UPDATE -- the new child's own row is bound
// to the descendant's OLD (pre-move) Path and simply never existed when
// Move's one-shot scan ran, so no later per-row UPDATE in Move's rewrite loop
// ever reaches it. The result is a live node whose stored Path no longer
// matches its own ParentID's current Path -- the exact "Path/ParentID chain
// disagree" corruption assertNoOrphans checks for -- despite every row Move
// DID touch being rewritten correctly.
//
// The fix locks every row of the subtree, not merely its two endpoints,
// before trusting the set is complete: scan for the prefix, lock every
// newly-seen row (touchLockByID, the same primitive lockLiveNode wraps), and
// repeat until a scan returns nothing this call has not already locked. Once
// every row of some scan is already locked by THIS transaction, no further
// insert can land uncontested underneath any of them: a concurrent
// CreateChild's own lockLiveNode(descendantID) call for a row this
// transaction already holds either has already fully committed (so this
// scan's fresh read already reflects it and the loop locks the newly
// discovered child) or blocks behind this transaction entirely (so it cannot
// commit an insert until this transaction is done, at which point it correctly
// re-reads the descendant's now-final, rebased Path rather than a stale one).
// The loop terminates because each iteration either finds nothing new (and
// returns) or locks at least one previously-unseen row, bounded by the
// tenant's total node count.
func lockSubtree(tx *gorm.DB, prefix string) ([]OrgNode, error) {
	seen := map[string]bool{}
	for {
		var rows []OrgNode
		if err := tx.
			Where("path LIKE ?", prefix+"%").
			Order("depth, id").
			Find(&rows).Error; err != nil {
			return nil, err
		}
		var newRows []OrgNode
		for _, n := range rows {
			if !seen[n.ID] {
				newRows = append(newRows, n)
			}
		}
		if len(newRows) == 0 {
			return rows, nil
		}
		for _, n := range newRows {
			seen[n.ID] = true
			if _, err := touchLockByID(tx, n.ID); err != nil {
				return nil, err
			}
		}
	}
}

// assertNameFreeTx is assertNameFree's transaction-bound twin: the same
// sibling-name pre-check, issued against an already-open tx instead of
// opening its own dbkit.WithTenantSession, for callers composing it into a
// larger atomic operation (CreateChild, Move). The database's own
// UNIQUE(tenant_id, parent_id, name) index remains the backstop this
// pre-check cannot close on its own; mapWriteError translates a lost race
// against it into the identical ErrDuplicateSiblingName.
func assertNameFreeTx(tx *gorm.DB, parentID, name string) error {
	var existing OrgNode
	err := tx.Where("parent_id = ?", parentID).Where("name = ?", name).First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	case err != nil:
		return ErrInternal.WithCause(err)
	}
	return ErrDuplicateSiblingName.WithParam("name", name)
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

// deleteLeaf mark-deletes the single node identified by nodeID (whose path
// is exactly prefix), and refuses -- rolling the whole statement back -- if
// that prefix turns out to match more than one currently-live row.
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
// # touchLockByID(nodeID) first, and why the bulk scan alone is not enough
//
// PostgreSQL's own re-check of a row a blocked UPDATE was waiting on
// (EvalPlanQual, under READ COMMITTED) re-verifies only the SPECIFIC ROWS
// the statement's original scan already selected as candidates -- it does
// NOT expand that candidate set to rows that came to match the WHERE clause
// only AFTER the scan ran, a newly INSERTed child above all. Concretely: if
// this UPDATE's own "path LIKE prefix%" scan takes its snapshot BEFORE a
// concurrent CreateChild's lockLiveNode(nodeID) call has locked nodeID, and
// this UPDATE then blocks on nodeID (already locked by that CreateChild),
// unblocking after CreateChild commits does NOT make this UPDATE notice the
// row CreateChild just inserted -- that row never existed when the original
// scan ran, so matched stays 1 (this node alone) instead of 2, and the
// delete proceeds thinking the node is still a childless leaf. The result is
// exactly the D1 orphan (a live child under a now-dead parent) despite
// CreateChild's own lock having been real and properly held -- confirmed as
// a genuine, reproducible failure against a real PostgreSQL server while
// building this round's fix (integration_test/postgres_concurrency_test.go),
// not merely a theoretical concern; SQLite's coarser, whole-file locking
// does not share this specific blind spot; the deleteLeaf-and-Postgres
// combination is not the property either one alone appeared to be. The fix
// is touchLockByID(tx, nodeID) as this function's OWN first statement,
// BEFORE the bulk scan below ever runs: it contends for the identical row
// lockLiveNode(nodeID) takes, so a concurrent CreateChild (or Restore, or
// Move locking this same node) is now forced to either have already fully
// committed, or to block behind THIS call -- either way, the bulk scan that
// follows always starts from a state where no such concurrent writer of
// nodeID itself can still be in flight, so any child it inserted is already
// visible to (or does not yet exist for) this fresh scan. touchLockByID
// reporting nodeID absent-or-dead maps directly to matched == 0 without
// running the bulk scan at all, matching what that scan would have found
// anyway.
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
//
// guard, when non-nil, runs against the SAME already-open transaction right
// after nodeID's own lock succeeds -- BEFORE the bulk mark-delete statement
// below ever runs -- and a non-nil return aborts the whole transaction
// (nothing is written) with that error surfaced unwrapped. TreeService.Delete
// is what passes one: the roster's "does anybody sit in this subtree" check,
// which used to run as its own separate, unlocked read entirely BEFORE this
// method's own transaction opened -- a real, closed TOCTOU window (a
// concurrent MembershipRepository add landing in the gap between that read
// and this method's own commit, leaving a membership bound to a row this
// call is about to soft-delete) that running the check here, inside the
// SAME lock this method already takes, closes: see tree.go's Delete doc
// comment for the full mechanism, and membership.go's MemberService.ensure
// for the other half this fix needed (a plain, unlocked read there could
// otherwise commit its own insert into this exact gap regardless of what
// this method does).
func (r *Repository) deleteLeaf(ctx context.Context, nodeID, prefix string, guard func(tx *gorm.DB) error) (matched int64, err error) {
	now := time.Now()
	deletedBy := softDeleteActor(ctx)
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		locked, lockErr := touchLockByID(tx, nodeID)
		if lockErr != nil {
			return lockErr
		}
		if !locked {
			matched = 0
			return nil
		}
		if guard != nil {
			if guardErr := guard(tx); guardErr != nil {
				return guardErr
			}
		}
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
	case err == nil:
		return matched, nil
	default:
		if appErr, ok := apperr.As(err); ok {
			return 0, appErr
		}
		return 0, ErrInternal.WithCause(err)
	}
}

// deleteSubtree mark-deletes the node identified by nodeID (whose path is
// exactly prefix) and every currently-live node beneath it, in one statement
// inside one transaction, and reports how many rows it touched. Mark-
// deleting a subtree row by row -- for instance by calling the promoted,
// single-row dbkit.Repository[OrgNode].Delete once per descendant -- would
// leave a partially soft-deleted tree behind on any mid-loop failure, and
// would abandon the very atomicity go/org/AGENTS.md's "Known limitations"
// already flags as missing for Move; one UPDATE cannot leave that window.
//
// It follows dbkit's own unexported softDelete shape exactly, the same way
// deleteLeaf's doc comment describes: a real *OrgNode, Model == Dest == &m,
// never a map payload.
//
// touchLockByID(nodeID) runs first for the identical reason deleteLeaf's own
// doc comment gives at length: without it, a concurrent Restore locking
// THIS exact node (as the parent it is about to un-delete something under,
// TreeService.Restore's own case) via lockLiveNode could commit -- reviving
// a descendant -- in the gap between this statement's snapshot and its own
// unblocking, and this bulk scan would never notice that newly-revived row,
// leaving it live under what this call just made a dead parent: the D5
// corruption, confirmed the same way against a real PostgreSQL server
// (integration_test/postgres_concurrency_test.go).
//
// The statement is a plain Updates against a TenantScoped model, so the
// isolation plugin injects the tenant filter here exactly as it did for the
// Delete this replaces. The added "deleted_at IS NULL" leaves an
// already-soft-deleted descendant (from some earlier, independent
// mark-delete) untouched rather than re-stamping its deleted_at/deleted_by
// with this call's own attribution.
//
// guard behaves exactly as deleteLeaf's own doc comment describes: non-nil,
// it runs against this same transaction right after nodeID's lock succeeds
// and before the bulk mark-delete statement, and a non-nil return aborts the
// whole transaction with that error surfaced unwrapped.
//
// It also returns the real id set the bulk mark-delete matched -- every row
// the cascade actually touched, the node itself included -- which is what
// TreeService.publishDeleted needs to widen org.node.deleted's
// DeletedNodeIds beyond a bare count. That set is read INSIDE this same
// transaction, right after guard runs and strictly BEFORE the mark-delete
// UPDATE below: the UPDATE's own RowsAffected is a count with no row
// identity behind it, and reading afterward would see nothing (the
// auto-scope plugin hides a row the moment its deleted_at is set), so the
// only place to capture the set is here, against the exact same WHERE
// clause the UPDATE uses next, inside the one transaction that already
// holds nodeID's lock and -- per this method's own atomicity fix above --
// admits no concurrent insert into this subtree in the gap between the two
// statements.
func (r *Repository) deleteSubtree(ctx context.Context, nodeID, prefix string, guard func(tx *gorm.DB) error) (int64, []string, error) {
	now := time.Now()
	deletedBy := softDeleteActor(ctx)
	var affected int64
	var deletedIDs []string
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		locked, lockErr := touchLockByID(tx, nodeID)
		if lockErr != nil {
			return lockErr
		}
		if !locked {
			affected = 0
			return nil
		}
		if guard != nil {
			if guardErr := guard(tx); guardErr != nil {
				return guardErr
			}
		}
		var rows []OrgNode
		if err := tx.
			Where("path LIKE ?", prefix+"%").
			Where("deleted_at IS NULL").
			Find(&rows).Error; err != nil {
			return ErrInternal.WithCause(err)
		}
		deletedIDs = nodeIDs(rows)
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
		if appErr, ok := apperr.As(err); ok {
			return 0, nil, appErr
		}
		return 0, nil, ErrInternal.WithCause(err)
	}
	return affected, deletedIDs, nil
}
