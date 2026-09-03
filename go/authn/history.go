package authn

import (
	"context"
	"errors"
)

// Reasons recorded in [Session.RevokeReason], continuing the set declared
// next to RevokeReasonLogout and RevokeReasonReplay in model.go -- declared
// here instead because both only ever originate from the two methods below,
// the same "constant lives beside its one call site" convention verification.go
// already follows for MethodSMS.
const (
	// RevokeReasonUserRevoked is an owner explicitly signing out ONE OTHER
	// device from their own device list (RevokeSession). It is distinct
	// from RevokeReasonLogout, which is a device signing itself out.
	RevokeReasonUserRevoked = "user_revoked"

	// RevokeReasonRevokeOthers is an owner signing out every session
	// except the one they are currently using (RevokeOtherSessions).
	RevokeReasonRevokeOthers = "revoke_others"
)

// maxLoginHistoryLimit is the hard ceiling on ListLoginHistory's limit
// parameter, independent of what a caller requests, so a client cannot turn
// its own login-history page into an unbounded query. defaultLoginHistoryLimit
// itself -- the fallback for a non-positive limit -- is already declared in
// repository.go, next to LoginAttemptRepository.ListByUser, which applies
// that same fallback a second time; ListLoginHistory applies it here too so
// the clamped value it logs and the value it hands the repository agree.
const maxLoginHistoryLimit = 200

// ListSessions returns every session belonging to userID, most recently
// created first, for the caller's own device-management page.
//
// It deliberately does not filter by Session.Status: a revoked session still
// belongs on the list (so a person can see "this is the device I signed out
// yesterday"), with Status itself telling the caller which rows are still
// active. Marking one entry as "this is the device you are using right now"
// is the caller's job, comparing each returned Session.ID against the
// calling Principal.SessionID -- this method has no opinion about which
// request is asking.
func (s *Service) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	return s.sessionRepo.ListByUser(ctx, userID)
}

// RevokeSession signs out ONE of userID's own sessions, identified by
// sessionID, from their device list.
//
// A sessionID that does not exist and a sessionID that belongs to somebody
// else produce the exact same ErrSessionNotFound, for the same
// no-existence-disclosure reason UnbindIdentity's identical check is:
// confirming that a session id is real but belongs to another account would
// let a caller enumerate other people's session ids by trying ones they do
// not own.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID string) error {
	if userID == "" {
		return ErrAuthenticationRequired
	}
	if sessionID == "" {
		return ErrSessionNotFound
	}

	session, err := s.sessionRepo.FindByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrSessionNotFound
		}
		return err
	}
	if session.UserID != userID {
		return ErrSessionNotFound
	}

	return s.sessions.Revoke(ctx, sessionID, RevokeReasonUserRevoked)
}

// RevokeOtherSessions signs out every one of userID's sessions EXCEPT
// currentSessionID, and returns how many were actually revoked.
//
// currentSessionID must itself belong to userID -- it comes from the calling
// Principal, never from a request parameter, so a caller can never ask to
// keep somebody else's session alive while revoking their own. Only sessions
// that are still SessionStatusActive count toward the returned total: a
// session already revoked contributes nothing to revoke a second time, which
// is what keeps a repeated call idempotent in its reported count as well as
// its effect.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, currentSessionID string) (int, error) {
	if userID == "" || currentSessionID == "" {
		return 0, ErrAuthenticationRequired
	}

	sessions, err := s.sessionRepo.ListByUser(ctx, userID)
	if err != nil {
		return 0, err
	}

	revoked := 0
	for i := range sessions {
		session := &sessions[i]
		if session.ID == currentSessionID {
			continue
		}
		if session.Status != SessionStatusActive {
			continue
		}
		if err := s.sessions.Revoke(ctx, session.ID, RevokeReasonRevokeOthers); err != nil {
			return revoked, err
		}
		revoked++
	}
	return revoked, nil
}

// ListLoginHistory returns userID's most recent login attempts, successful
// or not, newest first -- the record their own security page and an
// operator's investigation both read from.
//
// limit is clamped to (0, maxLoginHistoryLimit]: a non-positive value falls
// back to defaultLoginHistoryLimit, and anything larger than
// maxLoginHistoryLimit is capped rather than rejected, so a slightly
// over-eager client degrades to the ceiling instead of failing outright.
func (s *Service) ListLoginHistory(ctx context.Context, userID string, limit int) ([]LoginAttempt, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	switch {
	case limit <= 0:
		limit = defaultLoginHistoryLimit
	case limit > maxLoginHistoryLimit:
		limit = maxLoginHistoryLimit
	}
	return s.attempts.ListByUser(ctx, userID, limit)
}
