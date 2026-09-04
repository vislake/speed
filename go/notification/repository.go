package notification

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is the inbox's data path: the only sanctioned way to read and
// write in_app_messages rows.
//
// It is a named type embedding dbkit.Repository[InboxMessage] rather than
// the generic base itself, so the module's consumers and its documentation
// can name the concrete thing they hold -- the same reason org's repository
// and the reference app's notes repository declare their own named types.
// Everything Repository can do is promoted unchanged from the embedded
// base: Create, FindByID, Update, Delete and List, each carrying dbkit's
// tenant-isolation guarantees (the tenant comes from the context, never
// from the caller; a row of another tenant is indistinguishable from a row
// that does not exist, and both report dbkit's record-not-found code).
//
// The methods this type adds below -- the delivery path's FindByDedupeKey
// probe and the read surface the HTTP round builds on (ListForRecipient,
// UnreadCount, MarkRead, ReadAll) -- are its own query shapes, expressed
// the same way PreferenceRepository expresses its two (see its doc
// comment). Every one of them takes the recipient as an explicit argument
// because the row filter is the caller's identity, resolved by the HTTP
// layer's subject seam -- never read from the request, never assumed from
// the context -- while the tenant still comes from ctx alone.
type Repository struct {
	*dbkit.Repository[InboxMessage]

	// db is the same connection the embedded Repository was built on, kept
	// only so FindByDedupeKey can be composed on it. Every use routes
	// through WithTenantSession and a TenantScoped destination.
	db *gorm.DB
}

// NewRepository returns a Repository over db. db must already carry
// dbkit's tenant-isolation plugin -- the *gorm.DB dbkit.Open returns, which
// every host uses -- and the in_app_messages migration must already have
// been applied; the repository performs no I/O of its own at construction.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{
		Repository: dbkit.NewRepository[InboxMessage](db),
		db:         db,
	}
}

// FindByDedupeKey returns the inbox row whose dedupe_key is key, or
// (nil, nil) when no row carries it.
//
// This is the delivery path's redelivery probe: a retried job recomputes
// the same derived key (delivery.go's deriveDeliveryKey) and finds the row
// its first attempt wrote, turning a duplicate insert -- which the global
// UNIQUE index on dedupe_key would refuse -- into the "already delivered"
// answer that lets the retry converge without a second send.
//
// The tenant comes from ctx, and the query is written the way
// go/dbkit/AGENTS.md's "Known limitations" prescribes: built on the same
// *gorm.DB the embedded Repository was built on, against a TenantScoped
// destination, so the GORM isolation plugin still injects WHERE tenant_id
// even though Repository[T]'s own re-verification does not run for the
// call -- and run inside dbkit.WithTenantSession, so the PostgreSQL RLS
// session variable is set for it exactly as it is for every promoted
// method.
func (r *Repository) FindByDedupeKey(ctx context.Context, key string) (*InboxMessage, error) {
	var msg InboxMessage
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("dedupe_key = ?", key).First(&msg).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &msg, nil
}

// ListForRecipient returns the page of recipientUserID's own inbox rows in
// the tenant of ctx, newest first. The ordering is created_at DESC with id
// DESC as the tiebreak, so two rows written in the same instant still page
// deterministically (the spec's stable-paging promise, openapi.yaml's list
// operation).
//
// Expiry does not filter this listing: a message whose expiry_at passed is
// still a row of the recipient's inbox and still listed -- expiry governs
// only the unread predicate (see UnreadCount), never list membership, and
// the response row carries its expiry_at so the rendering side can drop the
// unread affordance itself. group restricts the page to one notification
// group ("" lists every group); a group with no rows answers an empty list,
// never an error. limit must be positive -- the HTTP surface resolves the
// spec's default and cap before this method is called -- and offset must be
// non-negative; the module's contract is that a caller reaching this method
// has already validated both, so nothing is silently clamped here.
func (r *Repository) ListForRecipient(ctx context.Context, recipientUserID, group string, limit, offset int) ([]InboxMessage, error) {
	var msgs []InboxMessage
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		q := tx.Where("recipient_user_id = ?", recipientUserID)
		if group != "" {
			// "group" is GROUP, a reserved word on PostgreSQL; the column is
			// quoted in the DDL (migrations' 0001 file) and must be quoted
			// here too -- both dialects accept the double-quoted form.
			q = q.Where(`"group" = ?`, group)
		}
		return q.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&msgs).Error
	})
	return msgs, err
}

// UnreadCount returns how many of recipientUserID's inbox rows in the
// tenant of ctx are unread under the module's unread predicate: read_at
// still nil AND expiry_at -- when set -- still in the future, compared
// against the server's own clock at the moment of the query (the spec's
// unread-count operation, openapi.yaml). A message that is unread but
// already expired does not count.
//
// The count is fetched as rows and measured with len rather than a COUNT
// query, because the module's data path never reaches for gorm's Count on a
// bare connection (dbkit/AGENTS.md's "Known limitations" prescribes the
// repository shapes this module may use, and a count is not one of them).
// A recipient's inbox is a pageable surface -- bounded in practice by the
// list cap and by expiry -- so materializing it to count it stays cheap.
func (r *Repository) UnreadCount(ctx context.Context, recipientUserID string) (int, error) {
	var msgs []InboxMessage
	now := time.Now().UTC()
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("recipient_user_id = ?", recipientUserID).
			Where("read_at IS NULL AND (expiry_at IS NULL OR expiry_at > ?)", now).
			Find(&msgs).Error
	})
	return len(msgs), err
}

// MarkRead marks messageID -- one of recipientUserID's own inbox rows in
// the tenant of ctx -- read, and answers nil.
//
// The operation is idempotent: a message already read answers nil exactly
// like a first read, without touching the row (the read_at already on it
// stays the first read's timestamp). A message id that is unknown, or that
// belongs to another recipient or another tenant, answers
// ErrMessageNotFound carrying the id -- the three cases are one refusal
// (see that error's comment), so one recipient can never learn whether
// another recipient's message id exists by probing it. Expiry does not
// gate the mark: an unread message whose expiry passed is still the
// caller's own row, and marking it read is what keeps the unread predicate
// monotone (the spec's mark-message-read operation).
func (r *Repository) MarkRead(ctx context.Context, recipientUserID, messageID string) error {
	var msg InboxMessage
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("id = ? AND recipient_user_id = ?", messageID, recipientUserID).
			First(&msg).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrMessageNotFound.WithParam("message_id", messageID)
		}
		return err
	}
	if msg.ReadAt != nil {
		return nil
	}

	now := time.Now().UTC()
	msg.ReadAt = &now
	if err := r.Update(ctx, &msg); err != nil {
		if isRecordNotFound(err) {
			// The row vanished between the fetch and the write; the message
			// the caller named no longer exists, which is the same refusal
			// the fetch above would have given it.
			return ErrMessageNotFound.WithParam("message_id", messageID)
		}
		return err
	}
	return nil
}

// ReadAll marks every inbox row recipientUserID still holds unread in the
// tenant of ctx -- read_at nil, expired or not: marking read is not gated
// on expiry, only counting is (the spec's mark-all-read operation) -- and
// returns how many rows the call actually flipped. A recipient with nothing
// unread gets (0, nil), the answer that distinguishes "nothing was unread"
// from "everything was already read" without a second round trip.
//
// Each flipped row is written through the promoted Update rather than one
// hand-written statement, because that is the module's only sanctioned
// write path; the loop is bounded by the rows the predicate matched, and a
// row that disappears between the fetch and its own write (another replica
// deleting it) is skipped rather than counted or failed. Every other write
// failure stops the loop and reports the error with the flips so far.
func (r *Repository) ReadAll(ctx context.Context, recipientUserID string) (int, error) {
	var msgs []InboxMessage
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("recipient_user_id = ?", recipientUserID).
			Where("read_at IS NULL").
			Find(&msgs).Error
	})
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	marked := 0
	for i := range msgs {
		msgs[i].ReadAt = &now
		if err := r.Update(ctx, &msgs[i]); err != nil {
			if isRecordNotFound(err) {
				continue
			}
			return marked, err
		}
		marked++
	}
	return marked, nil
}
