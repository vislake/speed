package org

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TreeService is the only sanctioned way to change a tenant's organization
// tree. It is what keeps a node's authoritative parent edge (ParentID) and
// its derived query index (Path, Depth) in lockstep: writing OrgNode rows
// through Repository directly would let the two drift, and every subtree
// query in this module -- and every data-visibility decision rbac builds on
// top of one -- reads the path, not the edge.
//
// Every method resolves the tenant from ctx through the embedded
// Repository's own fail-closed checks, so a context carrying no tenant fails
// before any statement runs. No method accepts a tenant identifier; there is
// no parameter on this type through which a caller could name one.
type TreeService struct {
	// repo is the tenant-scoped data-access type all reads and writes go
	// through.
	repo *Repository

	// newID generates a node id. It is a field rather than a direct call to
	// uuid.NewString so tests can pin ids -- and so the one place ids enter
	// the system stays visible, since their alphabet is what makes the
	// materialized-path prefix scan dialect-identical (path.go). Whatever it
	// returns is validated by validateNodeID before it is ever stored.
	newID func() string

	// maxDepth is the deepest Depth a node of this tree may carry, defaulting
	// to the package constant of the same name and overridable by a host
	// through Module's WithMaxDepth.
	//
	// It is an option rather than a dynamic configuration item because org
	// cannot read one without importing the config module, which the
	// dependency graph forbids; declaring a config schema this module would
	// then ignore would be a lying schema. See go/org/AGENTS.md.
	maxDepth int

	// members, when wired, is asked whether anybody is bound inside a subtree
	// before that subtree is deleted. Nil leaves the check off, which is the
	// right default for a TreeService built on its own: a tree with no roster
	// beside it cannot orphan a membership.
	members nodeMemberGuard

	// host is the lazily-read view of the host's Registry, used to publish
	// this module's tree events. Nil publishes nothing, which is what a
	// TreeService constructed outside a bootstrapped host does.
	host hostSeams
}

// nodeMemberGuard reports whether any membership is bound to one of the given
// nodes. TreeService uses it to refuse a delete that would leave memberships
// pointing at rows that no longer exist.
//
// It is an interface rather than a direct *MembershipRepository so that the
// tree half of this module stays testable without a roster, and so the
// dependency reads in one direction only: the tree asks a question, the
// roster answers it.
//
// The method is transaction-bound, not ctx-bound: Delete's own doc comment
// explains why the check must run inside the SAME transaction that locks the
// subtree being deleted, rather than as a separate call opening its own
// transaction the way a ctx-bound signature would force.
type nodeMemberGuard interface {
	anyInNodesTx(tx *gorm.DB, nodeIDs []string) (bool, error)
}

// NewTreeService returns a TreeService over db. db is expected to come from
// dbkit.Open, already migrated with this module's Migrations().
func NewTreeService(db *gorm.DB) *TreeService {
	return &TreeService{repo: NewRepository(db), newID: uuid.NewString, maxDepth: maxDepth}
}

// Repository returns the tree's data-access type, for callers that need the
// promoted dbkit.Repository[OrgNode] surface (a host's isolation test, for
// one) rather than a tree operation.
func (s *TreeService) Repository() *Repository { return s.repo }

// Root returns the caller tenant's root node, or ErrNodeNotFound when the
// tenant has no tree yet.
func (s *TreeService) Root(ctx context.Context) (*OrgNode, error) {
	return s.repo.findRoot(ctx)
}

// Get returns one node of the caller's tenant, or ErrNodeNotFound. A node
// belonging to another tenant reports ErrNodeNotFound as well, never a
// distinguishable error: see that error's own doc comment.
func (s *TreeService) Get(ctx context.Context, nodeID string) (*OrgNode, error) {
	node, err := s.repo.FindByID(ctx, nodeID)
	if err != nil {
		return nil, mapFindError(err, ErrNodeNotFound, nodeID)
	}
	return node, nil
}

// Children returns the direct children of nodeID, ordered by name. It
// returns ErrNodeNotFound when nodeID itself does not exist, so an empty
// result always means "this node has no children" rather than "this node may
// not exist".
func (s *TreeService) Children(ctx context.Context, nodeID string) ([]OrgNode, error) {
	if _, err := s.Get(ctx, nodeID); err != nil {
		return nil, err
	}
	return s.repo.children(ctx, nodeID)
}

// CreateRoot creates the caller tenant's root node.
//
// A tenant has exactly one root: a second call reports ErrRootAlreadyExists.
// That invariant is load-bearing beyond tidiness -- Move relies on every
// node in a tenant descending from the root, which is what makes moving the
// root itself always report ErrCycleNotAllowed instead of needing a rule of
// its own.
func (s *TreeService) CreateRoot(ctx context.Context, name, kind string) (*OrgNode, error) {
	cleanName, err := validateName(name)
	if err != nil {
		return nil, err
	}

	switch _, err := s.repo.findRoot(ctx); {
	case err == nil:
		return nil, ErrRootAlreadyExists
	case !hasCode(err, ErrNodeNotFound.Code):
		return nil, err
	}

	id := s.newID()
	if err := validateNodeID(id); err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	node := OrgNode{
		ID:       id,
		ParentID: "",
		Path:     buildPath("", id),
		Depth:    rootDepth,
		Name:     cleanName,
		Kind:     kind,
	}
	if err := s.create(ctx, &node); err != nil {
		return nil, err
	}
	s.publishCreated(ctx, node)
	return &node, nil
}

// CreateChild creates a node beneath parentID.
//
// The child's path and depth are derived from the parent's stored path, not
// from its stored depth, so the two can never disagree in a freshly written
// row; a parent whose stored path is malformed reports ErrInternal rather
// than propagating the corruption into a new row.
//
// # Atomicity against a concurrent delete of the parent
//
// findParent's read, the sibling-name check and the insert all run inside
// ONE dbkit.WithTenantSession transaction, with the parent row locked first
// through lockLiveNode -- never a plain read followed by a separate write,
// which is exactly the shape that used to leave a window between reading
// the parent as live and inserting the child: deleteLeaf's own mark-delete
// is a single UPDATE inside its own single-statement transaction, so a
// concurrent CreateChild whose parent-liveness check was a plain,
// unlocked read could observe the parent as live in the gap between that
// UPDATE and its COMMIT, and go on to insert a child whose stored path
// still named the parent's old, about-to-be-invalidated position -- the
// live-child-under-a-soft-deleted-parent corruption path.go's own doc
// comment calls out as never a supported state. lockLiveNode's blind,
// no-prior-read touch-update closes that: on PostgreSQL it takes the
// parent row's write lock and blocks until any concurrent deleteLeaf either
// commits (after which this call's own lock attempt correctly fails to
// match a now-dead row) or rolls back; on SQLite it is this transaction's
// first statement, which keeps it out of the read-then-write lock-upgrade
// hazard go/dbkit/AGENTS.md's "SQLite busy timeout" section documents.
//
// The whole transaction is wrapped in withRetry: SQLite contention this
// shape cannot itself avoid, and a PostgreSQL deadlock against an
// unrelated concurrent Move locking the same parent in the opposite
// order, both retry rather than surface as a raw, undocumented failure.
func (s *TreeService) CreateChild(ctx context.Context, parentID, name, kind string) (*OrgNode, error) {
	cleanName, err := validateName(name)
	if err != nil {
		return nil, err
	}
	if parentID == "" {
		return nil, ErrParentNotFound.WithParam("parent_id", parentID)
	}

	id := s.newID()
	if idErr := validateNodeID(id); idErr != nil {
		return nil, ErrInternal.WithCause(idErr)
	}

	var node OrgNode
	err = withRetry(func() error {
		return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
			parent, lockErr := lockLiveNode(tx, parentID)
			if lockErr != nil {
				if errors.Is(lockErr, gorm.ErrRecordNotFound) {
					return ErrParentNotFound.WithParam("parent_id", parentID)
				}
				return ErrInternal.WithCause(lockErr)
			}
			if pathErr := validatePath(parent.Path); pathErr != nil {
				return ErrInternal.WithCause(pathErr)
			}

			depth := depthOf(parent.Path) + 1
			if depth > s.maxDepth {
				return ErrMaxDepthExceeded.WithParam("max_depth", s.maxDepth)
			}
			if nameErr := assertNameFreeTx(tx, parentID, cleanName); nameErr != nil {
				return nameErr
			}

			node = OrgNode{
				ID:       id,
				ParentID: parent.ID,
				Path:     buildPath(parent.Path, id),
				Depth:    depth,
				Name:     cleanName,
				Kind:     kind,
			}
			if createErr := tx.Create(&node).Error; createErr != nil {
				return mapWriteError(createErr)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	s.publishCreated(ctx, node)
	return &node, nil
}

// Rename changes a node's display name. The name must stay unique among the
// node's siblings; renaming a node to the name it already has is a no-op that
// returns the node unchanged rather than an ErrDuplicateSiblingName against
// itself.
//
// A rename never touches Path: the path is built from ids, so a node's
// identity in every subtree query survives any number of renames.
//
// # Atomicity against a concurrent delete of the node being renamed
//
// The node read, the sibling-name check and the name update all run inside
// ONE dbkit.WithTenantSession transaction, with the node's own row locked
// first through lockLiveNode -- never a plain read followed by a separate
// write, which is exactly the shape this method used before. The old shape
// was s.Get, then a standalone dbkit.Repository[OrgNode].Update, and that
// Update is a full-field Save that writes the caller's in-memory model back
// over the whole row: a concurrent Delete soft-deleting this same node
// between the two calls had its committed mark (deleted_at/deleted_by,
// written by the delete's own UPDATE) silently overwritten by the rename's
// stale, pre-delete snapshot -- deleted_at written back to NULL -- silently
// resurrecting a node the caller had just deleted. lockLiveNode closes that
// the same way it closes it for CreateChild and Move: its blind, no-prior-
// read touch-update either blocks until a concurrent delete of this row
// resolves (and then correctly fails to match the now-dead row), or takes
// the lock itself, in which case a delete starting afterward blocks behind
// this transaction instead of racing it -- so the name UPDATE that follows
// can never land on a row deleted after this call's own read, and the
// update's own conditional WHERE (id + deleted_at IS NULL) re-checks the
// row's liveness at write time regardless. The whole transaction is wrapped
// in withRetry for the same contention reasons Move's own doc comment gives.
func (s *TreeService) Rename(ctx context.Context, nodeID, name string) (*OrgNode, error) {
	cleanName, err := validateName(name)
	if err != nil {
		return nil, err
	}

	var renamed *OrgNode
	err = withRetry(func() error {
		renamed = nil
		return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
			node, lockErr := lockLiveNode(tx, nodeID)
			if lockErr != nil {
				if errors.Is(lockErr, gorm.ErrRecordNotFound) {
					return ErrNodeNotFound.WithParam("node_id", nodeID)
				}
				return ErrInternal.WithCause(lockErr)
			}
			if node.Name == cleanName {
				renamed = node
				return nil
			}
			if nameErr := assertNameFreeTx(tx, node.ParentID, cleanName); nameErr != nil {
				return nameErr
			}

			res := tx.
				Where("id = ?", node.ID).
				Where("deleted_at IS NULL").
				Select("Name").
				Updates(&OrgNode{Name: cleanName})
			if res.Error != nil {
				return mapWriteError(res.Error)
			}
			if res.RowsAffected == 0 {
				return ErrInternal.WithCause(fmt.Errorf(
					"org: node %s vanished between rename lock and update", nodeID))
			}
			node.Name = cleanName
			renamed = node
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return renamed, nil
}

// Move re-parents a node, carrying its whole subtree with it.
//
// It is rejected when:
//
//   - newParentID does not name a node of the caller's tenant
//     (ErrParentNotFound) -- which is also what a cross-tenant target
//     reports, since the lookup is tenant-scoped;
//   - the target is the node itself or one of its own descendants
//     (ErrCycleNotAllowed), which would detach the subtree from the tree;
//   - the deepest node in the moved subtree would land past maxDepth
//     (ErrMaxDepthExceeded);
//   - the node's name is already taken among the target's children
//     (ErrDuplicateSiblingName).
//
// Moving the tenant root always reports ErrCycleNotAllowed: every node of a
// tenant descends from its single root, so every candidate target is one of
// the root's own descendants. That falls out of the invariant rather than
// needing a rule of its own.
//
// The rewrite is done entirely in Go -- see path.go's dialect-identity proof,
// property 4. No SQL string function is used, so the only structurally risky
// operation in this module carries no dialect-specific SQL at all.
//
// The whole rewrite -- reading the node, reading the new parent, the
// sibling-name check, the subtree scan and every rewritten row -- runs
// inside ONE dbkit.WithTenantSession transaction, with the moved node's own
// row and the new parent's row each locked through lockLiveNode BEFORE
// either is read for its authoritative Path: a plain, unlocked read of
// either (the shape this method used before) can observe a row as live and
// still hand back a Path a concurrent writer of that SAME row is about to
// invalidate -- a concurrent Delete soft-deleting the node or an ancestor,
// or a concurrent Move re-parenting the SAME node again -- landing this
// call's rewrite on a stale prefix once the concurrent writer commits.
// lockLiveNode's blind, no-prior-read touch-update rules that out: on
// PostgreSQL it takes the row's write lock and blocks until any concurrent
// writer of the SAME row resolves, so the read that follows it always
// reflects the true current state, not a snapshot from before that writer's
// commit; on SQLite it is this transaction's first statement (see
// lockLiveNode's own doc comment for why every subsequent read and write in
// the same transaction is then safe from the read-then-write lock-upgrade
// hazard go/dbkit/AGENTS.md's "SQLite busy timeout" section documents).
// Every row of the subtree -- locked fresh inside this same transaction,
// after the moved node's own lock is already held, through lockSubtree
// (repository.go) rather than a single plain, unlocked Find -- is then
// rewritten by its own conditional UPDATE (id + deleted_at IS NULL), so a
// descendant soft-deleted or otherwise altered between the scan and its own
// rewrite reports ErrInternal rather than silently landing a stale row.
//
// # Locking every descendant, not merely the two endpoints
//
// A plain, unlocked subtree scan leaves every row it returns other than the
// two rows already locked above (the moved node, the new parent) completely
// exposed for the whole rewrite that follows: a concurrent CreateChild
// targeting an INTERIOR descendant of the moved subtree -- never the moved
// node itself -- takes its own lockLiveNode on THAT row, which this method
// never otherwise touches, so the two calls never serialize on anything.
// Confirmed against a real PostgreSQL server: such a CreateChild's insert
// can land, and commit, entirely within the gap between this method's own
// subtree scan and that scan's later per-row rewrite -- the new child's row
// is bound to the descendant's OLD, pre-move Path and simply never existed
// when the one-shot scan ran, so nothing in the rewrite loop ever reaches
// it, leaving a live node whose Path no longer matches its own ParentID's
// current Path once this method commits. lockSubtree closes this the same
// way lockLiveNode closes it for a single row: it locks every row a scan
// returns, then re-scans, repeating until a scan turns up nothing this
// transaction has not already locked -- see its own doc comment in
// repository.go for why that loop is what actually rules out a new insert
// slipping in under an already-discovered descendant.
//
// # Concurrent Move || Move, Move || CreateChild, Move || Delete
//
// Two overlapping Moves serialize on whichever row they both lock first
// (the earlier one's moved-node lock, or its new-parent lock, or now any
// descendant lockSubtree takes, blocking the later one's attempt at the
// same row) and each observes the other's committed result once unblocked
// -- outcome equal to some serial order of the two, never a mix of both. A
// concurrent CreateChild targeting any node of this Move's subtree, not
// merely its two endpoints, takes the identical row lock through the same
// lockLiveNode/touchLockByID primitive, so the two calls serialize on that
// row exactly the same way, and whichever runs second re-reads the row's
// current Path after acquiring the lock -- never the stale one read before
// it blocked. A concurrent Delete of the moved node, an ancestor, or now any
// descendant is likewise a writer of one of the same rows this call locks,
// and is now inside this same locking discipline rather than racing it as a
// wholly independent, unguarded statement.
//
// # Deadlock, honestly
//
// Two Moves whose lock orders genuinely cross -- A locks X then wants Y
// while B locks Y then wants X -- can still deadlock under PostgreSQL's
// row-level locking; PostgreSQL detects this itself and aborts one side
// with a real, distinguishable error. withRetry (see concurrency.go) is
// what turns that into a transparent retry rather than a surfaced failure:
// the aborted side simply runs again from a clean read, and past a small,
// bounded number of such losses (txRetryBudget) reports the coded
// ErrConcurrentUpdate instead of hanging.
//
// KNOWN LIMITATION: a process that dies mid-transaction (after some rows
// commit is impossible -- the whole rewrite is one transaction now, so it is
// all-or-nothing -- but a process that dies BETWEEN this call returning and
// its caller acting on the result can still leave that caller's own
// downstream state stale, the same as any other successfully committed
// write.
func (s *TreeService) Move(ctx context.Context, nodeID, newParentID string) (*OrgNode, error) {
	if nodeID == newParentID {
		return nil, ErrCycleNotAllowed.WithParam("node_id", nodeID)
	}

	var moved *OrgNode
	var oldParentID, oldPath string
	var changed bool
	err := withRetry(func() error {
		moved, changed = nil, false
		return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
			node, lockErr := lockLiveNode(tx, nodeID)
			if lockErr != nil {
				if errors.Is(lockErr, gorm.ErrRecordNotFound) {
					return ErrNodeNotFound.WithParam("node_id", nodeID)
				}
				return ErrInternal.WithCause(lockErr)
			}
			if pathErr := validatePath(node.Path); pathErr != nil {
				return ErrInternal.WithCause(pathErr)
			}

			newParent, lockErr := lockLiveNode(tx, newParentID)
			if lockErr != nil {
				if errors.Is(lockErr, gorm.ErrRecordNotFound) {
					return ErrParentNotFound.WithParam("parent_id", newParentID)
				}
				return ErrInternal.WithCause(lockErr)
			}
			if pathErr := validatePath(newParent.Path); pathErr != nil {
				return ErrInternal.WithCause(pathErr)
			}

			if isDescendantOf(newParent.Path, node.Path) {
				return ErrCycleNotAllowed.WithParam("node_id", nodeID)
			}
			if node.ParentID == newParent.ID {
				moved = node
				return nil
			}
			if nameErr := assertNameFreeTx(tx, newParent.ID, node.Name); nameErr != nil {
				return nameErr
			}

			newPath := buildPath(newParent.Path, node.ID)
			delta := depthOf(newPath) - depthOf(node.Path)

			subtree, lockSubErr := lockSubtree(tx, subtreePrefix(node.Path))
			if lockSubErr != nil {
				return ErrInternal.WithCause(lockSubErr)
			}
			for _, n := range subtree {
				if depthOf(n.Path)+delta > s.maxDepth {
					return ErrMaxDepthExceeded.WithParam("max_depth", s.maxDepth)
				}
			}

			oldParentID, oldPath = node.ParentID, node.Path
			for i := range subtree {
				n := subtree[i]
				rebased, ok := rebasePath(n.Path, node.Path, newPath)
				if !ok {
					return ErrInternal.WithCause(ErrInvalidNodeID.WithParam("path", n.Path))
				}
				rowParentID := n.ParentID
				if n.ID == node.ID {
					rowParentID = newParent.ID
				}
				res := tx.
					Where("id = ?", n.ID).
					Where("deleted_at IS NULL").
					Select("Path", "Depth", "ParentID").
					Updates(&OrgNode{Path: rebased, Depth: depthOf(rebased), ParentID: rowParentID})
				if res.Error != nil {
					return mapWriteError(res.Error)
				}
				if res.RowsAffected == 0 {
					return ErrInternal.WithCause(fmt.Errorf(
						"org: descendant %s of moved node %s vanished mid-move", n.ID, nodeID))
				}
				n.Path = rebased
				n.Depth = depthOf(rebased)
				n.ParentID = rowParentID
				if n.ID == node.ID {
					moved = &n
				}
			}
			changed = true
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if moved == nil {
		return nil, ErrInternal.WithCause(ErrNodeNotFound.WithParam("node_id", nodeID))
	}
	if changed {
		publishEvent(ctx, s.host, EventNodeMoved, NodeMoved{
			NodeID:      moved.ID,
			OldParentID: oldParentID,
			NewParentID: moved.ParentID,
			OldPath:     oldPath,
			NewPath:     moved.Path,
		})
	}
	return moved, nil
}

// publishCreated announces one newly created node.
func (s *TreeService) publishCreated(ctx context.Context, node OrgNode) {
	publishEvent(ctx, s.host, EventNodeCreated, NodeCreated{
		NodeID:   node.ID,
		ParentID: node.ParentID,
		Path:     node.Path,
		Depth:    node.Depth,
		Kind:     node.Kind,
	})
}

// Delete removes a node.
//
// A node with children is removed only when cascade is true, in which case
// its whole subtree goes with it in one statement; otherwise the call reports
// ErrNodeHasChildren. org deliberately does NOT re-parent orphans to the
// grandparent: doing so silently widens the data scope of every member bound
// beneath the deleted node, which turns a delete into a privilege escalation.
//
// The tenant root is never deletable (ErrRootNotDeletable): removing it would
// leave the tenant with no tree and every membership dangling.
//
// A node with members bound to it, or to anything beneath it, is not
// deletable either (ErrNodeHasMembers) once a host has wired the roster --
// see memberGuardFor.
//
// Both paths are a single statement inside a single transaction, so a node
// cannot be orphaned by a child arriving between a "does it have children?"
// check and the delete itself -- see Repository.deleteLeaf for why that check
// lives inside the transaction rather than ahead of it. Both now ALSO take
// lockLiveNode's own lock on nodeID as their transaction's first statement,
// before the bulk LIKE-prefix scan runs, closing the cross-operation window
// a concurrent CreateChild, Move or Restore locking this SAME node could
// otherwise open even with deleteLeaf/deleteSubtree's own single-statement
// guarantee intact -- see those methods' own doc comments in repository.go
// for the exact PostgreSQL mechanism this closes, and withRetry (below) for
// why the whole call is retried rather than left to surface a transient
// conflict as a raw failure.
//
// # The sweep's path prefix is derived under the lock, never by this method
//
// This method's own outer Get above is an ordinary, unlocked read: a
// concurrent Move of nodeID can commit between that read and the sweep's
// transaction, so a prefix this method computed from the read node's Path
// could name the node's old position -- and the cascade would then match
// zero rows while this method reported success, publishing org.node.deleted
// with an empty DeletedNodeIds (a silent 204 that deleted nothing).
// deleteLeaf/deleteSubtree therefore re-read the node INSIDE their
// transaction, right after their own lock on it succeeds, and derive the
// sweep prefix from that locked row's current Path -- see deleteLeaf's doc
// comment in repository.go. A delete that races a Move of the same node thus
// always resolves to one of two consistent outcomes -- the cascade removes
// the node at its current location, or the node is gone (below) -- never a
// silent zero-match. The outer Get above is kept only for the cheap
// not-found/root/validation errors this method reports before opening a
// write transaction; what it observed is never trusted for the sweep itself.
// One honest consequence: the org.node.deleted event's Path field may name
// the node's position as of that outer read if a Move commits mid-call --
// DeletedNodeIds is the authoritative statement of what was removed.
//
// # The cascade branch treats "nothing to remove" as node_not_found
//
// deleteSubtree reports removed == 0 when its lock finds nodeID already
// gone (a concurrent delete of the same node won first -- the identical
// situation the non-cascade branch's matched == 0 maps to ErrNodeNotFound).
// The cascade branch must answer the same coded error rather than treating
// zero as a success to announce: publishing org.node.deleted with
// RemovedCount 0 and an empty DeletedNodeIds would tell every subscriber a
// deletion happened when nothing was deleted.
//
// # The members check runs INSIDE the same locked transaction, not before it
//
// The original shape here ran the roster check (then named assertNoMembers)
// as its own separate, unlocked read entirely BEFORE deleteLeaf/deleteSubtree
// ever opened their own transaction -- a plain "check, then act" pair with a
// real, unguarded window in between: a concurrent MemberService.Add binding a
// fresh membership to any node of the subtree in that gap would sail through
// (the check already ran and found nothing) while the cascade proceeded to
// soft-delete the whole subtree regardless, leaving that just-created
// membership bound to a now-invisible row -- confirmed reproducible against
// a real, plain SQLite database (no PostgreSQL-specific timing needed: this
// was a wide-open gap on both dialects, since the two reads/writes involved
// share no lock at all). memberGuardFor below closes it by moving the check
// inside deleteLeaf/deleteSubtree's own transaction, run right after nodeID's
// lock succeeds and the current prefix is derived -- deleteLeaf runs it
// strictly before its own mark-delete statement, deleteSubtree only AFTER
// lockSubtree has locked every row of the subtree (see deleteSubtree's doc
// comment for why a cascade's guard cannot run before that wider lock: an
// Add to an INTERIOR descendant locks only that row, which a cascade has not
// touched at guard time) -- so it sees either a membership already committed
// before this transaction's own lock was taken (and refuses), or nothing yet,
// in which case a concurrent Add attempting to bind under one of these SAME
// rows is forced through the other half of this fix: MemberService.ensure
// (membership.go) now takes the identical lockLiveNode lock on its target
// node before creating the membership, so it either already committed (and
// this transaction's own read, above, sees it) or blocks behind this
// transaction and, once it resumes, correctly discovers the node it wanted
// is now mark-deleted and refuses with ErrNodeNotFound instead of completing
// a dangling insert.
func (s *TreeService) Delete(ctx context.Context, nodeID string, cascade bool) error {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	if node.IsRoot() {
		return ErrRootNotDeletable.WithParam("node_id", nodeID)
	}
	if pathErr := validatePath(node.Path); pathErr != nil {
		return ErrInternal.WithCause(pathErr)
	}

	guard := s.memberGuardFor(nodeID)

	if cascade {
		var removed int64
		var deletedIDs []string
		retryErr := withRetry(func() error {
			var deleteErr error
			removed, deletedIDs, deleteErr = s.repo.deleteSubtree(ctx, nodeID, guard)
			return deleteErr
		})
		if retryErr != nil {
			return retryErr
		}
		if removed == 0 {
			return ErrNodeNotFound.WithParam("node_id", nodeID)
		}
		s.publishDeleted(ctx, *node, true, removed, deletedIDs)
		return nil
	}

	var matched int64
	err = withRetry(func() error {
		var deleteErr error
		matched, deleteErr = s.repo.deleteLeaf(ctx, nodeID, guard)
		return deleteErr
	})
	if err != nil {
		return err
	}
	switch {
	case matched == 0:
		return ErrNodeNotFound.WithParam("node_id", nodeID)
	case matched > 1:
		return ErrNodeHasChildren.
			WithParam("node_id", nodeID).
			WithParam("descendant_count", matched-1)
	}
	// A non-cascading delete only ever matches one row -- itself -- or the
	// switch above would already have refused it as ErrNodeHasChildren, so
	// the deleted-id set is trivially [nodeID] with no extra query needed.
	s.publishDeleted(ctx, *node, false, matched, []string{nodeID})
	return nil
}

// memberGuardFor returns the transaction-bound closure deleteLeaf/deleteSubtree
// run -- right after nodeID's own lock succeeds and the current prefix is
// derived, in whichever position the caller's doc comment specifies relative
// to its own sweep -- to report ErrNodeHasMembers when anybody is bound
// inside the subtree about to be deleted. The prefix arrives as an argument
// from the caller of the closure (the repository method running the sweep),
// never captured here: it is only authoritative once derived under the lock,
// which happens inside deleteLeaf/deleteSubtree, not in this method.
//
// Without it a cascading delete would leave memberships pointing at rows that
// no longer exist, and a dangling membership is not a cosmetic problem: it is
// a person whose data scope can no longer be resolved. org refuses the delete
// rather than deleting the memberships too, for the same reason it refuses to
// re-parent orphans -- silently changing who is in a tenant, or what they can
// see, is not something a structural edit should do on its own. Move the
// members first, then delete the node.
//
// The returned closure is a no-op when no roster is wired, which is the case
// for a TreeService constructed on its own -- deleteLeaf/deleteSubtree still
// receive it and still call it, but it immediately reports no members every
// time, exactly as the check being entirely absent used to behave.
func (s *TreeService) memberGuardFor(nodeID string) func(tx *gorm.DB, prefix string) error {
	return func(tx *gorm.DB, prefix string) error {
		if s.members == nil {
			return nil
		}
		var subtree []OrgNode
		if err := tx.Where("path LIKE ?", prefix+"%").Find(&subtree).Error; err != nil {
			return ErrInternal.WithCause(err)
		}
		occupied, err := s.members.anyInNodesTx(tx, nodeIDs(subtree))
		if err != nil {
			return err
		}
		if occupied {
			return ErrNodeHasMembers.WithParam("node_id", nodeID)
		}
		return nil
	}
}

// publishDeleted announces one removed node (and, for a cascade, its whole
// subtree). deletedIDs is the real row set the delete removed -- see
// Repository.deleteSubtree's own doc comment for where it is captured.
func (s *TreeService) publishDeleted(ctx context.Context, node OrgNode, cascade bool, removed int64, deletedIDs []string) {
	publishEvent(ctx, s.host, EventNodeDeleted, NodeDeleted{
		NodeID:         node.ID,
		Path:           node.Path,
		Cascade:        cascade,
		RemovedCount:   removed,
		DeletedNodeIds: deletedIDs,
	})
}

// Restore makes a previously mark-deleted node visible again, wrapping the
// promoted dbkit.Repository[OrgNode].Restore -- the mark-delete inverse
// Repository.deleteLeaf/deleteSubtree's own doc comments point to -- and
// re-reading the row so the caller gets its current data back, exactly as
// Rename does after its own write.
//
// It reports ErrNodeNotFound both for an id with nothing to restore (never
// existed, belongs to another tenant) and for an id that exists but is not
// currently mark-deleted -- the identical collapsed signal
// dbkit.Repository[T].Restore's own doc comment describes and mapFindError
// already applies everywhere else in this file, so a caller cannot learn
// which case it hit from the error shape alone.
//
// # Restore is per-node, never cascading
//
// Restoring nodeID undoes exactly the mark-delete of nodeID itself. It does
// NOT restore any descendant a cascading TreeService.Delete soft-deleted
// alongside it, and does not attempt to distinguish "deleted in that same
// cascade" from "independently soft-deleted earlier for an unrelated
// reason" -- the two are indistinguishable without inventing a batch marker
// this round does not add. That is a deliberate design decision, not an
// oversight: it is the same "no implicit side effect on a structural edit"
// discipline this file already applies elsewhere -- Delete does not
// re-parent orphans to the grandparent (that would silently widen a
// member's data scope) and does not delete memberships along with a node
// (the members guard refuses instead) -- and a cascading Restore would carry
// the identical failure mode Delete's own doc comments warn against:
// resurrecting a descendant that was independently, deliberately removed
// for a reason of its own, just because some ancestor happened to be
// restored later. A node restored here may therefore have descendants that
// stay invisible, mark-deleted, exactly as Delete's cascade left them (or
// as their own, earlier, unrelated delete left them); the caller restores
// each such descendant explicitly by id. See go/org/AGENTS.md's "Soft
// deletion" section for the full rationale and the resulting known
// limitation -- org exposes no query today that lists a node's
// mark-deleted descendants, which a future admin restore surface would
// need.
//
// Restore is deliberately not exposed over HTTP this round; see
// go/org/AGENTS.md's "Soft deletion" section for why.
//
// # Restore refuses to land a node on a dead parent
//
// Per-node-not-cascading (above) means a restored node's PARENT can still be
// mark-deleted -- most commonly because a cascading Delete took both down
// together, and only the descendant has been restored so far. Restoring
// nodeID in that state would silently create a structurally broken tree that
// is NOT fail-closed the way the rest of this module is: Subtree scans down
// from a live ancestor above the gap would surface the restored node (the
// prefix-match LIKE scan cannot see that an intermediate row is invisible),
// CreateChild would let a caller grow a fresh subtree hanging off it, and
// Ancestors/byIDs would silently drop the invisible parent from the chain
// rather than reporting a corrupt read. That directly contradicts path.go's
// own invariant that a node whose Path disagrees with its ParentID chain is
// "corrupt, not a supported state," so Restore checks the immediate parent's
// liveness BEFORE writing anything and reports ErrRestoreParentNotLive
// instead. The caller restores top-down, ancestor before descendant, exactly
// as TestTreeService_Restore_IsNotCascading already does for the
// still-live-ancestor case; restoring the tenant root itself needs no such
// check, since a root's ParentID is always the empty-string sentinel and it
// is never itself deletable (ErrRootNotDeletable).
//
// # Atomicity against a concurrent cascade delete of the parent
//
// The initial findByIDIncludingDeleted read below stays its own, separate,
// read-only call: existing.ParentID cannot itself go stale in a way that
// matters, since nothing changes a soft-deleted row's ParentID before it is
// restored (Move only ever touches live rows). What DOES need to be atomic
// is the parent-liveness CHECK and the restore WRITE together: the original
// shape here (a plain s.Get(parentID) read, then a separate
// s.repo.Restore call) left exactly the window a concurrent cascade
// TreeService.Delete of the ancestor could land in, committing between the
// two and leaving the freshly restored node under a now-dead parent -- the
// corrupt state this method's own "refuses to land a node on a dead parent"
// section above exists to rule out.
//
// The fix runs both steps inside ONE dbkit.WithTenantSession transaction,
// parent-lock first: lockLiveNode's blind, no-prior-read touch-update on
// existing.ParentID either blocks until a concurrent cascade delete of that
// same parent resolves (and then correctly fails to match once it commits),
// or takes the lock itself, in which case a delete of THAT parent starting
// afterward blocks behind this transaction instead of racing it -- so the
// restore write that follows, inside the same transaction, can never
// observe a parent that was live at lock time but dead by the time the
// restore itself commits. withRetry wraps the whole thing for the same
// contention reasons Move's own doc comment gives.
func (s *TreeService) Restore(ctx context.Context, nodeID string) (*OrgNode, error) {
	existing, err := s.repo.findByIDIncludingDeleted(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	err = withRetry(func() error {
		return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
			if !existing.IsRoot() {
				if _, lockErr := lockLiveNode(tx, existing.ParentID); lockErr != nil {
					if errors.Is(lockErr, gorm.ErrRecordNotFound) {
						return ErrRestoreParentNotLive.
							WithParam("node_id", nodeID).
							WithParam("parent_id", existing.ParentID)
					}
					return ErrInternal.WithCause(lockErr)
				}
			}
			return restoreNodeTx(tx, nodeID)
		})
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNodeNotFound.WithParam("node_id", nodeID)
		}
		return nil, err
	}

	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	s.publishRestored(ctx, *node)
	return node, nil
}

// restoreNodeTx clears deleted_at/deleted_by on the node identified by id,
// inside an already-open transaction, mirroring
// dbkit.Repository[OrgNode].Restore's own conditional shape (deleted_at IS
// NOT NULL, so a live or never-existing row reports gorm.ErrRecordNotFound
// rather than silently no-opping) without opening a second transaction of
// its own -- TreeService.Restore needs this write in the SAME transaction as
// its parent-liveness lock, which the promoted, single-call Restore method
// cannot express.
func restoreNodeTx(tx *gorm.DB, id string) error {
	res := tx.
		Where("id = ?", id).
		Where("deleted_at IS NOT NULL").
		Unscoped().
		Select("DeletedAt", "DeletedBy").
		Updates(&OrgNode{DeletedAt: nil, DeletedBy: ""})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// publishRestored announces one node made visible again by Restore.
func (s *TreeService) publishRestored(ctx context.Context, node OrgNode) {
	publishEvent(ctx, s.host, EventNodeRestored, NodeRestored{
		NodeID: node.ID,
		Path:   node.Path,
	})
}

// Ancestors returns nodeID's ancestors, root first, excluding nodeID itself.
// The tenant root has no ancestors and yields an empty slice.
//
// The chain is read from the node's own materialized path -- one query for
// however deep the node sits, no recursion, no per-level round trip.
func (s *TreeService) Ancestors(ctx context.Context, nodeID string) ([]OrgNode, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if pathErr := validatePath(node.Path); pathErr != nil {
		return nil, ErrInternal.WithCause(pathErr)
	}

	segments := pathSegments(node.Path)
	return s.repo.byIDs(ctx, segments[:len(segments)-1])
}

// Descendants returns every node strictly beneath nodeID, ordered by (depth,
// id). Use Subtree when the node itself should be included.
func (s *TreeService) Descendants(ctx context.Context, nodeID string) ([]OrgNode, error) {
	subtree, err := s.Subtree(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]OrgNode, 0, len(subtree))
	for _, n := range subtree {
		if n.ID != nodeID {
			out = append(out, n)
		}
	}
	return out, nil
}

// Subtree returns nodeID together with every node beneath it, ordered by
// (depth, id) -- the node itself first. It is one indexed prefix scan
// regardless of how deep the subtree runs.
func (s *TreeService) Subtree(ctx context.Context, nodeID string) ([]OrgNode, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if pathErr := validatePath(node.Path); pathErr != nil {
		return nil, ErrInternal.WithCause(pathErr)
	}
	return s.repo.subtree(ctx, subtreePrefix(node.Path))
}

// create inserts node, translating a lost race on the sibling-name unique
// index into the same ErrDuplicateSiblingName the pre-check reports, so the
// caller sees one error for one condition however the collision was detected.
func (s *TreeService) create(ctx context.Context, node *OrgNode) error {
	if err := s.repo.Create(ctx, node); err != nil {
		return mapWriteError(err)
	}
	return nil
}

// mapFindError translates dbkit's tenant-scoped not-found into the org-level
// error the caller asked for, leaving every other error (a missing tenant
// context above all) untouched so it keeps its own meaning.
func mapFindError(err error, notFound *apperr.Error, id string) error {
	if hasCode(err, dbkit.ErrRecordNotFound.Code) {
		return notFound.WithParam("node_id", id)
	}
	return err
}

// mapWriteError translates a lost race on the sibling-name unique index into
// ErrDuplicateSiblingName. gorm.ErrDuplicatedKey is dialect-agnostic because
// dbkit.Open enables gorm's TranslateError, so this one check covers both
// PostgreSQL and SQLite.
func mapWriteError(err error) error {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicateSiblingName.WithCause(err)
	}
	if hasCode(err, dbkit.ErrRecordNotFound.Code) {
		return ErrNodeNotFound.WithCause(err)
	}
	return err
}

// hasCode reports whether err is, or wraps, an *apperr.Error with the given
// code. Codes are compared rather than pointers because WithParam and
// WithCause derive a new *apperr.Error every time.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
