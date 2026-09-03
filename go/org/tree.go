package org

import (
	"context"
	"errors"

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
type nodeMemberGuard interface {
	anyInNodes(ctx context.Context, nodeIDs []string) (bool, error)
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
func (s *TreeService) CreateChild(ctx context.Context, parentID, name, kind string) (*OrgNode, error) {
	cleanName, err := validateName(name)
	if err != nil {
		return nil, err
	}

	parent, err := s.findParent(ctx, parentID)
	if err != nil {
		return nil, err
	}

	depth := depthOf(parent.Path) + 1
	if depth > s.maxDepth {
		return nil, ErrMaxDepthExceeded.WithParam("max_depth", s.maxDepth)
	}
	if err := s.assertNameFree(ctx, parentID, cleanName); err != nil {
		return nil, err
	}

	id := s.newID()
	if err := validateNodeID(id); err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	node := OrgNode{
		ID:       id,
		ParentID: parent.ID,
		Path:     buildPath(parent.Path, id),
		Depth:    depth,
		Name:     cleanName,
		Kind:     kind,
	}
	if err := s.create(ctx, &node); err != nil {
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
func (s *TreeService) Rename(ctx context.Context, nodeID, name string) (*OrgNode, error) {
	cleanName, err := validateName(name)
	if err != nil {
		return nil, err
	}
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if node.Name == cleanName {
		return node, nil
	}
	if err := s.assertNameFree(ctx, node.ParentID, cleanName); err != nil {
		return nil, err
	}

	node.Name = cleanName
	if err := s.repo.Update(ctx, node); err != nil {
		return nil, mapWriteError(err)
	}
	return node, nil
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
// KNOWN LIMITATION: the rewritten rows are saved one at a time through
// dbkit.Repository[OrgNode].Update, each in its own transaction, because
// Repository[T] exposes no transactional batch seam today. A process that
// dies mid-move therefore leaves a partially re-parented subtree, whose rows'
// paths disagree with their parent edges until the move is repeated. The
// cheap fix -- one statement with a SQL replace() over the path column -- is
// exactly what property 4 forbids, and the right fix is a dbkit round that
// grows Repository[T] a transactional batch write. Recorded in go/org's
// AGENTS.md.
func (s *TreeService) Move(ctx context.Context, nodeID, newParentID string) (*OrgNode, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if pathErr := validatePath(node.Path); pathErr != nil {
		return nil, ErrInternal.WithCause(pathErr)
	}
	if nodeID == newParentID {
		return nil, ErrCycleNotAllowed.WithParam("node_id", nodeID)
	}

	newParent, err := s.findParent(ctx, newParentID)
	if err != nil {
		return nil, err
	}
	if isDescendantOf(newParent.Path, node.Path) {
		return nil, ErrCycleNotAllowed.WithParam("node_id", nodeID)
	}
	if node.ParentID == newParent.ID {
		return node, nil
	}
	if nameErr := s.assertNameFree(ctx, newParent.ID, node.Name); nameErr != nil {
		return nil, nameErr
	}

	newPath := buildPath(newParent.Path, node.ID)
	delta := depthOf(newPath) - depthOf(node.Path)

	subtree, err := s.repo.subtree(ctx, subtreePrefix(node.Path))
	if err != nil {
		return nil, err
	}
	for _, n := range subtree {
		if depthOf(n.Path)+delta > s.maxDepth {
			return nil, ErrMaxDepthExceeded.WithParam("max_depth", s.maxDepth)
		}
	}

	var moved *OrgNode
	for i := range subtree {
		n := subtree[i]
		rebased, ok := rebasePath(n.Path, node.Path, newPath)
		if !ok {
			return nil, ErrInternal.WithCause(ErrInvalidNodeID.WithParam("path", n.Path))
		}
		n.Path = rebased
		n.Depth = depthOf(rebased)
		if n.ID == node.ID {
			n.ParentID = newParent.ID
		}
		if err := s.repo.Update(ctx, &n); err != nil {
			return nil, mapWriteError(err)
		}
		if n.ID == node.ID {
			moved = &n
		}
	}
	if moved == nil {
		return nil, ErrInternal.WithCause(ErrNodeNotFound.WithParam("node_id", nodeID))
	}
	publishEvent(ctx, s.host, EventNodeMoved, NodeMoved{
		NodeID:      moved.ID,
		OldParentID: node.ParentID,
		NewParentID: newParent.ID,
		OldPath:     node.Path,
		NewPath:     moved.Path,
	})
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
// see assertNoMembers.
//
// Both paths are a single statement inside a single transaction, so a node
// cannot be orphaned by a child arriving between a "does it have children?"
// check and the delete itself -- see Repository.deleteLeaf for why that check
// lives inside the transaction rather than ahead of it.
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

	prefix := subtreePrefix(node.Path)
	if memberErr := s.assertNoMembers(ctx, prefix, nodeID); memberErr != nil {
		return memberErr
	}

	if cascade {
		removed, deleteErr := s.repo.deleteSubtree(ctx, prefix)
		if deleteErr != nil {
			return deleteErr
		}
		s.publishDeleted(ctx, *node, true, removed)
		return nil
	}

	matched, err := s.repo.deleteLeaf(ctx, prefix)
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
	s.publishDeleted(ctx, *node, false, matched)
	return nil
}

// assertNoMembers reports ErrNodeHasMembers when anybody is bound inside the
// subtree about to be deleted.
//
// Without it a cascading delete would leave memberships pointing at rows that
// no longer exist, and a dangling membership is not a cosmetic problem: it is
// a person whose data scope can no longer be resolved. org refuses the delete
// rather than deleting the memberships too, for the same reason it refuses to
// re-parent orphans -- silently changing who is in a tenant, or what they can
// see, is not something a structural edit should do on its own. Move the
// members first, then delete the node.
//
// The check is skipped when no roster is wired, which is the case for a
// TreeService constructed on its own.
func (s *TreeService) assertNoMembers(ctx context.Context, prefix, nodeID string) error {
	if s.members == nil {
		return nil
	}
	subtree, err := s.repo.subtree(ctx, prefix)
	if err != nil {
		return err
	}
	occupied, err := s.members.anyInNodes(ctx, nodeIDs(subtree))
	if err != nil {
		return err
	}
	if occupied {
		return ErrNodeHasMembers.WithParam("node_id", nodeID)
	}
	return nil
}

// publishDeleted announces one removed node (and, for a cascade, its whole
// subtree).
func (s *TreeService) publishDeleted(ctx context.Context, node OrgNode, cascade bool, removed int64) {
	publishEvent(ctx, s.host, EventNodeDeleted, NodeDeleted{
		NodeID:       node.ID,
		Path:         node.Path,
		Cascade:      cascade,
		RemovedCount: removed,
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

// findParent resolves a parent node, reporting ErrParentNotFound for an
// empty id, an unknown id, or an id belonging to another tenant. It also
// rejects a parent whose stored path is malformed, so a corrupt row never
// seeds a corrupt child.
func (s *TreeService) findParent(ctx context.Context, parentID string) (*OrgNode, error) {
	if parentID == "" {
		return nil, ErrParentNotFound.WithParam("parent_id", parentID)
	}
	parent, err := s.repo.FindByID(ctx, parentID)
	if err != nil {
		return nil, mapFindError(err, ErrParentNotFound, parentID)
	}
	if err := validatePath(parent.Path); err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return parent, nil
}

// assertNameFree reports ErrDuplicateSiblingName when parentID already has a
// child named name.
func (s *TreeService) assertNameFree(ctx context.Context, parentID, name string) error {
	switch _, err := s.repo.bySiblingName(ctx, parentID, name); {
	case err == nil:
		return ErrDuplicateSiblingName.WithParam("name", name)
	case hasCode(err, ErrNodeNotFound.Code):
		return nil
	default:
		return err
	}
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
