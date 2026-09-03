package notification

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// PreferenceService is the preference matrix's decision layer: the only
// sanctioned way to set, read and resolve a recipient's channel preferences.
//
// It sits between the module's future HTTP handler (and the delivery
// subscriber of a later block) and PreferenceRepository. What makes it a
// service rather than a thin repository pass-through is that the matrix is
// only meaningful against the type taxonomy, which the repository must never
// know about:
//
//   - a preference names a notification type, and the type's declaration
//     (the channels it uses, whether opting out is legal) comes from the
//     LIVE registrar, read at call time -- never a snapshot, and never
//     denormalized onto the row. Set validates the caller's selection
//     against that declaration and refuses anything it cannot honor, and
//     ResolveForDelivery answers "which channels does this recipient
//     actually get" by folding the declaration's defaults under an absent
//     row.
//   - the matrix's absence semantics live here too: no row means "the
//     recipient has not chosen" and the type's DefaultChannels apply
//     (preference.go's doc comment), and the stored empty array means a
//     deliberate opt-out. ResolveForDelivery is where those two readings
//     diverge into two different answers.
//
// The service is stateless apart from its dependencies: the repository, the
// id generator (a seam so tests can pin ids), and the registrar reference
// wired by Module.Register through attachTypes. It performs no I/O at
// construction and keeps nothing across calls.
type PreferenceService struct {
	// repo is the preference matrix's data path.
	repo *PreferenceRepository

	// newID supplies the primary key of a newly created preference row.
	// It is a field rather than a direct uuid.NewString call so tests can
	// override it with a deterministic sequence; production uses
	// uuid.NewString (see NewPreferenceService).
	newID func() string

	// types is the live notification-type taxonomy the service validates
	// against. It is nil until Module.Register attaches the host registry's
	// registrar through attachTypes; every method treats a nil source as an
	// empty taxonomy -- see lookupType's doc comment for why that is the
	// honest answer rather than an error about wiring.
	types pkgcore.NotificationRegistrar
}

// NewPreferenceService returns a PreferenceService over db, with uuid-based
// row ids. db must come from dbkit.Open and carry this module's migrations,
// exactly as PreferenceRepository requires; the service performs no I/O at
// construction.
func NewPreferenceService(db *gorm.DB) *PreferenceService {
	return &PreferenceService{
		repo:  NewPreferenceRepository(db),
		newID: uuid.NewString,
	}
}

// attachTypes binds the registry's notification-type registrar to the
// service. Module.Register calls it during registration, after the module's
// own declarations succeeded, so a host whose modules register their
// notification types after notification's own Register ran still gets a live
// taxonomy (see types.go's doc comment). Attaching is idempotent -- the last
// registrar wins, and there is exactly one caller.
func (s *PreferenceService) attachTypes(types pkgcore.NotificationRegistrar) {
	s.types = types
}

// lookupType returns the declared notification type whose Key is typeKey, or
// ErrTypeNotFound when nobody declared it. A nil registrar source -- the
// service before Module.Register attached one -- is treated as declaring
// nothing at all: with no types registered, every key is not-found, which is
// the same honest answer a host that never registers any notification type
// should get, and it keeps the error path uniform instead of inventing a
// second, wiring-shaped error for a case that is indistinguishable from an
// empty taxonomy at call time.
func (s *PreferenceService) lookupType(typeKey string) (pkgcore.NotificationType, error) {
	if s.types == nil {
		return pkgcore.NotificationType{}, ErrTypeNotFound.WithParam("type_key", typeKey)
	}
	for _, typ := range s.types.Types() {
		if typ.Key == typeKey {
			return typ, nil
		}
	}
	return pkgcore.NotificationType{}, ErrTypeNotFound.WithParam("type_key", typeKey)
}

// Set stores the recipient's channel selection for one notification type as
// their preference, replacing whatever they had stored before. It is the
// matrix's only write path: an HTTP handler of a later block, a settings
// screen's submit, all land here.
//
// The call is validated in a fixed order, and each refusal is a distinct
// error so a client can tell them apart:
//
//  1. An empty recipientUserID is refused with ErrRecipientRequired: a
//     preference row is meaningless without the user it applies to, and
//     storing one would make the row unreachable (every read is keyed by
//     recipient).
//  2. A typeKey nobody declared is refused with ErrTypeNotFound.
//  3. A selection the type can never honor -- a channel outside the
//     platform vocabulary, the same channel twice, or a channel the type
//     does not use -- is refused whole with ErrPreferenceInvalidChannels.
//     Refusing the whole write on the first offending channel, rather than
//     silently dropping channels, is what keeps every stored row a subset
//     of its type's DefaultChannels, the invariant ResolveChannels reasons
//     from.
//  4. An empty selection on a type whose declaration does not permit
//     opting out (Unsubscribable false) is refused with
//     ErrPreferenceOptoutNotAllowed; on an unsubscribable type the empty
//     array is stored and means the recipient gets nothing.
//
// A valid selection is stored in canonical vocabulary order (types.go's
// sortedChannels), whatever order the caller listed it in. "Stored" is an
// upsert: the row for (tenant, recipient, type) is created when none exists
// and updated when one does. Two concurrent first-writes race on the table's
// unique index; the loser of that race retries once, re-reading the row the
// winner committed and updating it, so concurrent Set calls converge on a
// single row with the last write winning.
//
// The tenant comes from ctx; without one every repository call below fails
// closed. Every failure inside the store is reported as
// ErrInternal.WithCause.
func (s *PreferenceService) Set(ctx context.Context, recipientUserID, typeKey string, channels []string) error {
	if recipientUserID == "" {
		return ErrRecipientRequired
	}

	typ, err := s.lookupType(typeKey)
	if err != nil {
		return err
	}
	canonical, err := validateSelection(typeKey, typ, channels)
	if err != nil {
		return err
	}

	// Upsert with a bounded duplicate-key retry: the find-then-create pair
	// below cannot close the race between two first-writes on its own, so
	// the unique index is the backstop, and a lost race -- at most once, on
	// the very first attempt's create -- re-reads and updates instead of
	// failing. Last write wins, which is the correct semantics for a
	// preference: the later submission is the recipient's later decision.
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := s.repo.ByUserAndType(ctx, recipientUserID, typeKey)
		if err != nil {
			return ErrInternal.WithCause(err)
		}
		if existing == nil {
			pref := &NotificationPreference{
				ID:              s.newID(),
				RecipientUserID: recipientUserID,
				TypeKey:         typeKey,
				Channels:        channelsJSON(canonical),
			}
			if err := s.repo.Create(ctx, pref); err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) && attempt == 0 {
					continue // another writer created the row between our read and our create
				}
				return ErrInternal.WithCause(err)
			}
			return nil
		}
		existing.Channels = channelsJSON(canonical)
		if err := s.repo.Update(ctx, existing); err != nil {
			return ErrInternal.WithCause(err)
		}
		return nil
	}
	return ErrInternal.WithCause(errors.New("notification: preference write did not converge after a duplicate-key retry"))
}

// Get returns the recipient's stored preference for one type, or (nil, nil)
// when they have none.
//
// Unlike Set and ResolveChannels, Get does not consult the type taxonomy:
// the question it answers -- "is there a stored row for this (recipient,
// type)" -- is answerable from the row alone, and (nil, nil) means "no
// stored preference" whether or not the type still exists. A caller who
// needs the type's existence or its defaults uses ResolveChannels, which
// folds the taxonomy in.
//
// The tenant comes from ctx; another tenant's row is indistinguishable from
// no row (see PreferenceRepository.ByUserAndType). Store failures are
// reported as ErrInternal.WithCause.
func (s *PreferenceService) Get(ctx context.Context, recipientUserID, typeKey string) (*NotificationPreference, error) {
	pref, err := s.repo.ByUserAndType(ctx, recipientUserID, typeKey)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return pref, nil
}

// ResolveForDelivery answers the delivery question: which channels should a
// delivery to this recipient, for this type, actually use, and under which
// group does the type's copy sit?
//
// The group is the notification type's declaration, read once here and
// handed on unmodified: delivery stores it on the inbox row and on nothing
// else, and every row a type's deliveries produce therefore carries the
// group the declaring module chose, whatever the recipient's own
// preferences did to the channels.
//
// The channel answer is the stored preference when the recipient has one,
// and the type's declared DefaultChannels when they do not -- absence means
// "has not chosen", and the defaults are never materialized into rows (see
// preference.go), so the recipient who never chose keeps receiving on the
// type's defaults, including when a later release changes what those are.
// An opted-out recipient (a stored empty selection, legal only on an
// unsubscribable type) resolves to an empty slice -- the answer that makes
// delivery's opt-out semantics: a user who turns a channel off receives
// nothing on it, whatever the type declares.
//
// Unlike Get, this method validates the type first and refuses an unknown
// typeKey with ErrTypeNotFound: with no declaration there are no defaults to
// fold in, and answering from nothing would silently invent channels.
//
// The returned slice is a fresh copy -- a caller may sort or slice it
// without corrupting the source's declaration, and a caller must never
// mutate the declaration through it. Store failures and a corrupt stored
// channels column are reported as ErrInternal.WithCause.
func (s *PreferenceService) ResolveForDelivery(ctx context.Context, recipientUserID, typeKey string) (group string, channels []string, err error) {
	typ, err := s.lookupType(typeKey)
	if err != nil {
		return "", nil, err
	}

	pref, err := s.repo.ByUserAndType(ctx, recipientUserID, typeKey)
	if err != nil {
		return "", nil, ErrInternal.WithCause(err)
	}
	if pref == nil {
		return typ.Group, slices.Clone(typ.DefaultChannels), nil
	}

	channels, err = parseChannels(pref.Channels)
	if err != nil {
		return "", nil, ErrInternal.WithCause(err)
	}
	return typ.Group, channels, nil
}

// ResolveChannels answers ResolveForDelivery's channel half alone, for
// callers that need the channels and not the group.
func (s *PreferenceService) ResolveChannels(ctx context.Context, recipientUserID, typeKey string) ([]string, error) {
	_, channels, err := s.ResolveForDelivery(ctx, recipientUserID, typeKey)
	return channels, err
}

// ListForUser returns every preference the recipient has stored in the
// tenant of ctx, ordered by type_key (the matrix's rendering order). It is
// the roster behind a settings screen's "your preferences" panel: stored
// rows only -- a type the recipient never chose is absent from the list by
// design, and the panel renders its defaults from the live taxonomy
// (NotificationTypes) instead.
func (s *PreferenceService) ListForUser(ctx context.Context, recipientUserID string) ([]NotificationPreference, error) {
	prefs, err := s.repo.ListByUser(ctx, recipientUserID)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return prefs, nil
}

// NotificationTypes returns the live type taxonomy, in registration order,
// for callers that render the preference matrix's rows and their defaults.
// It returns nil -- the honest empty taxonomy -- before Module.Register
// attached one; no caller should treat that as a type missing from a larger
// set, because there is no larger set to miss from.
func (s *PreferenceService) NotificationTypes() []pkgcore.NotificationType {
	if s.types == nil {
		return nil
	}
	return s.types.Types()
}

// validateSelection checks a proposed channel selection against the type it
// is for, and returns the selection in canonical vocabulary order when it is
// valid. It implements Set's refusal order step 3 and 4 -- see Set's doc
// comment for the rationale of each error.
func validateSelection(typeKey string, typ pkgcore.NotificationType, channels []string) ([]string, error) {
	invalid := func() error {
		return ErrPreferenceInvalidChannels.
			WithParam("type_key", typeKey).
			WithParam("channels", strings.Join(channels, ", "))
	}

	seen := make(map[string]struct{}, len(channels))
	for _, ch := range channels {
		if _, dup := seen[ch]; dup {
			return nil, invalid()
		}
		seen[ch] = struct{}{}
		if !isKnownChannel(ch) {
			return nil, invalid()
		}
		if !slices.Contains(typ.DefaultChannels, ch) {
			return nil, invalid()
		}
	}

	if len(channels) == 0 && !typ.Unsubscribable {
		return nil, ErrPreferenceOptoutNotAllowed.WithParam("type_key", typeKey)
	}

	return sortedChannels(channels), nil
}
