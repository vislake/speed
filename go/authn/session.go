package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"

	obs "github.com/vislake/speed/go/observability"
)

const (
	// DefaultRefreshTokenTTL is how long a refresh token stays usable.
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour

	// DefaultSessionTTL bounds the session itself, independently of the
	// refresh tokens inside it. Without it a session that keeps refreshing
	// never ends, and "sign in again occasionally" stops being enforceable
	// at all.
	DefaultSessionTTL = 90 * 24 * time.Hour

	// refreshSecretBytes is the entropy of the opaque refresh token: 256
	// bits. It is a value the server generates, so it can be full entropy,
	// which is exactly why its stored digest is a plain SHA-256 rather
	// than a password KDF -- there is no guessing attack for a slow hash
	// to slow down.
	refreshSecretBytes = 32

	// revokedSessionKeyPrefix namespaces the immediate-revocation list
	// inside the shared KVStore.
	revokedSessionKeyPrefix = "authn:revoked_session:"
)

// RevocationMode selects what "sign this device out" costs and how fast it
// takes effect. It answers the conflict a stateless token creates: a JWT
// cannot be withdrawn once signed, yet signing a device out is exactly a
// withdrawal.
//
// It is a configuration value, never a deployment-mode branch. Both modes
// work identically in the standalone and distributed deployment modes,
// because the revocation list is a pkgcore.KVStore and both deployment modes
// have one.
type RevocationMode string

const (
	// RevocationModeNatural revokes the session and its refresh tokens and
	// lets outstanding access tokens expire on their own. Cost: zero extra
	// work per request. Consequence: a signed-out device keeps working for
	// at most one access-token lifetime.
	RevocationModeNatural RevocationMode = "natural"

	// RevocationModeImmediate additionally records the revoked session in
	// the KVStore, which Middleware consults on every authenticated
	// request. Cost: one key-value read per request. Consequence: sign-out
	// takes effect at once, which regulated deployments require.
	//
	// The consultation is wired by default, not by host ceremony: a
	// Service attaches this manager to the Verifier it hands out, and
	// Middleware consults that source unless the host passed an explicit
	// WithRevocationChecker -- so a host that selects this mode through
	// WithRevocationMode gets its enforcement without a second, forgettable
	// wiring step.
	//
	// The list only ever holds sessions that are revoked AND not yet
	// naturally expired, with a TTL of one access-token lifetime, so it
	// stays proportional to the revocation rate rather than to the number
	// of sessions ever created.
	RevocationModeImmediate RevocationMode = "immediate"
)

// StartSessionInput describes the session a successful authentication
// creates.
type StartSessionInput struct {
	// UserID is the authenticated user.
	UserID string
	// TenantID is the tenant the session's first access token is issued
	// for. The caller has already verified membership in it.
	TenantID pkgcore.TenantID
	// AMR lists how the user authenticated.
	AMR []string
	// Device, UserAgent and IP describe the client, for the owner's own
	// device list.
	Device    string
	UserAgent string
	IP        string
}

// IssuedRefreshToken is a newly minted refresh credential: the opaque secret
// to hand the client, and the row that records its digest.
//
// Secret is present exactly once, here, and is never stored or logged. The
// row holds only its SHA-256 digest, so a dump of the database does not yield
// a usable refresh token.
type IssuedRefreshToken struct {
	// Secret is the opaque token the client presents. It exists in this
	// struct and in the response body, and nowhere else.
	Secret string
	// Record is the stored row.
	Record *RefreshToken
}

// SessionManager owns session lifetime and the refresh-token family: starting
// a session, rotating its tokens, detecting a replayed token, and revoking
// the lot.
type SessionManager struct {
	sessions   *SessionRepository
	tokens     *RefreshTokenRepository
	kv         pkgcore.KVStore
	bus        pkgcore.EventBus
	mode       RevocationMode
	now        func() time.Time
	refreshTTL time.Duration
	sessionTTL time.Duration
	accessTTL  time.Duration

	// auditActions is the registrar the replay-response audit record
	// (emitReplayAudit) validates its action string against. It is nil
	// until module.go's Register wires it from the host's
	// pkgcore.ComponentRegistry, right after reg.AuditActions.Add has declared the
	// actions -- the same post-Add wiring SSOService's own service-layer
	// emit receives -- so a manager assembled directly through
	// NewSessionManager (every unit test in this package) records no audit
	// rows, exactly like the handler's nil-bus short-circuit.
	auditActions pkgcore.AuditActionRegistrar
}

// NewSessionManager assembles a SessionManager. kv and bus are the pkgcore
// seams, so the same code runs under both deployment modes; nothing here ever
// asks which one it is in.
func NewSessionManager(
	sessions *SessionRepository,
	tokens *RefreshTokenRepository,
	kv pkgcore.KVStore,
	bus pkgcore.EventBus,
	mode RevocationMode,
	now func() time.Time,
	refreshTTL, sessionTTL, accessTTL time.Duration,
) (*SessionManager, error) {
	switch {
	case sessions == nil || tokens == nil:
		return nil, errors.New("authn: NewSessionManager requires the session and refresh-token repositories")
	case kv == nil:
		return nil, errors.New("authn: NewSessionManager requires a key-value store")
	case bus == nil:
		return nil, errors.New("authn: NewSessionManager requires an event bus")
	case now == nil:
		return nil, errors.New("authn: NewSessionManager requires a clock")
	}
	if mode != RevocationModeImmediate {
		mode = RevocationModeNatural
	}
	return &SessionManager{
		sessions:   sessions,
		tokens:     tokens,
		kv:         kv,
		bus:        bus,
		mode:       mode,
		now:        now,
		refreshTTL: refreshTTL,
		sessionTTL: sessionTTL,
		accessTTL:  accessTTL,
	}, nil
}

// Start creates a session and the first refresh token of its family.
//
// Both inserts run under withInsertConflictRetry: each mints its row's
// primary key (newID) before the insert, so a conflict that a retry turns
// into a duplicate-key refusal is provably this call's own earlier
// attempt, not another writer's row (see that helper's own doc comment for
// the argument). Without the retry a single lost race on either insert --
// under SQLite a busy window the file's holder outlasts -- surfaces as an
// internal error on an otherwise valid sign-in.
func (m *SessionManager) Start(ctx context.Context, in StartSessionInput) (*Session, IssuedRefreshToken, error) {
	if in.UserID == "" {
		return nil, IssuedRefreshToken{}, errors.New("authn: cannot start a session without a user id")
	}

	now := m.now()
	session := &Session{
		ID:              newID(),
		UserID:          in.UserID,
		Status:          SessionStatusActive,
		CurrentTenantID: string(in.TenantID),
		Device:          in.Device,
		UserAgent:       in.UserAgent,
		IP:              in.IP,
		CreatedAt:       now,
		LastSeenAt:      now,
		ExpiresAt:       now.Add(m.sessionTTL),
	}
	session.SetAMR(in.AMR)

	if err := withInsertConflictRetry(
		func() error { return m.sessions.Create(ctx, session) },
		func() (bool, error) { return m.sessionRowExists(ctx, session.ID) },
	); err != nil {
		return nil, IssuedRefreshToken{}, err
	}

	// A fresh family id per login: the family is the unit replay detection
	// revokes, so it must not span two logins of the same user.
	issued, err := m.issueRefreshToken(ctx, session, newID(), "")
	if err != nil {
		return nil, IssuedRefreshToken{}, err
	}
	return session, issued, nil
}

// sessionRowExists reports whether a session row carrying id is present
// now. It is withInsertConflictRetry's residue check for Start's insert: a
// duplicate-key refusal raised by a retry means the earlier attempt's write
// landed, and this read confirms it before the retry reports completion.
func (m *SessionManager) sessionRowExists(ctx context.Context, id string) (bool, error) {
	if _, err := m.sessions.FindByID(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// issueRefreshToken mints one token in familyID, recording rotatedFrom as the
// token it replaces (empty for the first of a family).
func (m *SessionManager) issueRefreshToken(ctx context.Context, session *Session, familyID, rotatedFrom string) (IssuedRefreshToken, error) {
	secret, digest, err := newRefreshSecret()
	if err != nil {
		return IssuedRefreshToken{}, err
	}

	now := m.now()
	record := &RefreshToken{
		ID:          newID(),
		SessionID:   session.ID,
		UserID:      session.UserID,
		FamilyID:    familyID,
		RotatedFrom: rotatedFrom,
		TokenHash:   digest,
		Status:      RefreshTokenStatusActive,
		CreatedAt:   now,
		ExpiresAt:   now.Add(m.refreshTTL),
	}
	if err := withInsertConflictRetry(
		func() error { return m.tokens.Create(ctx, record) },
		func() (bool, error) { return m.refreshTokenRowExists(ctx, record.ID, digest) },
	); err != nil {
		return IssuedRefreshToken{}, err
	}
	return IssuedRefreshToken{Secret: secret, Record: record}, nil
}

// refreshTokenRowExists reports whether the refresh-token row named by id
// is present now, and that the row found by digest is that very row. It is
// withInsertConflictRetry's residue check for issueRefreshToken's insert;
// it looks the row up by digest rather than by id because the repository
// deliberately reads tokens only through their digest (the plaintext never
// needs to be recoverable, and the digest is the one column the insert and
// every later lookup share).
func (m *SessionManager) refreshTokenRowExists(ctx context.Context, id, digest string) (bool, error) {
	found, err := m.tokens.FindByHash(ctx, digest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return found.ID == id, nil
}

// Rotate consumes the presented refresh token and issues its replacement,
// returning the session the token belongs to. It is the composition of
// resolveRotation followed by commitRotation below: the two internal steps
// stay one atomic-looking call from a caller's perspective, split so
// Service.refresh can run its own re-verification between them.
//
// It is where replay detection lives. Every refresh invalidates the token it
// was given and mints a new one in the same family, so a token is usable
// exactly once. Presenting one that has already been consumed therefore means
// a second copy of it exists -- the credential leaked -- and the response is
// not merely to refuse this request but to revoke the WHOLE family and its
// session, because the thief's freshly rotated token is in that family too.
// Refusing only the replayed token would leave whoever stole it signed in.
//
// One consequence must be stated rather than discovered: two concurrent
// refreshes with the same token are indistinguishable from a replay, and are
// treated as one. That is the intended trade -- a client racing itself loses
// its session, an attacker racing the victim loses theirs -- and it is why a
// client must serialise its own refreshes.
func (m *SessionManager) Rotate(ctx context.Context, presented string) (*Session, IssuedRefreshToken, error) {
	record, session, err := m.resolveRotation(ctx, presented)
	if err != nil {
		return nil, IssuedRefreshToken{}, err
	}
	return m.commitRotation(ctx, record, session)
}

// resolveRotation is Rotate's read-only half: it locates the presented
// token, treats an already-consumed one as a replay (revoking the family
// and session, exactly as Rotate does for that case), and loads the
// session the token belongs to -- all without spending anything.
//
// It exists as its own step so a caller with its own business-rule
// re-verification to run in between -- Service.refresh's membership and
// user-status re-check chief among them -- can run that re-verification
// against the token's session BEFORE the token is actually consumed. A
// re-verification failure must not leave the presented token permanently
// spent with nothing to show the caller for it: the client's own,
// entirely legitimate retry with that same token would otherwise look
// identical to an actual replay and pay the same price -- the whole
// family and session revoked, a "suspected theft" event fired -- over
// what is really a transient membership-lookup failure or a passing
// status flap. See Service.refresh's doc comment for the full account.
func (m *SessionManager) resolveRotation(ctx context.Context, presented string) (*RefreshToken, *Session, error) {
	if presented == "" {
		return nil, nil, ErrRefreshTokenInvalid
	}

	record, err := m.tokens.FindByHash(ctx, hashRefreshSecret(presented))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, ErrRefreshTokenInvalid
		}
		return nil, nil, err
	}

	if record.Status == RefreshTokenStatusRotated {
		return nil, nil, m.handleReplay(ctx, record)
	}
	if record.Status != RefreshTokenStatusActive {
		return nil, nil, ErrRefreshTokenInvalid
	}
	if !m.now().Before(record.ExpiresAt) {
		return nil, nil, ErrRefreshTokenInvalid
	}

	session, err := m.sessions.FindByID(ctx, record.SessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, ErrRefreshTokenInvalid
		}
		return nil, nil, err
	}
	if session.Status != SessionStatusActive || !m.now().Before(session.ExpiresAt) {
		return nil, nil, ErrSessionRevoked
	}

	return record, session, nil
}

// commitRotation is Rotate's mutating half: the atomic single-use
// consumption of record plus minting its replacement in the same family.
// A caller reordering its own checks around resolveRotation must have
// already run every one of them against session by the time it calls
// this -- once commitRotation returns, the presented token is spent for
// good, replay detection included, and there is no undoing that.
func (m *SessionManager) commitRotation(ctx context.Context, record *RefreshToken, session *Session) (*Session, IssuedRefreshToken, error) {
	won, err := m.tokens.Consume(ctx, record.ID, m.now())
	if err != nil {
		return nil, IssuedRefreshToken{}, err
	}
	if !won {
		// Someone else consumed this exact token between
		// resolveRotation's read and this update. From here the two
		// cases -- a client racing itself and a thief racing the
		// victim -- are the same observation, and the safe reading of
		// that observation is the second one.
		return nil, IssuedRefreshToken{}, m.handleReplay(ctx, record)
	}

	issued, err := m.issueRefreshToken(ctx, session, record.FamilyID, record.ID)
	if err != nil {
		return nil, IssuedRefreshToken{}, err
	}

	if err := m.sessions.TouchLastSeen(ctx, session.ID, m.now()); err != nil {
		// The refresh itself succeeded; failing it because a
		// last-seen timestamp did not update would turn a cosmetic
		// problem into a sign-out.
		obs.FromContext(ctx).Warn("session last-seen update failed", "session_id", session.ID, "error", err)
	}
	return session, issued, nil
}

// handleReplay revokes the whole family and its session, records the
// response in the audit trail (see emitReplayAudit), publishes the security
// event, and returns the error the caller must surface.
func (m *SessionManager) handleReplay(ctx context.Context, record *RefreshToken) error {
	now := m.now()
	if _, err := m.tokens.RevokeFamily(ctx, record.FamilyID, now); err != nil {
		return err
	}

	// The replayed session is loaded once and shared with the revoke path
	// below, so its own tenant can ride on the replay event and the audit
	// record without a second lookup. A session row that is already gone --
	// Revoke tolerates that, and the replay event must still fire -- leaves
	// the event's tenant empty rather than failing the detection.
	session, err := m.sessions.FindByID(ctx, record.SessionID)
	switch {
	case err == nil:
		if revokeErr := m.revokeSession(ctx, session, RevokeReasonReplay); revokeErr != nil {
			return revokeErr
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}

	tenantID := pkgcore.TenantID("")
	if session != nil {
		tenantID = pkgcore.TenantID(session.CurrentTenantID)
	}
	m.emitReplayAudit(ctx, record, tenantID)
	m.publish(ctx, pkgcore.Event{
		Type:     EventSessionReplayDetected,
		TenantID: tenantID,
		Payload: SessionReplayDetectedPayload{
			UserID:    record.UserID,
			SessionID: record.SessionID,
			FamilyID:  record.FamilyID,
		},
	})
	return ErrRefreshTokenReused
}

// emitReplayAudit records a detected replay response in the audit trail,
// through audit.Emit -- the same declarative collection mechanism
// recordAudit (handler.go) uses for this module's other session changes.
// handleReplay is the record's site: it is the single choke point through
// which every detection passes (an already-rotated token presented again,
// and a lost consume race, both funnel here), it holds the token record and
// revoked session the row must name, and no HTTP handler could record this
// at all -- a refresh request is credential-less by design, so the handler
// that answers its 401 has no principal and no session to attribute a row
// to. Recording here also covers every non-HTTP caller of Service.Refresh.
//
// The row reuses AuditActionSessionRevoke, the same action the handler's
// owner-initiated revocations record under, because the response IS a
// session revoke: the whole family and the session end, with
// RevokeReasonReplay the reason. Owner-initiated rows record Success=true;
// this one records the refused credential presentation the way every other
// refused presentation in this module's trail reads (a failed sign-in: a
// user.login row with Success=false), with RevokeReasonReplay as the
// FailureReason -- the same vocabulary value the revoked session row itself
// stores in its revoke_reason column, so the trail and the session table
// agree, and a replay response is never mistaken for a logout. The action's
// own doc (events.go) states the same ambiguity in place.
//
// Attribution: the row's actor is the account owner -- record.UserID, set
// explicitly on ctx -- because the refresh path attests no identity at all:
// the presented credential WAS the identity, and it was refused, so there
// is no authenticating layer upstream to vouch for an actor. The id is the
// authoritative attribution, exactly as recordAudit's own doc says of its
// actors; no display name is resolved here because this manager holds no
// users repository. The tenant is the one the revoked session itself acted
// in -- its CurrentTenantID -- or none when the session row is already
// gone, the same tenant the replay event's payload carries, layered
// unconditionally so an ambient context tenant can never leak onto the row.
//
// The emit is best-effort, like recordAudit: the family and session are
// already revoked and the caller is about to receive the 401, so an
// audit-write failure must not change that answer -- it is logged at Error
// for the operator instead. The bus is non-nil by construction
// (NewSessionManager refuses a nil one); the registrar is the only
// nil-capable input, and a nil one -- a directly assembled manager, before
// module.go's Register wiring -- skips the record silently, mirroring
// SSOService's own nil-registrar short-circuit.
func (m *SessionManager) emitReplayAudit(ctx context.Context, record *RefreshToken, tenantID pkgcore.TenantID) {
	if m.auditActions == nil {
		return
	}
	actx := pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: record.UserID})
	actx = pkgcore.WithTenant(actx, tenantID)
	if err := audit.Emit(actx, m.bus, m.auditActions, audit.Input{
		Action:   AuditActionSessionRevoke,
		Resource: audit.Resource{Type: "session", ID: record.SessionID},
		Result:   audit.Result{Success: false, FailureReason: RevokeReasonReplay},
	}); err != nil {
		obs.FromContext(ctx).Error("authn replay-response audit event emit failed",
			"user_id", record.UserID, "session_id", record.SessionID, "error", err)
	}
}

// RevokeOthers signs out every one of userID's sessions EXCEPT
// keepSessionID, and returns how many session rows this call actually
// revoked.
//
// The count is a count of rows this call flipped from active to revoked.
// Each flip is the same compare-and-swap Revoke performs -- status in the
// WHERE clause, the database as arbiter -- so a session a concurrent caller
// already revoked is neither revoked again nor counted here, which is what
// keeps a repeated call idempotent in its reported count as well as its
// effect.
//
// A failure never abandons the batch. Every eligible session is attempted
// whatever happens to the ones before it, and each failure -- a flip the
// database refused, a refresh-token invalidation or revocation-list write
// that errored after its session's row had already flipped -- is collected
// and returned alongside the count, errors.Join'ed, so the caller hears
// both how far the batch got and that something on it failed. A row whose
// flip succeeded is counted even when one of its follow-ups failed: the
// flip is the revocation, the follow-ups (tokens, the immediate-revocation
// entry, the event) are per-row work on top of it, and a follow-up failure
// must not make the account's true state unreportable.
//
// The revoke event is published only for rows THIS call flipped, so a batch
// racing a concurrent revoke of one of its targets does not announce a
// second security notice for that session.
func (m *SessionManager) RevokeOthers(ctx context.Context, userID, keepSessionID, reason string) (int, error) {
	now := m.now()
	sessions, err := m.sessions.ListByUser(ctx, userID)
	if err != nil {
		return 0, err
	}

	revoked := 0
	var failures []error
	for i := range sessions {
		session := &sessions[i]
		if session.ID == keepSessionID {
			continue
		}
		if session.Status != SessionStatusActive {
			continue
		}

		changed, err := m.sessions.Revoke(ctx, session.ID, reason, now)
		if err != nil {
			// The flip itself was refused; the row stands active and a
			// later retry can still reach it. Record and keep going.
			failures = append(failures, fmt.Errorf("authn: revoke session %s: %w", session.ID, err))
			continue
		}
		if !changed {
			// A concurrent revoke won between the list read and this flip.
			// Not this call's revocation: not counted, no event announced.
			continue
		}
		revoked++
		if _, err := m.tokens.RevokeBySession(ctx, session.ID, now); err != nil {
			failures = append(failures, fmt.Errorf("authn: revoke session %s refresh tokens: %w", session.ID, err))
		}
		if err := m.markRevoked(ctx, session.ID); err != nil {
			failures = append(failures, fmt.Errorf("authn: record session %s in the revocation list: %w", session.ID, err))
		}
		m.publish(ctx, pkgcore.Event{
			Type:     EventSessionRevoked,
			TenantID: pkgcore.TenantID(session.CurrentTenantID),
			Payload: SessionRevokedPayload{
				UserID:    session.UserID,
				SessionID: session.ID,
				Reason:    reason,
			},
		})
	}
	return revoked, errors.Join(failures...)
}

// Revoke signs a session out: the row is marked revoked, every refresh token
// bound to it is invalidated, and -- in immediate mode -- the session id is
// added to the revocation list Middleware consults.
//
// It is safe to call on an already revoked session; the event is published
// only for the call that actually changed the row, so a double sign-out does
// not produce two security notices.
func (m *SessionManager) Revoke(ctx context.Context, sessionID, reason string) error {
	session, err := m.sessions.FindByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return m.revokeSession(ctx, session, reason)
}

// revokeSession is Revoke's body once the session row is in hand; handleReplay
// calls it directly so the session is not loaded twice.
func (m *SessionManager) revokeSession(ctx context.Context, session *Session, reason string) error {
	now := m.now()
	sessionID := session.ID

	changed, err := m.sessions.Revoke(ctx, sessionID, reason, now)
	if err != nil {
		return err
	}
	if _, err := m.tokens.RevokeBySession(ctx, sessionID, now); err != nil {
		return err
	}
	if err := m.markRevoked(ctx, sessionID); err != nil {
		return err
	}
	if !changed {
		return nil
	}

	// The event belongs to the tenant the revoked session itself acted in
	// -- its CurrentTenantID -- not to whichever caller asked for the
	// revoke. A session can be ended by its own logout, by replay
	// detection inside a refresh, or later by a host job, and the fact
	// "this session stopped being usable" is a fact about that session in
	// that tenant; the session row is the one authoritative source for it
	// on every one of those paths.
	m.publish(ctx, pkgcore.Event{
		Type:     EventSessionRevoked,
		TenantID: pkgcore.TenantID(session.CurrentTenantID),
		Payload: SessionRevokedPayload{
			UserID:    session.UserID,
			SessionID: sessionID,
			Reason:    reason,
		},
	})
	return nil
}

// markRevoked adds sessionID to the immediate-revocation list, with a TTL of
// one access-token lifetime.
//
// That TTL is exactly right rather than merely convenient: any access token
// still valid now was issued at most one lifetime ago, so it cannot outlive
// the entry. A longer TTL would keep dead entries around; a shorter one would
// let a token outlive its own revocation, which is the bug this mode exists
// to prevent.
//
// In natural mode this does nothing at all -- there is no list.
func (m *SessionManager) markRevoked(ctx context.Context, sessionID string) error {
	if m.mode != RevocationModeImmediate {
		return nil
	}
	return m.kv.Set(ctx, revokedSessionKeyPrefix+sessionID, []byte{1}, m.accessTTL)
}

// IsRevoked reports whether sessionID is on the immediate-revocation list.
//
// In natural mode it always reports false without touching the store: that
// mode's whole point is that no per-request lookup happens. In immediate mode
// a store failure is returned as an error, and Middleware turns that into a
// refusal -- a revocation check that could not run is not a revocation check
// that passed.
func (m *SessionManager) IsRevoked(ctx context.Context, sessionID string) (bool, error) {
	if m.mode != RevocationModeImmediate {
		return false, nil
	}
	_, found, err := m.kv.Get(ctx, revokedSessionKeyPrefix+sessionID)
	if err != nil {
		return false, fmt.Errorf("authn: read revocation list: %w", err)
	}
	return found, nil
}

// Mode reports which revocation mode this manager was built with.
func (m *SessionManager) Mode() RevocationMode { return m.mode }

// publish emits evt and logs a delivery failure rather than returning it. The
// state change the event describes has already been committed, so failing the
// caller's operation would misreport a durable success as a failure; the log
// line is what an operator investigates instead.
func (m *SessionManager) publish(ctx context.Context, evt pkgcore.Event) {
	if err := m.bus.Publish(ctx, evt); err != nil {
		obs.FromContext(ctx).Warn("domain event publish failed", "event_type", evt.Type, "error", err)
	}
}

// newRefreshSecret draws a fresh opaque refresh token and returns it together
// with the digest to store.
func newRefreshSecret() (secret, digest string, err error) {
	raw := make([]byte, refreshSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("authn: draw refresh token: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(raw)
	return secret, hashRefreshSecret(secret), nil
}

// hashRefreshSecret returns the hex SHA-256 digest stored for secret.
func hashRefreshSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// compile-time check that *SessionManager satisfies the revocation seam
// Middleware consults.
var _ RevocationChecker = (*SessionManager)(nil)
