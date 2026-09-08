package integration

import (
	"context"
	"time"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// AuthenticatedAPIKey is Service.Authenticate's success result: the resolved
// tenant plus the minimal identity/scope context a caller needs to decide
// what happens next, deliberately NEVER carrying the row's Hash or any other
// storage-only field.
type AuthenticatedAPIKey struct {
	// KeyID is the authenticated key's own id -- what a caller keys a
	// per-key rate-limit counter on (LayeredLimiter's key layer, the
	// design's three-layer composition made concrete against a real
	// Authenticate-gated surface) and what an audit trail cites.
	KeyID string

	// TenantID is the tenant Authenticate resolved the presented key to --
	// see that method's own doc comment for how. A caller attaching it to a
	// request's context (pkgcore.WithTenant) is exactly what Middleware
	// (middleware.go) does.
	TenantID string

	// CreatedBy is the id of the user who originally created (or, after a
	// rotation, most recently re-created) this key -- the responsible party
	// of record, not a live "who is calling now" identity. See APIKey's own
	// "creator leaving does not revoke the key" doc comment: CreatedBy stays
	// exactly what it was even if that user has since left the tenant.
	CreatedBy string

	// Scopes is the frozen-at-issuance permission subset this key may act
	// within -- see APIKey.Scopes' own doc comment. A caller authorizing
	// what the request may do tests set membership against THIS, never
	// against CreatedBy's own current permissions (which may have changed,
	// or vanished, since issuance).
	Scopes []string
}

// Authenticate resolves rawKey to the APIKey that issued it, or refuses with
// ErrAuthenticationFailed -- an outward-identical refusal across every
// possible cause (no such hash anywhere, a revoked key, an expired key), per
// the repository-wide no-enumeration Security rule and the identical
// discipline go/authn's ErrInvalidCredentials and go/sharing's
// ErrNotAccessible apply to their own refusal answers: a caller presenting
// a wrong key must learn nothing about whether the key ever existed, was
// rotated away, or expired.
//
// # Tenant resolution
//
// Unlike every other Service method (Create, List, Rotate, Revoke), which
// all require a tenant already in ctx (pkgcore.MustTenantFromContext),
// Authenticate takes NO tenant from ctx at all and requires none: a
// genuinely inbound request authenticated by nothing but this key carries no
// separate tenant claim, exactly go/sharing's AccessPublic's own situation
// for an anonymous share-link visitor. Authenticate resolves the tenant
// itself, from the hash alone, through apiKeyHashIndex
// (APIKeyRepository.tenantForHash, model.go's own doc comment has the full
// "why a new table" argument), then continues with the ordinary
// tenant-scoped read (APIKeyRepository.byHash) under that resolved tenant --
// the identical two-step shape (*sharing.Service).AccessPublic uses
// (tenantForTokenHash then re-entering the tenant-scoped Access), applied
// here to a bearer API key instead of a bearer share token.
//
// This two-step shape departs from the migration comment on
// uq_integration_api_keys_tenant_hash
// (migrations/{sqlite,postgres}/0001_create_integration_api_keys.sql),
// which assumed an authentication lookup would already know its tenant
// from request context. That assumption does not hold for a real inbound
// API-key surface -- see model.go's apiKeyHashIndex doc comment for the
// full argument -- so Authenticate resolves the tenant from the hash
// instead of requiring a caller to already have one.
//
// # What is NOT re-derived
//
// Scopes is returned exactly as frozen at issuance (or at the most recent
// Rotate); Authenticate never re-validates it against CreatedBy's current
// permissions the way Create/Rotate do at issuance time -- an authenticated
// request's authorization decision is the caller's own business, using the
// Scopes this method hands back, not something Authenticate itself decides.
//
// # LastUsedAt
//
// A successful Authenticate best-effort records now as the row's
// LastUsedAt -- the write APIKey.LastUsedAt's own field comment describes
// as otherwise never happening, since no other code path authenticates a
// request with a key. This write is display bookkeeping, never a security
// control -- a failure to record it is logged and swallowed, never
// surfaced as an authentication failure: an operator missing a "last used"
// timestamp on one request is a materially smaller problem than refusing an
// otherwise-valid, otherwise-live key over a bookkeeping write that failed.
func (s *Service) Authenticate(ctx context.Context, rawKey string) (*AuthenticatedAPIKey, error) {
	if rawKey == "" {
		return nil, ErrAuthenticationFailed
	}
	hash := hashAPIKeyToken(rawKey)

	tenant, err := s.repo.tenantForHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	tenantCtx := pkgcore.WithTenant(ctx, tenant)
	row, err := s.repo.byHash(tenantCtx, hash)
	if err != nil {
		return nil, err
	}

	now := s.clock()
	if row.IsRevoked() || row.IsExpired(now) {
		return nil, ErrAuthenticationFailed
	}

	scopes, err := parseScopes(row.Scopes)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	if updateErr := s.recordLastUsed(tenantCtx, row, now); updateErr != nil {
		obs.FromContext(ctx).Warn("integration failed to record an API key's last-used timestamp",
			"key_id", row.ID, "error", updateErr)
	}

	return &AuthenticatedAPIKey{
		KeyID:     row.ID,
		TenantID:  string(tenant),
		CreatedBy: row.CreatedBy,
		Scopes:    scopes,
	}, nil
}

// recordLastUsed persists row's LastUsedAt as now through a TARGETED
// single-column update (APIKeyRepository.touchLastUsed) -- ctx must already
// carry row's own tenant (Authenticate's tenantCtx). Failure is reported to
// the caller, which treats it as non-fatal to the surrounding Authenticate
// call; see that method's own "LastUsedAt" doc comment section for why.
//
// The targeted shape is load-bearing, not a style preference: this write
// races a concurrent Service.Revoke, and a full-row Update of an
// already-read (and therefore possibly stale) row would carry that stale
// copy's nil RevokedAt back into the database, silently undoing the
// revocation -- reviving a key the tenant already revoked. A write that
// touches nothing but last_used_at cannot revive anything, whichever way
// the race resolves.
func (s *Service) recordLastUsed(ctx context.Context, row *APIKey, now time.Time) error {
	return s.repo.touchLastUsed(ctx, row.ID, now)
}
