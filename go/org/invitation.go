package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// tableInvitations is the org_invitations table name, shared by the model's
// TableName and by the migrations' header comments.
const tableInvitations = "org_invitations"

// tableInvitationTokenIndex is the org_invitation_token_index table name,
// shared by invitationTokenIndex's TableName and by the migrations' header
// comments.
const tableInvitationTokenIndex = "org_invitation_token_index"

// EmailSerializerName is the GORM serializer name the Invitation.Email
// column is encrypted under.
//
// A host must register it once during bootstrap, before opening the
// *gorm.DB org's tables live in:
//
//	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)
//
// The name is exported precisely so a host never spells it as a literal.
// The cipher's key MUST be a different secret from the HMAC key given to
// WithEmailIndexer: an AES key and a blind-index key that are the same
// 32 bytes weaken both, which is why dbkit takes them through two separate
// constructors rather than one.
const EmailSerializerName = "org_email_enc"

// EmailIndexColumn is the exact SQL column name of the blind-index column
// Invitation.EmailIndex is indexed under -- the value of the `column:` gorm
// tag on that field. It is exported for the same reason EmailSerializerName
// is: the column name is host wiring, not module code. A host builds the
// blind indexer WithEmailIndexer accepts through
//
//	dbkit.NewBlindIndexer(EmailIndexColumn, key, dbkit.NormalizeEmail)
//
// and dbkit's contract makes that column argument the column's exact SQL
// name: it refuses an EMPTY name, but has no guard for a non-empty wrong one
// (its doc comment says so), so the name must cross the package boundary as
// a referenced constant rather than a hand-typed string that can drift from
// the schema. The module's unit suite pins the constant against the model's
// gorm tag and the migrated schema (email_index_column_drift_test.go).
const EmailIndexColumn = "email_index"

// RegisterEmailSerializer wires cipher into GORM's serializer registry
// under EmailSerializerName, so Invitation.Email is transparently sealed
// on write and opened on read. It is the host-facing half of the
// registration EmailSerializerName's own doc comment documents.
//
// Call it during host bootstrap, BEFORE opening the *gorm.DB org's models
// live in: GORM's registry is process-global and is consulted while a
// model's schema is parsed, so registering afterwards leaves the parsed
// schema pointing at nothing. It is deliberately NOT done inside
// Module.Register -- by then the database is already open, and Register is
// forbidden from doing anything but declare.
//
// A nil cipher is refused rather than registered: a model whose serializer
// silently does nothing would store the address in plaintext, which is the
// one outcome the encrypted column exists to prevent. The cipher's key MUST
// be a different secret from the HMAC key passed to NewEmailIndexer -- an
// AES key and a blind-index key that are the same 32 bytes weaken both.
func RegisterEmailSerializer(cipher *dbkit.Cipher) error {
	if cipher == nil {
		return fmt.Errorf("org: RegisterEmailSerializer requires a cipher for %q", EmailSerializerName)
	}
	dbkit.RegisterEncryptedSerializer(EmailSerializerName, cipher)
	return nil
}

// NewEmailIndexer builds the blind indexer WithEmailIndexer accepts, over
// this module's own EmailIndexColumn and dbkit.NormalizeEmail -- the exact
// column and canonical form the schema and the invitation lookups were
// designed around.
//
// The column argument travels as EmailIndexColumn rather than a host-typed
// string for the reason that constant's doc comment gives: dbkit refuses an
// EMPTY column name but has no guard for a non-empty wrong one, so a host
// that spelled the column itself could register an indexer that only fails
// the day someone calls Equal on it. Handing the constant's ownership to
// this constructor removes that failure mode at the source; dbkit's own
// empty-column refusal still applies to it.
//
// key must be exactly 32 bytes and a secret used for nothing else (see
// dbkit.NewBlindIndexer); an invitation whose address cannot be indexed can
// never be found again, so the key must not change between restarts.
func NewEmailIndexer(key []byte) (*dbkit.BlindIndexer, error) {
	return dbkit.NewBlindIndexer(EmailIndexColumn, key, dbkit.NormalizeEmail)
}

// The lifecycle states an Invitation can be in. Closed set, kept in Go for
// the same reason MembershipStatus* are: PostgreSQL has enum types and
// SQLite does not, so the column is a plain VARCHAR on both engines.
const (
	// InvitationStatusPending is an invitation that has been sent and
	// neither accepted nor withdrawn. It may still expire.
	InvitationStatusPending = "pending"
	// InvitationStatusAccepted is an invitation that produced a membership.
	InvitationStatusAccepted = "accepted"
	// InvitationStatusRevoked is an invitation the tenant withdrew.
	InvitationStatusRevoked = "revoked"
)

// invitationTokenBytes is the entropy of an invitation token. 32 bytes from
// crypto/rand is the same strength dbkit requires of an encryption key, and
// the token is a bearer credential: whoever holds it joins the tenant.
const invitationTokenBytes = 32

// Invitation is a pending offer to join a tenant at a particular node of its
// organization tree.
//
// # Data domain
//
// Tenant data: an invitation belongs
// to exactly one tenant and must never be visible from another. It
// implements dbkit.TenantScoped, is reached only through
// InvitationRepository, and its isolation is proven by
// tenancytest.AssertIsolated.
//
// # The token is never stored
//
// The value that goes into the invitation link is 32 random bytes, handed to
// the caller ONCE by InviteService.Invite and never persisted. What the row
// keeps is TokenHash, the SHA-256 of that value. A leaked database backup
// therefore yields no usable invitation link, and acceptance is a hash
// lookup rather than a comparison against a stored secret.
//
// # The address is encrypted, and still queryable
//
// Email is an email address: PII, and encrypted at rest through the
// serializer named by EmailSerializerName. An encrypted column cannot be
// queried, so EmailIndex carries the HMAC blind index dbkit.NewBlindIndexer computes
// over dbkit.NormalizeEmail's canonical form. Every write goes through
// BlindIndexer.Index and every lookup through BlindIndexer.Equal; org
// reimplements neither.
//
// # Cross-module references
//
// InviterUserID names a row in authn's users table with no foreign key, for
// the same reason Membership.UserID does.
type Invitation struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// NodeID is the node the invitee will be bound to on acceptance.
	NodeID string `gorm:"column:node_id;size:36;not null"`

	// Email is the invitee's address, encrypted at rest. It is never logged,
	// never put in an event payload and never echoed in an error parameter.
	Email string `gorm:"column:email;serializer:org_email_enc;not null"`

	// EmailIndex is the HMAC blind index of Email, the only way this table
	// can be searched by address. It never leaves org -- not in an event
	// payload, not in an HTTP response, not in the audit trail -- and the
	// reason is not that a keyed digest is hard to reverse. HMAC without
	// the key is dictionary-resistant: only a key holder can compute index
	// values for candidate addresses at all, which is the whole point of
	// dbkit's blind-index mechanism (its key is host-injected and lives
	// in-process). The real reason is that the index is a STABLE LINKABLE
	// IDENTIFIER: the same address always yields the same 64 hex
	// characters, on this table, across every tenant and every system that
	// indexes addresses under the same key, and for as long as the key
	// lives. Standing alone, that stability lets a holder of any collection
	// of rows correlate an invitee across them -- across tenants, across
	// time, across systems; combined with any oracle (a reader who invites
	// a guessed address and reads its index back, the self-invite
	// read-back) or a leaked key, the index is equivalent to the address
	// itself, because the input space is a small dictionary of
	// person-attributable strings. An address-derived value this stable
	// must not reach any exit whose audience is wider than the row's own:
	// org.member.invited carries the invitation's id and a subscriber that
	// must reach the invitee reads this row (events.go's MemberInvited doc
	// comment states the same rule from the payload's side), the HTTP
	// responses never echo it (handler.go's toInvitationResponse), and the
	// audit trail -- the most permanent exit of all, append-only with no
	// delete by design, and tenant-readable where the org rows themselves
	// are subtree-readable -- receives it only as the audit:"redact"
	// marker the tag below declares, never the value.
	EmailIndex string `gorm:"column:email_index;size:64;not null;index" audit:"redact"`

	// InviterUserID is the member who issued the invitation.
	InviterUserID string `gorm:"column:inviter_user_id;size:64;not null"`

	// Locale is the language the invitation message was rendered in, chosen
	// when the invitation was created: the OrgCreateInvitationRequest.locale
	// the inviting operator named on the invitee's behalf, or the platform
	// default when they named none. It is deliberately never the operator's
	// own Accept-Language header -- the invitee has made no request of their
	// own yet for the server to read a preference from, and the message must
	// render in the RECIPIENT's language, never the operator's UI language.
	// It is captured on the row rather than looked up at send time because
	// the recipient may not be a user at all and so has no profile to read a
	// preference from either.
	Locale string `gorm:"column:locale;size:16;not null"`

	// TokenHash is the hex-encoded SHA-256 of the invitation token. The
	// token itself is never stored.
	TokenHash string `gorm:"column:token_hash;size:64;not null"`

	// Status is one of the InvitationStatus* constants.
	Status string `gorm:"column:status;size:16;not null"`

	// ExpiresAt is when the invitation stops being acceptable. It is checked
	// at acceptance time against the service clock, so no sweeper is needed
	// for correctness.
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// AcceptedAt is when the invitation was accepted, nil while it was not.
	AcceptedAt *time.Time `gorm:"column:accepted_at"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the org_invitations table.
func (Invitation) TableName() string { return tableInvitations }

// IsPending reports whether the invitation is still open at the given
// moment: neither accepted nor revoked, and not yet expired.
func (i Invitation) IsPending(now time.Time) bool {
	return i.Status == InvitationStatusPending && now.Before(i.ExpiresAt)
}

// AuditResourceType implements dbkit.Auditable: it names org's audit
// resource kind "org.invitation", the label dbkit's automatic GORM
// write-capture plugin attaches to every Invitation write's
// WriteCapturedEvent -- from which go/dbkit/audit's persister derives the
// declared "org.invitation.create" and "org.invitation.update" actions
// (module.go's audit-action block, composed from the label above). An
// invite records as "org.invitation.create"; an acceptance and a revoke
// are compare-and-swap status flips and record as
// "org.invitation.update". What the trail records of the invitee is
// deliberate and stated on the fields themselves: the Email column is a
// GORM serializer field, so capture redacts it to "[redacted]"
// automatically, and EmailIndex carries the audit:"redact" capture
// opt-out and is captured the same way -- the audit row is the most
// permanent exit a stable linkable identifier of a guessable address
// could reach (append-only, no delete by design, tenant-readable where
// the org rows themselves are subtree-readable), so the trail gets the
// marker, never the value. TokenHash is recorded, deliberately, and the
// credential context is why it is not same-family: the token is 32 fresh
// random bytes per invitation, so its SHA-256 has no candidate space (no
// dictionary to reverse with, key or no key), no stability to link on
// (every invitation's hash is unique, even for the same address twice),
// and no person-attributable meaning -- and the only entity a recorded
// hash could ever confirm anything to is a holder of the token value
// itself, who already holds the credential the confirmation is about (see
// hashInvitationToken). The deliberately narrow,
// non-tenant-scoped invitationTokenIndex row is NOT Auditable: it is
// bookkeeping for the token, and its writes belong to no tenant -- see
// that type's own doc comment. Captured only when a host wires
// dbkit.Options.AuditBus on org's connection AND lists this model in its
// Options.AuditModels scope (org.AuditableModels() is the module's own
// list).
func (Invitation) AuditResourceType() string { return AuditResourceTypeInvitation }

// compile-time check that Invitation satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Invitation{}

// compile-time check that Invitation satisfies dbkit.Auditable.
var _ dbkit.Auditable = Invitation{}

// invitationTokenIndex is the narrow, deliberately non-tenant-scoped row
// that resolves an invitation token's owning tenant before any tenant is
// known at all -- the mechanism InviteService.Accept uses when the accepting
// caller holds no tenant context (a freshly invited person has no
// membership -- and typically no token -- in the inviting tenant yet).
//
// It mirrors go/sharing's shareTokenIndex: the same shape, the same
// data-domain reasoning, and the same "write alongside the row it indexes,
// read only by the tenantless entry point, never updated" lifecycle
// (that type's doc comment in go/sharing/model.go has the full argument;
// this one records only what is org-specific).
//
// # Data domain
//
// Platform data, NOT tenant data, and deliberately so: a tenantless
// acceptor holds no
// tenant claim for dbkit's tenant-scope GORM plugin to filter by, and that
// plugin fails every tenant-scoped query closed when the context carries
// none (go/dbkit's tenant_scope.go) -- correctly, since it has no way to
// tell "this caller is allowed to look this up with no tenant" apart from an
// ordinary forgotten-tenant bug. The same repository-wide rule that
// governs identity/platform data applies here without
// exception: this table implements no dbkit.TenantScoped, is reached through
// dbkit.Open()'s plain *gorm.DB (never dbkit.Repository[T], whose generic
// constraint requires TenantScoped, which this type must NOT implement), and
// its isolation suite is tenancytest.AssertNotTenantScoped, not
// AssertIsolated -- the identical treatment go/authn's users table,
// go/jobs's jobRecord, go/config's row, go/dbkit/audit's AuditEvent and
// go/sharing's own token index already get for the same reason: something
// that must be resolvable before a tenant is known cannot itself be
// tenant-scoped.
//
// # Deliberately narrow
//
// This is NOT a general cross-tenant query capability, and carries nothing
// that would make it one: two columns, a token hash and the tenant it
// belongs to, nothing else -- no invitation id, no node, no status, no
// expiry. A row here answers exactly one question ("which tenant does this
// hash belong to") and nothing further; every other question about the
// invitation it names -- is it pending, revoked, expired -- is still
// answered exclusively by the ordinary tenant-scoped org_invitations row,
// reached only after this lookup hands back a tenant to attach to ctx.
//
// # Written alongside its invitation, read only by the tenantless Accept
//
// InvitationRepository.createPending inserts this row in the same database
// transaction as its org_invitations row, so an invitation is never left
// reachable by its own tenant (via the tenant-scoped byTokenHash, when the
// accepting caller already holds that tenant) while being permanently
// unreachable by a tenantless caller holding the same token -- an
// inconsistency a two-step, non-transactional write could otherwise leave
// behind indefinitely, since nothing else in this module ever repairs a
// missing index row. It is never updated afterward: Revoke and Accept leave
// it in place, because the tenantless Accept needs it to resolve a tenant
// and reach the ordinary tenant-scoped path even for a token whose
// invitation has since been accepted or revoked -- exactly how that path is
// meant to answer the case (org.invitation_already_accepted /
// org.invitation_revoked, not a dead end before it is ever reached).
type invitationTokenIndex struct {
	// TokenHash is the hex-encoded SHA-256 of the invitation token -- the
	// exact same value Invitation.TokenHash stores, and the primary key
	// here: two tokens hashing to the same value is cryptographically
	// negligible (see hashInvitationToken), so the constraint is cheap
	// insurance, not a meaningfully defended invariant.
	TokenHash string `gorm:"column:token_hash;primaryKey;size:64"`

	// TenantID is the plain, unenforced tenant identifier this row exists
	// to answer -- unenforced in the identical sense every other
	// platform-data table's tenant_id column is (go/jobs's jobRecord,
	// go/config's row, go/dbkit/audit's AuditEvent): a real column, never
	// filtered or populated by dbkit's tenant-scope plugin, because this
	// type implements no TenantScoped.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`
}

// TableName names the org_invitation_token_index table.
func (invitationTokenIndex) TableName() string { return tableInvitationTokenIndex }

// maxEmailLen is the longest address org accepts, the length limit RFC 5321
// puts on a forward path. Bounded in Go for the same reason node names are:
// SQLite does not enforce a column width under type affinity, and the column
// here holds ciphertext anyway, which is longer than the plaintext.
const maxEmailLen = 254

// validateInviteEmail returns the trimmed address, or ErrInvalidEmail.
//
// The check is deliberately syntactic and minimal -- one "@", a non-empty
// local part, a domain with a dot in it, no whitespace, control or
// non-ASCII characters, and a length bound -- rather than an attempt at
// RFC 5322. Anything stricter rejects addresses that genuinely deliver;
// anything looser lets org spend a rate-limit slot and a mail attempt on a
// value that cannot possibly be one.
//
// It exists because the refusal must be org's own coded ErrInvalidEmail and
// must land before the address costs a rate-limit slot or a mail attempt.
// dbkit.NormalizeEmail performs a structural gate of its own when it
// indexes, so this check is not the only guard on the address -- it is the
// earliest one, and the one that decides org's coded answer. The refusal
// set mirrors that gate's exactly, so no address the indexer would refuse
// can slip past here to be refused downstream instead: an invitation's
// EmailIndex column is mandatory, and an address without a canonical form
// is one org cannot store.
//
// The address is never echoed into the error: an error's parameters are
// rendered, logged and traced, and an address is PII.
func validateInviteEmail(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" || len(address) > maxEmailLen {
		return "", ErrInvalidEmail
	}
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" {
		return "", ErrInvalidEmail
	}
	if strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return "", ErrInvalidEmail
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	for _, r := range address {
		// Non-ASCII is refused exactly as dbkit.NormalizeEmail refuses it
		// when it indexes: the canonical form this address is blind-indexed
		// under serves ASCII mailbox names.
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrInvalidEmail
		}
	}
	return address, nil
}

// newInvitationToken returns a fresh invitation token and its stored hash.
//
// The token is base64url-encoded so it survives a URL query string without
// escaping; the hash is hex so the column is a fixed 64 characters on both
// engines. A failure of crypto/rand is fatal to the operation and is
// reported, never worked around with a weaker source.
func newInvitationToken() (token, hash string, err error) {
	raw := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("org: generating an invitation token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashInvitationToken(token), nil
}

// hashInvitationToken returns the hex-encoded SHA-256 of an invitation
// token: what the row stores, and what a lookup is keyed on.
//
// SHA-256 rather than a password hash is deliberate and correct here: the
// input is 32 bytes of full-entropy randomness, not a human-chosen secret,
// so there is no dictionary to slow an attacker down with.
func hashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// InvitationRepository is org's tenant-scoped data-access type for
// Invitation.
//
// Same construction rules as Repository and MembershipRepository: the extra
// query shapes compose on the *gorm.DB the isolation plugin protects,
// against a TenantScoped destination, inside dbkit.WithTenantSession. No
// hand-written tenant predicate, no db.Table / db.Model / db.Raw.
type InvitationRepository struct {
	*dbkit.Repository[Invitation]

	db *gorm.DB
}

// NewInvitationRepository returns an InvitationRepository backed by db.
func NewInvitationRepository(db *gorm.DB) *InvitationRepository {
	return &InvitationRepository{Repository: dbkit.NewRepository[Invitation](db), db: db}
}

// byTokenHash returns the invitation of the caller's tenant whose stored
// hash is hash, or ErrInvitationNotFound.
//
// The tenant scoping is the security property that matters here: a token
// minted for another tenant simply does not match, so nothing about it can
// be learned. The tenant is never taken from the token itself -- it comes
// either from the request context (a caller tenancy.Middleware already
// resolved) or, for a tenantless acceptor, from the deliberately narrow
// invitationTokenIndex row tenantForTokenHash resolves first and attaches
// with pkgcore.WithTenant. See InviteService.Accept for why that ordering is
// not negotiable.
func (r *InvitationRepository) byTokenHash(ctx context.Context, hash string) (*Invitation, error) {
	var inv Invitation
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("token_hash = ?", hash).First(&inv).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrInvitationNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &inv, nil
}

// acceptIfPending atomically transitions the invitation identified by id,
// scoped to the caller's tenant, from InvitationStatusPending to
// InvitationStatusAccepted, stamping AcceptedAt, and reports whether this
// call is the one that performed the transition.
//
// The "status = ?" condition on the UPDATE below is a compare-and-swap
// guard that dbkit.Repository[T].Update cannot express -- it writes
// unconditionally once id and tenant match. Without this guard, two
// concurrent InviteService.Accept calls racing on the same token could both
// read InvitationStatusPending before either commits, and both would go on
// to create a membership: exactly the single-use violation this method
// exists to close. Gating the write itself on the row still being pending
// means at most one caller ever observes won == true; a caller that
// observes false lost the race (or the invitation was revoked out from
// under it) and MUST NOT create a membership.
func (r *InvitationRepository) acceptIfPending(ctx context.Context, id string, acceptedAt time.Time) (won bool, err error) {
	var rowsAffected int64
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("status = ?", InvitationStatusPending).
			Updates(&Invitation{Status: InvitationStatusAccepted, AcceptedAt: &acceptedAt})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if dbErr != nil {
		return false, ErrInternal.WithCause(dbErr)
	}
	return rowsAffected == 1, nil
}

// revokeIfPending atomically transitions the invitation identified by id,
// scoped to the caller's tenant, from InvitationStatusPending to
// InvitationStatusRevoked, and reports whether this call is the one that
// performed the transition -- the status-revoked twin of acceptIfPending
// above, and the answer to the same problem on the other side of the token:
// a plain,
// unconditional Update after a status read would overwrite an Accept that
// won its own compare-and-swap between that read and that write --
// landing the invitation terminal-revoked while the membership the accept
// created stayed live. Gating the write on the row still being pending
// means the revoke can never overwrite a state another caller already
// committed; the caller re-reads and classifies when won == false, exactly
// as Accept's own caller does after a lost acceptIfPending.
func (r *InvitationRepository) revokeIfPending(ctx context.Context, id string) (won bool, err error) {
	var rowsAffected int64
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("status = ?", InvitationStatusPending).
			Select("Status").
			Updates(&Invitation{Status: InvitationStatusRevoked})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if dbErr != nil {
		return false, ErrInternal.WithCause(dbErr)
	}
	return rowsAffected == 1, nil
}

// settleClaim transitions the invitation identified by id, scoped to the
// caller's tenant, OUT of the accepted state a claim currently holds and
// INTO status, clearing AcceptedAt, and reports whether this call performed
// the transition.
//
// It is the claim-holder's counterpart to acceptIfPending and revokeIfPending
// above: InviteService.Accept wins pending -> accepted on this row, and if
// it then fails to produce the membership the claim was for it settles the
// claim (InviteService.settleFailedAccept) -- back to pending when the
// failure was transient, to revoked when the invitation can never be
// fulfilled. Settling is a write over a state THIS CALL won, so it is gated
// on the row still holding that claim (status = 'accepted'), exactly as the
// other two transitions are gated on pending. It is deliberately never the
// unconditional, full-row Update Accept's old failure path issued, which
// wrote whatever the caller's stale in-memory snapshot held over whatever
// state the row had moved to; when the guard matches nothing, the accepted
// state this call won is already gone (a concurrent writer moved the row
// first) and the caller leaves that writer's outcome alone.
//
// The write selects Status and AcceptedAt only: the invitee's address, the
// expiry and the rest of the row are not this transition's to touch, the
// same column discipline revokeIfPending applies to its own single-column
// transition.
func (r *InvitationRepository) settleClaim(ctx context.Context, id, status string) (won bool, err error) {
	var rowsAffected int64
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("status = ?", InvitationStatusAccepted).
			Select("Status", "AcceptedAt").
			Updates(&Invitation{Status: status, AcceptedAt: nil})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if dbErr != nil {
		return false, ErrInternal.WithCause(dbErr)
	}
	return rowsAffected == 1, nil
}

// createPending revokes every pending invitation already outstanding for
// invitation's own address, then inserts invitation and its
// invitationTokenIndex row, all inside ONE dbkit.WithTenantSession
// transaction -- the atomic replacement for a separate revoke-then-Create
// pair of transactions.
//
// # The race this closes
//
// If the revoke ran as a read (pendingByEmail) followed by a per-row Update
// loop in one transaction, and the insert followed in a wholly separate
// one, two concurrent Invite calls for
// the SAME address could each run their own read-then-revoke-loop before
// either had inserted anything, see no pending invitation to revoke (there
// is genuinely none yet), and both go on to Create: two simultaneously
// live tokens for one address, violating this module's own "at most one
// live token at a time" claim (invite.go's Invite doc comment) --
// acceptIfPending's per-id compare-and-swap does not help here, since the
// two tokens are two DIFFERENT rows, and two different accepters could each
// win one.
//
// # The single transaction
//
// Three writes, in order, no read of anything in between -- so this
// transaction's first statement is a write, the same reasoning
// lockLiveNode's own doc comment in repository.go gives for why that keeps
// SQLite out of the read-then-write lock-upgrade hazard:
//
//  1. A blind BULK revoke: every row of the caller's tenant whose blind
//     index matches email's and whose status is still Pending is flipped
//     to Revoked in one UPDATE, with no prior SELECT enumerating which
//     rows those are. This alone does not yet close the race (a second,
//     concurrent Invite's own revoke could run either before or after
//     this one commits, and if it runs after, it revokes nothing new --
//     there being nothing pending left for it to find).
//  2. The insert. This is where the race is actually arbitrated: the
//     partial unique index on (tenant_id, email_index) WHERE status =
//     'pending' (migrations/{postgres,sqlite}/0005_unique_pending_invitation.sql)
//     admits at most one row per address in this state, so of two
//     concurrent Invite transactions racing to reach this step, the
//     database lets exactly one INSERT succeed; the other's INSERT fails
//     with gorm.ErrDuplicatedKey, translated by the caller
//     (InviteService.Invite) into the coded ErrInvitationAlreadyPending.
//  3. The invitationTokenIndex row: the same transaction also writes the
//     narrow token_hash -> tenant_id index row that lets a TENANTLESS
//     acceptor resolve this invitation's tenant (see that type's own doc
//     comment). The two writes share the tenant-scope GORM plugin's
//     session exactly like go/sharing's createWithTokenIndex: the plugin
//     forces invitation's tenant_id the way every tenant-scoped Create
//     relies on, and does not touch invitationTokenIndex at all, since
//     that type implements no dbkit.TenantScoped.
//
// createPending itself does not translate any error -- it returns whatever
// the transaction returns, unwrapped, so its caller can compose it with
// whatever else it needs to report.
func (r *InvitationRepository) createPending(ctx context.Context, indexer *dbkit.BlindIndexer, email string, invitation *Invitation) error {
	cond, err := indexer.Equal(email)
	if err != nil {
		return ErrInvalidEmail.WithCause(err)
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.
			Where(cond).
			Where("status = ?", InvitationStatusPending).
			Select("Status").
			Updates(&Invitation{Status: InvitationStatusRevoked}).Error; err != nil {
			return err
		}
		if err := tx.Create(invitation).Error; err != nil {
			return err
		}
		idx := &invitationTokenIndex{TokenHash: invitation.TokenHash, TenantID: string(tenant)}
		return tx.Create(idx).Error
	})
}

// tenantForTokenHash resolves the tenant an invitation token hash belongs
// to, with NO tenant predicate anywhere in the query -- the one deliberately
// narrow exception to this module's "every query is tenant-scoped" rule, and
// the mechanism invitationTokenIndex's own doc comment justifies in full: an
// accepting invitee holds no tenant claim (a freshly invited person has no
// membership in the inviting tenant yet, which is the whole point), so
// nothing about their request can scope this lookup by tenant before it runs
// -- that is precisely the property this method exists to establish, not
// violate.
//
// This is not a second byTokenHash and not a general cross-tenant query
// capability: it reads invitationTokenIndex, a table that was never
// tenant-scoped to begin with (see that type's own doc comment for the full
// data-domain reasoning), through an ordinary, unfiltered query -- dbkit's
// tenant-scope GORM plugin never engages here at all, because the plugin
// only ever acts on a model implementing dbkit.TenantScoped, and
// invitationTokenIndex deliberately does not. Nothing here reaches for raw
// SQL, pkgcore.WithSystemContext, or any other escape hatch around a
// tenant-scoped query -- there is no tenant-scoped query to escape, because
// this method touches a different, narrower table than byTokenHash does. It
// returns a tenant id and nothing else: no invitation id, no node, no
// status, so a caller cannot use this method to learn anything about an
// invitation beyond which tenant a token's hash belongs to.
//
// InviteService.Accept is this method's only caller: a tenantless Accept
// resolves the tenant here, attaches it to ctx with pkgcore.WithTenant, and
// then runs the ordinary tenant-scoped flow unchanged -- byTokenHash's own
// lookup, the status checks, the single-use compare-and-swap and the
// membership creation all still happen exactly where they always have. An
// unrecognized hash here returns ErrInvitationNotFound, the same sentinel
// byTokenHash returns for an unrecognized hash under a known tenant, so a
// caller cannot distinguish "no such token anywhere" from "no such token in
// the tenant it otherwise resolved to".
func (r *InvitationRepository) tenantForTokenHash(ctx context.Context, hash string) (pkgcore.TenantID, error) {
	var idx invitationTokenIndex
	err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&idx).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", ErrInvitationNotFound
	case err != nil:
		return "", ErrInternal.WithCause(err)
	}
	return pkgcore.TenantID(idx.TenantID), nil
}

// byStatus returns the caller tenant's invitations in the given status,
// newest first and then by id so the order is total and stable.
func (r *InvitationRepository) byStatus(ctx context.Context, status string) ([]Invitation, error) {
	var out []Invitation
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("status = ?", status).
			Order("created_at DESC, id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// pendingByEmail returns the caller tenant's pending invitations for one
// address, looked up through the blind index.
//
// The address never appears in the SQL: indexer.Equal normalizes it exactly
// as the write path did and yields a condition on the index column alone.
// Passing the raw address to Equal rather than a precomputed digest is
// dbkit's own contract -- it is what makes a lookup under a different
// normalization impossible.
func (r *InvitationRepository) pendingByEmail(ctx context.Context, indexer *dbkit.BlindIndexer, email string) ([]Invitation, error) {
	cond, err := indexer.Equal(email)
	if err != nil {
		return nil, ErrInvalidEmail.WithCause(err)
	}
	var out []Invitation
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where(cond).
			Where("status = ?", InvitationStatusPending).
			Order("created_at DESC, id").
			Find(&out).Error
	})
	if dbErr != nil {
		return nil, ErrInternal.WithCause(dbErr)
	}
	return out, nil
}
