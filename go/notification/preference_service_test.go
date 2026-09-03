package notification

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/notification/internal/testutil"
	"github.com/vislake/speed/go/notification/migrations"
)

// The fixture taxonomy this file's tests validate against, standing in for
// the declarations real business modules register on a host's registry
// (types.go's doc comment): two unsubscribable types whose defaults span
// different channel sets, and one transactional type that cannot be switched
// off entirely. Every preference test below reasons about exactly these
// three declarations.
const (
	fixtureTypeAppointment = "clinic.appointment_reminder"
	fixtureTypeResult      = "clinic.result_ready"
	fixtureTypeSecurity    = "clinic.security_alert"
)

var fixtureTypes = []pkgcore.NotificationType{
	{Key: fixtureTypeAppointment, Group: "appointments", DefaultChannels: []string{ChannelInApp, ChannelEmail, ChannelSMS}, Unsubscribable: true},
	{Key: fixtureTypeResult, Group: "results", DefaultChannels: []string{ChannelInApp, ChannelEmail}, Unsubscribable: true},
	{Key: fixtureTypeSecurity, Group: "security", DefaultChannels: []string{ChannelInApp, ChannelEmail, ChannelSMS}, Unsubscribable: false},
}

// fixtureRegistrar is a stand-in for the registry's NotificationRegistrar
// (pkgcore.NotificationRegistrar): it answers Types() with a fixed list and
// accepts Add without complaint, which is all the service ever does with the
// registrar it was attached. Values semantics keep it safe to hand around.
type fixtureRegistrar struct {
	types []pkgcore.NotificationType
}

func (r fixtureRegistrar) Add(...pkgcore.NotificationType) error { return nil }
func (r fixtureRegistrar) Types() []pkgcore.NotificationType     { return r.types }

// attachedService returns a PreferenceService over a fresh migrated database
// with the fixture taxonomy attached, the state every real call runs in after
// Module.Register.
func attachedService(t *testing.T) *PreferenceService {
	t.Helper()
	svc := NewPreferenceService(newTestDB(t))
	svc.attachTypes(fixtureRegistrar{types: fixtureTypes})
	return svc
}

// detachedService returns a PreferenceService whose taxonomy was never
// attached -- the state before Module.Register ran.
func detachedService(t *testing.T) *PreferenceService {
	t.Helper()
	return NewPreferenceService(newTestDB(t))
}

// assertParam fails t unless err's apperr carries param key with the string
// value want.
func assertParam(t *testing.T, err error, key, want string) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error, want a %q parameter", err, want)
	}
	got, ok := appErr.Params[key]
	if !ok {
		t.Fatalf("error %v carries no %q parameter, want %q", err, key, want)
	}
	if got != want {
		t.Errorf("param %q = %v, want %q", key, got, want)
	}
}

// assertNoParam fails t unless err's apperr carries no param under key.
func assertNoParam(t *testing.T, err error, key string) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	if _, present := appErr.Params[key]; present {
		t.Errorf("error %v carries an unexpected %q parameter", err, key)
	}
}

// TestPreferenceService_Set_MissingRecipient_RefusedBeforeAnythingElse pins
// Set's refusal order step 1: an empty recipient is refused before the type
// taxonomy is even consulted, so a caller that forgot the user gets the
// recipient error even when it also named a type nobody declared -- the first
// fixable problem is the one reported.
func TestPreferenceService_Set_MissingRecipient_RefusedBeforeAnythingElse(t *testing.T) {
	svc := attachedService(t)

	err := svc.Set(tenantCtx("tenant-acme"), "", fixtureTypeAppointment, []string{ChannelInApp})
	assertCode(t, err, "notification.recipient_required")

	unknown := svc.Set(tenantCtx("tenant-acme"), "", "clinic.never_declared", []string{ChannelInApp})
	assertCode(t, unknown, "notification.recipient_required")
}

// TestPreferenceService_Set_UnknownType_Refused pins refusal order step 2: a
// type key outside the live taxonomy is refused with the type code and the
// offending key as its parameter, whatever channels the caller listed.
func TestPreferenceService_Set_UnknownType_Refused(t *testing.T) {
	svc := attachedService(t)

	err := svc.Set(tenantCtx("tenant-acme"), "user-7", "clinic.never_declared", []string{ChannelInApp, ChannelEmail})
	assertCode(t, err, "notification.type_not_found")
	assertParam(t, err, "type_key", "clinic.never_declared")
}

// TestPreferenceService_Set_DetachedService_NoTaxonomy_EveryTypeUnknown pins
// the nil-registrar reading (lookupType's doc comment): before Module.Register
// attached a registrar, the service treats every key as not-found -- the same
// honest answer an empty taxonomy gives -- and NotificationTypes reports nil,
// never a fabricated list.
func TestPreferenceService_Set_DetachedService_NoTaxonomy_EveryTypeUnknown(t *testing.T) {
	svc := detachedService(t)
	ctx := tenantCtx("tenant-acme")

	err := svc.Set(ctx, "user-7", fixtureTypeAppointment, []string{ChannelInApp})
	assertCode(t, err, "notification.type_not_found")
	assertParam(t, err, "type_key", fixtureTypeAppointment)

	_, err = svc.ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	assertCode(t, err, "notification.type_not_found")

	if got := svc.NotificationTypes(); got != nil {
		t.Errorf("NotificationTypes() = %v, want nil for a detached service", got)
	}
}

// TestPreferenceService_Set_InvalidSelections_RefusedWithParams pins refusal
// order step 3: a selection the type can never honor is refused whole with
// ErrPreferenceInvalidChannels, naming both the type and the caller's raw
// channel list -- a vocabulary stranger, a duplicate, or a channel the type
// does not use. Every refusal carries both parameters, so the client can
// render the error without echoing state back.
func TestPreferenceService_Set_InvalidSelections_RefusedWithParams(t *testing.T) {
	cases := []struct {
		name     string
		typeKey  string
		channels []string
	}{
		{"a channel outside the platform vocabulary", fixtureTypeAppointment, []string{"push"}},
		{"the same channel twice", fixtureTypeAppointment, []string{ChannelInApp, ChannelInApp}},
		{"a channel the type does not use", fixtureTypeResult, []string{ChannelSMS}},
		{"an unknown channel after a valid one", fixtureTypeAppointment, []string{ChannelEmail, "webhook"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := attachedService(t)
			err := svc.Set(tenantCtx("tenant-acme"), "user-7", tc.typeKey, tc.channels)
			assertCode(t, err, "notification.preference_invalid_channels")
			assertParam(t, err, "type_key", tc.typeKey)
			assertParam(t, err, "channels", strings.Join(tc.channels, ", "))
		})
	}
}

// TestPreferenceService_Set_EmptySelection_OnTransactionalType_Refused pins
// refusal order step 4's forbidden half: an empty selection on a type that
// may not be switched off (Unsubscribable false) is refused with the
// opt-out code. The error carries the type and nothing else -- the channel
// list is empty by definition and adds no information.
func TestPreferenceService_Set_EmptySelection_OnTransactionalType_Refused(t *testing.T) {
	svc := attachedService(t)

	err := svc.Set(tenantCtx("tenant-acme"), "user-7", fixtureTypeSecurity, nil)
	assertCode(t, err, "notification.preference_optout_not_allowed")
	assertParam(t, err, "type_key", fixtureTypeSecurity)
	assertNoParam(t, err, "channels")
}

// TestPreferenceService_Set_EmptySelection_OnUnsubscribableType_StoresOptOut
// pins refusal order step 4's legal half: on an unsubscribable type the empty
// selection is stored -- as "[]", never NULL and never as an absent row -- and
// ResolveChannels then answers with nothing. The matrix now distinguishes the
// recipient who never chose (defaults) from the recipient who chose nothing
// (opt-out), which is the whole point of the empty-array encoding.
func TestPreferenceService_Set_EmptySelection_OnUnsubscribableType_StoresOptOut(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	if err := svc.Set(ctx, "user-7", fixtureTypeAppointment, nil); err != nil {
		t.Fatalf("Set(empty selection on unsubscribable type): %v", err)
	}

	pref, err := svc.Get(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pref == nil {
		t.Fatal("Get = (nil, nil), want the stored opt-out row")
	}
	if string(pref.Channels) != "[]" {
		t.Errorf("stored channels = %s, want the JSON empty array []", pref.Channels)
	}

	got, err := svc.ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("ResolveChannels = %v (len %d), want an opted-out recipient to resolve to no channels", got, len(got))
	}
}

// TestPreferenceService_Set_StoresChannelsInCanonicalOrder proves the
// stored value is deterministic whatever order the caller listed: the
// canonical vocabulary order (in_app, email, sms), never the caller's. The
// JSON bytes are what the delivery subscriber's dedupe-key derivation will
// hash, so their determinism is the point.
func TestPreferenceService_Set_StoresChannelsInCanonicalOrder(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	if err := svc.Set(ctx, "user-7", fixtureTypeAppointment, []string{ChannelSMS, ChannelEmail, ChannelInApp}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	pref, err := svc.Get(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pref == nil {
		t.Fatal("Get = (nil, nil), want the stored row")
	}
	if string(pref.Channels) != `["in_app","email","sms"]` {
		t.Errorf("stored channels = %s, want the canonical order [\"in_app\",\"email\",\"sms\"]", pref.Channels)
	}
}

// TestPreferenceService_Set_SecondWrite_OverwritesSingleRow proves the
// upsert's second half: writing a new selection for a question that already
// has a stored answer replaces that answer in place -- the same row updated,
// never a second row -- because at most one answer per (tenant, recipient,
// type) is what the matrix's unique index guarantees.
func TestPreferenceService_Set_SecondWrite_OverwritesSingleRow(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	if err := svc.Set(ctx, "user-7", fixtureTypeAppointment, []string{ChannelSMS}); err != nil {
		t.Fatalf("Set(first): %v", err)
	}
	if err := svc.Set(ctx, "user-7", fixtureTypeAppointment, []string{ChannelEmail}); err != nil {
		t.Fatalf("Set(second): %v", err)
	}

	rows, err := svc.ListForUser(ctx, "user-7")
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListForUser returned %d rows, want 1: the second Set must update the row, not add one", len(rows))
	}
	got, err := parseChannels(rows[0].Channels)
	if err != nil {
		t.Fatalf("parseChannels: %v", err)
	}
	if !slices.Equal(got, []string{ChannelEmail}) {
		t.Errorf("channels after the second Set = %v, want the second selection [email]", got)
	}
}

// TestPreferenceService_Set_NoTenantInContext_FailsClosed proves every
// service entry point fails closed on a context carrying no tenant: the
// repository underneath refuses, and the service reports the refusal as its
// internal error (with the underlying cause on the record), never as a
// success and never as a tenant-shaped leak from the store.
func TestPreferenceService_Set_NoTenantInContext_FailsClosed(t *testing.T) {
	svc := attachedService(t)
	noTenant := context.Background()

	if err := svc.Set(noTenant, "user-7", fixtureTypeAppointment, []string{ChannelInApp}); err == nil {
		t.Error("Set(no tenant) succeeded, want an internal error")
	} else {
		assertCode(t, err, "notification.internal_error")
	}
	if _, err := svc.Get(noTenant, "user-7", fixtureTypeAppointment); err == nil {
		t.Error("Get(no tenant) succeeded, want an internal error")
	} else {
		assertCode(t, err, "notification.internal_error")
	}
	if _, err := svc.ResolveChannels(noTenant, "user-7", fixtureTypeAppointment); err == nil {
		t.Error("ResolveChannels(no tenant) succeeded, want an internal error")
	} else {
		assertCode(t, err, "notification.internal_error")
	}
	if _, err := svc.ListForUser(noTenant, "user-7"); err == nil {
		t.Error("ListForUser(no tenant) succeeded, want an internal error")
	} else {
		assertCode(t, err, "notification.internal_error")
	}
}

// TestPreferenceService_Get_NoStoredPreference_ReturnsNilNil proves Get
// answers from the row alone: no stored row means (nil, nil) whether or not
// the type still exists in the taxonomy -- the question "is there a stored
// answer" is answerable without the type, which is exactly why Get does not
// consult it (see Get's doc comment).
func TestPreferenceService_Get_NoStoredPreference_ReturnsNilNil(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	for _, typeKey := range []string{fixtureTypeAppointment, "clinic.never_declared"} {
		pref, err := svc.Get(ctx, "user-7", typeKey)
		if err != nil {
			t.Fatalf("Get(%s): %v", typeKey, err)
		}
		if pref != nil {
			t.Errorf("Get(%s) = %+v, want (nil, nil)", typeKey, pref)
		}
	}
}

// TestPreferenceService_ResolveChannels_NoPreference_ReturnsDeclaredDefaults
// proves the absence semantics at the service boundary: a recipient who never
// chose resolves to the type's declared defaults -- read live from the
// taxonomy, never materialized into a row. The returned slice must also be a
// copy: corrupting it (a delivery path sorting it, say) must not corrupt the
// declaration the next call reads.
func TestPreferenceService_ResolveChannels_NoPreference_ReturnsDeclaredDefaults(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	first, err := svc.ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	want := []string{ChannelInApp, ChannelEmail, ChannelSMS}
	if !slices.Equal(first, want) {
		t.Fatalf("ResolveChannels = %v, want the declared defaults %v", first, want)
	}

	first[0] = "corrupted"
	second, err := svc.ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels (after corrupting the first result): %v", err)
	}
	if !slices.Equal(second, want) {
		t.Errorf("ResolveChannels after the caller corrupted its first result = %v, want %v: the result must be a fresh copy", second, want)
	}
}

// TestPreferenceService_ResolveChannels_StoredPreference_Wins proves the
// other half of the fold: a stored selection -- including one that narrows
// the defaults -- is what a recipient actually gets, defaults be damned.
func TestPreferenceService_ResolveChannels_StoredPreference_Wins(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	if err := svc.Set(ctx, "user-7", fixtureTypeAppointment, []string{ChannelEmail}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := svc.ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	if !slices.Equal(got, []string{ChannelEmail}) {
		t.Errorf("ResolveChannels = %v, want the stored selection [email], not the full defaults", got)
	}
}

// TestPreferenceService_ResolveChannels_OtherTenantsPreference_Invisible
// proves the isolation fold: tenant A's stored preference must not leak into
// tenant B's resolution for the same person and type -- B's recipient never
// chose, so B resolves to the defaults. (A person who belongs to both tenants
// has separate preferences per tenant; that separation is the whole point of
// the tenant-scoped row.)
func TestPreferenceService_ResolveChannels_OtherTenantsPreference_Invisible(t *testing.T) {
	svc := attachedService(t)

	if err := svc.Set(tenantCtx("tenant-acme"), "user-7", fixtureTypeAppointment, []string{ChannelSMS}); err != nil {
		t.Fatalf("Set(tenant-acme): %v", err)
	}

	got, err := svc.ResolveChannels(tenantCtx("tenant-bright"), "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels(tenant-bright): %v", err)
	}
	want := []string{ChannelInApp, ChannelEmail, ChannelSMS}
	if !slices.Equal(got, want) {
		t.Errorf("ResolveChannels(tenant-bright) = %v, want the defaults %v: another tenant's preference must be invisible", got, want)
	}
}

// TestPreferenceService_ListForUser_StoredRowsOrderedByTypeKey drives the
// settings roster: stored rows only, ordered by type_key, scoped to the
// calling tenant and recipient. Rows were set in a deliberately shuffled
// order, and other recipients' and other tenants' rows were set alongside.
func TestPreferenceService_ListForUser_StoredRowsOrderedByTypeKey(t *testing.T) {
	svc := attachedService(t)
	ctx := tenantCtx("tenant-acme")

	for _, typeKey := range []string{fixtureTypeSecurity, fixtureTypeAppointment, fixtureTypeResult} {
		if err := svc.Set(ctx, "user-7", typeKey, []string{ChannelInApp}); err != nil {
			t.Fatalf("Set(%s): %v", typeKey, err)
		}
	}
	// Same recipient elsewhere, and another recipient here: neither may leak
	// into user-7's tenant-acme list.
	if err := svc.Set(tenantCtx("tenant-bright"), "user-7", fixtureTypeAppointment, []string{ChannelEmail}); err != nil {
		t.Fatalf("Set(tenant-bright): %v", err)
	}
	if err := svc.Set(ctx, "user-8", fixtureTypeAppointment, []string{ChannelEmail}); err != nil {
		t.Fatalf("Set(user-8): %v", err)
	}

	rows, err := svc.ListForUser(ctx, "user-7")
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	var keys []string
	for i := range rows {
		keys = append(keys, rows[i].TypeKey)
	}
	want := []string{fixtureTypeAppointment, fixtureTypeResult, fixtureTypeSecurity}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("ListForUser type keys = %v, want %v (ordered by type_key)", keys, want)
	}
}

// newBusyTimeoutSQLite opens a fresh, migrated SQLite database whose DSN
// carries a busy_timeout pragma, for tests that make concurrent writers
// collide on purpose.
//
// The plain dbtest.NewSQLite helper deliberately does not set one: its
// databases are per-test private files where a lock never has a second
// contender to wait for. A concurrency test is exactly that second
// contender: without a busy timeout, the loser of a write race on SQLite can
// fail with SQLITE_BUSY instead of waiting for the winner's commit -- and
// then observing a duplicate-key error, which is the branch under test, would
// be a scheduling accident rather than a guarantee. The pragma turns the
// loser's write into a wait, so the loser deterministically sees the winner's
// committed row (as a duplicate on insert, or as the row on re-read).
func newBusyTimeoutSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "preferences.sqlite") + "?_pragma=busy_timeout%3d5000"
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("open SQLite with busy_timeout: %v", err)
	}
	testutil.Migrate(t, db, dbkit.DialectSQLite, moduleName, migrations.FS)
	return db
}

// TestPreferenceService_ConcurrentFirstWrites_ConvergeOnSingleRow proves the
// upsert's first half under real contention: several writers racing to be the
// first to answer the same (tenant, recipient, type) question must converge
// on exactly one row with no error -- the loser's insert hits the unique
// index as a duplicate and the bounded retry re-reads and updates instead
// (see Set's doc comment on why last write wins is the right semantics for a
// preference).
//
// Each round races two writers on a fresh recipient over the same type;
// repeating the race across rounds is what keeps the create-vs-create branch
// exercised across schedulings -- some rounds the loser's read lands after
// the winner's insert and the loser simply updates, other rounds both reads
// land before either insert and the loser's insert genuinely collides. The
// busy_timeout database above is what makes both orderings deterministic
// outcomes rather than busy errors. Which writer's channels survive is the
// race's answer, so the assertion is membership in the two candidates, never
// a particular winner.
func TestPreferenceService_ConcurrentFirstWrites_ConvergeOnSingleRow(t *testing.T) {
	db := newBusyTimeoutSQLite(t)
	svc := NewPreferenceService(db)
	svc.attachTypes(fixtureRegistrar{types: fixtureTypes})
	ctx := tenantCtx("tenant-acme")

	const writers = 2
	const rounds = 5
	candidates := [][]string{
		{ChannelInApp},
		{ChannelEmail, ChannelSMS},
	}

	errs := make(chan error, writers*rounds)
	var wg sync.WaitGroup
	for round := 1; round <= rounds; round++ {
		recipient := fmt.Sprintf("user-%d", round)
		start := make(chan struct{})
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(selection []string) {
				defer wg.Done()
				<-start
				errs <- svc.Set(ctx, recipient, fixtureTypeAppointment, selection)
			}(candidates[w])
		}
		close(start)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Set error = %v, want every writer to converge", err)
		}
	}

	for round := 1; round <= rounds; round++ {
		recipient := fmt.Sprintf("user-%d", round)
		rows, err := svc.ListForUser(ctx, recipient)
		if err != nil {
			t.Fatalf("ListForUser(%s): %v", recipient, err)
		}
		if len(rows) != 1 {
			t.Errorf("ListForUser(%s) returned %d rows, want exactly 1: concurrent first-writes must converge on a single row", recipient, len(rows))
			continue
		}
		got, err := parseChannels(rows[0].Channels)
		if err != nil {
			t.Fatalf("parseChannels: %v", err)
		}
		if !slices.Equal(got, candidates[0]) && !slices.Equal(got, candidates[1]) {
			t.Errorf("surviving channels for %s = %v, want one of the two written selections %v or %v",
				recipient, got, candidates[0], candidates[1])
		}
	}
}

// TestPreferenceService_NotificationTypes_LiveTaxonomy exposes the service's
// view of the taxonomy to the settings screen that renders the preference
// matrix: the full attached list, in registration order -- the order a UI
// shows rows in.
func TestPreferenceService_NotificationTypes_LiveTaxonomy(t *testing.T) {
	svc := attachedService(t)

	got := svc.NotificationTypes()
	if !reflect.DeepEqual(got, fixtureTypes) {
		t.Errorf("NotificationTypes() = %v, want the attached taxonomy %v", got, fixtureTypes)
	}
}
