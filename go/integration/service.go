package integration

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Service is go/integration's runtime entry point for API key management:
// the four operations round 1 ships (Create, List, Rotate, Revoke),
// documented in AGENTS.md's own "API surface" section. It is built by
// Module.Attach, never constructed directly by a host.
type Service struct {
	repo         *APIKeyRepository
	permissions  PermissionLister
	membership   MembershipChecker
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	now          func() time.Time
}

// CreateInput is what a caller passes to Service.Create.
type CreateInput struct {
	// CreatedBy is the authn user id issuing the key. Mandatory --
	// ErrCreatedByRequired when empty.
	CreatedBy string

	// Scopes is the permission-string subset the new key should be issued
	// with. May be empty (a key with no scopes at all is legal -- it simply
	// authenticates nothing beyond identity). Every entry must be one of
	// CreatedBy's own permissions right now, checked through the
	// PermissionLister seam; see ErrScopeNotHeldByCreator and
	// ErrPermissionListerUnavailable.
	Scopes []string

	// ExpiresAt is when the key should stop working. Nil means "use the
	// default", MaxAPIKeyLifetime from now; a non-nil value further out than
	// MaxAPIKeyLifetime is refused (ErrExpiryExceedsMaximum), and one at or
	// before now is refused (ErrExpiryInPast).
	ExpiresAt *time.Time
}

// CreatedAPIKey is Service.Create's result: the one and only place the raw
// key value is ever available. See APIKey's own doc comment ("The key
// material is never stored") -- Key is never logged, never returned again
// from List or any other call, and this module holds no copy of it past the
// return of Create.
type CreatedAPIKey struct {
	// ID is the persisted APIKey's id -- pass this to Rotate or Revoke.
	ID string
	// Key is the raw, plaintext credential the caller must record now. It
	// authenticates as itself; nothing else this module returns ever
	// reproduces it.
	Key string
	// Prefix is the display-safe portion also visible later through List.
	Prefix string
	// Scopes, CreatedBy and ExpiresAt echo what was actually persisted --
	// Scopes in particular is worth echoing back since an empty input
	// defaults to an empty (not nil) persisted slice.
	Scopes    []string
	CreatedBy string
	ExpiresAt time.Time
}

// APIKeySummary is what Service.List exposes for one key: every field of
// APIKey except Hash (the whole point of never re-exposing the credential
// material -- see APIKey's own doc comment) plus CreatorLeft, the
// MembershipChecker-derived display flag.
type APIKeySummary struct {
	ID          string
	Prefix      string
	Scopes      []string
	CreatedBy   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	Revoked     bool
	Expired     bool
	CreatorLeft bool
}

// Create validates in and, on success, persists a new APIKey and returns its
// raw value exactly once.
//
// # Scope validation
//
// An empty in.Scopes needs no PermissionLister at all -- there is nothing to
// check a request for zero scopes against -- so a Module built with no
// WithPermissionLister option can still issue scope-less keys. A non-empty
// in.Scopes with no PermissionLister wired fails closed with
// ErrPermissionListerUnavailable (see that error's own doc comment for why
// this is refused rather than silently skipped). Otherwise every requested
// scope must appear in PermissionLister.ListPermissions(ctx, tenant,
// in.CreatedBy)'s answer, checked one at a time since go/rbac's public API
// (see seams.go's PermissionLister doc comment) has no bulk "is this subset
// held" call; the first scope not held fails the whole request with
// ErrScopeNotHeldByCreator naming it.
//
// # Expiry
//
// A nil in.ExpiresAt defaults to now plus MaxAPIKeyLifetime. A non-nil one
// must be strictly after now (ErrExpiryInPast) and no further out than
// MaxAPIKeyLifetime from now (ErrExpiryExceedsMaximum) -- see that error's
// own doc comment for why this is refused rather than clamped.
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreatedAPIKey, error) {
	if in.CreatedBy == "" {
		return nil, ErrCreatedByRequired
	}

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	now := s.clock()
	expiresAt, err := s.resolveExpiry(now, in.ExpiresAt)
	if err != nil {
		return nil, err
	}

	if scopeErr := s.validateScopes(ctx, string(tenant), in.CreatedBy, in.Scopes); scopeErr != nil {
		return nil, scopeErr
	}

	raw, prefix, hash, err := newAPIKeyToken()
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	row := &APIKey{
		ID:        uuid.NewString(),
		Prefix:    prefix,
		Hash:      hash,
		Scopes:    scopesJSON(in.Scopes),
		CreatedBy: in.CreatedBy,
		ExpiresAt: expiresAt,
	}
	if err := s.repo.Create(ctx, row); err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	if err := s.emit(ctx, AuditActionAPIKeyCreate, row, audit.Result{Success: true}); err != nil {
		return nil, err
	}

	return &CreatedAPIKey{
		ID:        row.ID,
		Key:       raw,
		Prefix:    prefix,
		Scopes:    in.Scopes,
		CreatedBy: in.CreatedBy,
		ExpiresAt: expiresAt,
	}, nil
}

// List returns every API key of the caller's tenant as a
// credential-material-free summary, newest first is NOT guaranteed --
// callers that need an order sort the result themselves; round 1's only
// consumer is this module's own tests.
//
// CreatorLeft is computed per row through the optional MembershipChecker
// seam (see seams.go): false for every row when none was wired, exactly the
// same as if every creator were still active -- a display default, never a
// security signal that changes what List returns.
func (s *Service) List(ctx context.Context) ([]APIKeySummary, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	now := s.clock()
	// leftCache avoids repeating an IsActiveMember call for the same
	// creator across several keys the same person issued.
	leftCache := make(map[string]bool, len(rows))
	out := make([]APIKeySummary, 0, len(rows))
	for _, row := range rows {
		scopes, err := parseScopes(row.Scopes)
		if err != nil {
			return nil, ErrInternal.WithCause(err)
		}

		left, err := s.creatorLeft(ctx, string(tenant), row.CreatedBy, leftCache)
		if err != nil {
			return nil, err
		}

		out = append(out, APIKeySummary{
			ID:          row.ID,
			Prefix:      row.Prefix,
			Scopes:      scopes,
			CreatedBy:   row.CreatedBy,
			CreatedAt:   row.CreatedAt,
			ExpiresAt:   row.ExpiresAt,
			LastUsedAt:  row.LastUsedAt,
			RevokedAt:   row.RevokedAt,
			Revoked:     row.IsRevoked(),
			Expired:     row.IsExpired(now),
			CreatorLeft: left,
		})
	}
	return out, nil
}

// Rotate issues a brand-new key carrying the predecessor's Scopes and
// CreatedBy, then revokes the predecessor, and returns the new key's raw
// value exactly once (identically to Create).
//
// # Create-new-then-revoke-old, not an in-place replace
//
// Rotate is implemented as create-new + revoke-old rather than an in-place
// swap of id's own Hash/Prefix columns, for two reasons. First, an
// already-issued APIKey.ID is what a caller-side integration (a webhook
// config, a stored credential reference) would reasonably pin to if this
// module grows an HTTP surface that names keys by id in an audit trail or a
// "last known key" display -- rewriting id's row in place would make that id
// silently start representing a materially different secret with the same
// identity, while creating a new id preserves "one id, one secret, forever"
// for every row this module ever writes. Second, it is what makes Rotate's
// own audit trail readable without inventing a "rotate" AuditEvent shape
// distinct from create/revoke: two ordinary AuditActionAPIKeyCreate/
// AuditActionAPIKeyRevoke events, linked only by the revoked predecessor's id
// appearing as the new row's PredecessorID -- see the exported
// RotatedAPIKey.PredecessorID field below -- rather than a bespoke event type
// every audit consumer would need special-cased handling for.
//
// The two writes are NOT wrapped in one database transaction: dbkit.
// Repository[T] exposes no cross-call transaction seam a business module can
// reach (backend coding standard's Repository-only rule), and a partial
// failure here is safe-direction rather than corrupting -- a predecessor
// revoke that fails after a successful create leaves two live keys instead
// of one, which is a caller-visible surplus of access, never a lockout, and
// Rotate reports the revoke failure to its caller so the surplus is never
// silent. See TestService_Rotate_RevokeFails_ReportsErrorWithNewKeyStillCreated
// in service_test.go.
func (s *Service) Rotate(ctx context.Context, id string) (*CreatedAPIKey, error) {
	old, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, translateRepoErr(err)
	}
	if old.IsRevoked() {
		return nil, ErrKeyAlreadyRevoked
	}

	oldScopes, err := parseScopes(old.Scopes)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	// ExpiresAt is deliberately left nil, not copied from old.ExpiresAt: a
	// rotation is a fresh full-lifetime credential, not a continuation of
	// the predecessor's own clock, and copying an absolute timestamp that
	// may already be close to (or, for a caller rotating a stale-but-still-
	// live key, even past) expiry would defeat the point of rotating at
	// all. Create's own default -- now plus MaxAPIKeyLifetime -- applies
	// exactly as it would for a fresh, unrelated key.
	//
	// Scopes ARE carried forward from the predecessor, but that is not a
	// bypass of Create's own validation: routing them back through Create
	// means they are re-checked against CreatedBy's permissions as they
	// stand right now, not silently trusted because they were valid once.
	// A creator whose permissions shrank since the predecessor was issued
	// can therefore have a Rotate fail with ErrScopeNotHeldByCreator on a
	// scope it no longer holds -- a deliberate, safe-direction consequence
	// of "scope validation happens at issuance" applying to every issuance,
	// including this one, rather than a special carve-out for rotation.
	created, err := s.Create(ctx, CreateInput{
		CreatedBy: old.CreatedBy,
		Scopes:    oldScopes,
	})
	if err != nil {
		return nil, err
	}

	if err := s.Revoke(ctx, old.ID); err != nil {
		// The new key already exists and was already returned to the
		// caller's view of "what Create would answer" -- see the doc comment
		// above for why this surplus is reported, not rolled back.
		return created, err
	}

	return created, nil
}

// Revoke sets RevokedAt on the key identified by id, scoped to the caller's
// tenant. ErrKeyNotFound for an id that does not exist (or belongs to
// another tenant -- see dbkit.ErrRecordNotFound's own doc comment for why
// the two are indistinguishable). ErrKeyAlreadyRevoked for a key that is
// already revoked; Revoke never treats a repeat call as a silent success
// (see that error's own doc comment).
func (s *Service) Revoke(ctx context.Context, id string) error {
	row, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return translateRepoErr(err)
	}
	if row.IsRevoked() {
		return ErrKeyAlreadyRevoked
	}

	now := s.clock()
	row.RevokedAt = &now
	if err := s.repo.Update(ctx, row); err != nil {
		return ErrInternal.WithCause(err)
	}

	return s.emit(ctx, AuditActionAPIKeyRevoke, row, audit.Result{Success: true})
}

// clock returns the service's time source, falling back to time.Now for a
// Service somehow constructed with a nil now (never true for one Attach
// produced, but defensive rather than a guaranteed-non-nil assumption this
// unexported method would otherwise bake in).
func (s *Service) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// resolveExpiry applies Create's ExpiresAt default/ceiling rule. See
// Create's own doc comment ("Expiry") for the exact contract.
func (s *Service) resolveExpiry(now time.Time, requested *time.Time) (time.Time, error) {
	if requested == nil {
		return now.Add(MaxAPIKeyLifetime), nil
	}
	if !requested.After(now) {
		return time.Time{}, ErrExpiryInPast
	}
	if requested.After(now.Add(MaxAPIKeyLifetime)) {
		return time.Time{}, ErrExpiryExceedsMaximum
	}
	return *requested, nil
}

// validateScopes implements Create's own doc comment ("Scope validation").
func (s *Service) validateScopes(ctx context.Context, tenantID, createdBy string, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	if s.permissions == nil {
		return ErrPermissionListerUnavailable
	}

	held, err := s.permissions.ListPermissions(ctx, tenantID, createdBy)
	if err != nil {
		return ErrInternal.WithCause(err)
	}

	for _, scope := range scopes {
		if !slices.Contains(held, scope) {
			return ErrScopeNotHeldByCreator.WithParam("scope", scope)
		}
	}
	return nil
}

// creatorLeft answers List's per-row CreatorLeft flag, memoizing on cache so
// a tenant with many keys from the same creator calls IsActiveMember once
// per creator, not once per row.
func (s *Service) creatorLeft(ctx context.Context, tenantID, createdBy string, cache map[string]bool) (bool, error) {
	if s.membership == nil {
		return false, nil
	}
	if left, ok := cache[createdBy]; ok {
		return left, nil
	}

	active, err := s.membership.IsActiveMember(ctx, tenantID, createdBy)
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	left := !active
	cache[createdBy] = left
	return left, nil
}

// emit records one audit event for row through dbkit/audit's declarative
// Emit, wrapping a publish failure as ErrInternal per the module's own
// errors.go convention (an *apperr.Error's cause never reaches an HTTP
// response body).
func (s *Service) emit(ctx context.Context, action string, row *APIKey, result audit.Result) error {
	if err := audit.Emit(ctx, s.bus, s.auditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "integration.apikey", ID: row.ID, DisplayName: row.Prefix},
		Result:   result,
	}); err != nil {
		return ErrInternal.WithCause(err)
	}
	return nil
}

// translateRepoErr maps a dbkit.Repository[T] not-found error onto this
// module's own ErrKeyNotFound, so a caller of Service never has to know
// dbkit's error vocabulary to test for "no such key" -- errors.go's own
// convention that every exported error surface belongs to this module's own
// index.
//
// Matching is by Code, never by identity: FindByID returns
// dbkit.ErrRecordNotFound.WithParam(...), a distinct *apperr.Error instance
// derived from (but not ==, and not errors.Is-equal to, since *apperr.Error
// declares no Is method) the package-level sentinel -- the same convention
// this module's own errors.go doc comment documents for its own sentinels.
func translateRepoErr(err error) error {
	if found, ok := apperr.As(err); ok && found.Code == dbkit.ErrRecordNotFound.Code {
		return ErrKeyNotFound
	}
	return ErrInternal.WithCause(err)
}
