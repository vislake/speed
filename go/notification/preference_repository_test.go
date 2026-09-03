package notification

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newTestPreference returns a fully populated preference row with id as its
// primary key. Its TenantModel is deliberately left empty: dbkit's repository
// overwrites TenantID from the context's tenant, and the AssertIsolated suite
// below pins that behaviour.
func newTestPreference(id string) *NotificationPreference {
	return &NotificationPreference{
		ID:              id,
		RecipientUserID: "user-7",
		TypeKey:         "clinic.appointment_reminder",
		Channels:        channelsJSON([]string{ChannelInApp, ChannelEmail}),
	}
}

// TestPreferenceRepository_AssertIsolated runs the tenant-data isolation suite
// every tenant-scoped repository in this codebase is required to pass
// (docs/internal/04-data-and-tenancy.md): two tenants' rows created side by
// side, each tenant's FindByID/List/Update/Delete seeing only its own, a
// forged TenantID overwritten by the context's tenant, and a tenant-less
// context failing closed.
//
// The closure must vary more than the id: notification_preferences' unique
// index (tenant, recipient, type) forbids two rows for one tenant answering
// the same recipient-and-type question, and the suite creates several rows
// per tenant -- so the recipient advances with the same counter as the id,
// keeping every row the suite asks for a distinct question.
func TestPreferenceRepository_AssertIsolated(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)

	seq := 0
	tenancytest.AssertIsolated[NotificationPreference](t, repo.Repository, func(tenant pkgcore.TenantID) *NotificationPreference {
		seq++
		return &NotificationPreference{
			ID:              fmt.Sprintf("pref-%s-%06d", tenant, seq),
			RecipientUserID: fmt.Sprintf("user-%06d", seq),
			TypeKey:         "clinic.appointment_reminder",
			Channels:        channelsJSON([]string{ChannelInApp, ChannelEmail}),
		}
	})
}

// TestPreferenceRepository_ByUserAndType_NoRow_ReturnsNilNil pins the
// repository's most important contract: an absent row is a VALUE in this
// domain -- the type's DefaultChannels apply -- reported as (nil, nil), never
// as an error and never as a zero-valued row.
func TestPreferenceRepository_ByUserAndType_NoRow_ReturnsNilNil(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)

	got, err := repo.ByUserAndType(tenantCtx("tenant-acme"), "user-7", "clinic.appointment_reminder")
	if err != nil {
		t.Fatalf("ByUserAndType on an empty table: %v", err)
	}
	if got != nil {
		t.Errorf("ByUserAndType = %+v, want (nil, nil) for an absent row", got)
	}
}

// TestPreferenceRepository_ByUserAndType_OtherTenantsRow_Invisible proves the
// cross-tenant collapse at the custom-finder level: another tenant's row for
// the same recipient and type must read exactly like a row that does not
// exist -- (nil, nil), never a row and never a more revealing error. The
// tenant filter comes from the context alone (the isolation plugin), never
// from the finder's own WHERE clause.
func TestPreferenceRepository_ByUserAndType_OtherTenantsRow_Invisible(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)

	pref := newTestPreference("pref-000001")
	if err := repo.Create(tenantCtx("tenant-acme"), pref); err != nil {
		t.Fatalf("Create(tenant-acme): %v", err)
	}

	got, err := repo.ByUserAndType(tenantCtx("tenant-bright"), pref.RecipientUserID, pref.TypeKey)
	if err != nil {
		t.Fatalf("ByUserAndType(other tenant): %v", err)
	}
	if got != nil {
		t.Errorf("ByUserAndType(other tenant) = %+v, want (nil, nil): another tenant's row is indistinguishable from an absent one", got)
	}
}

// TestPreferenceRepository_DuplicateQuestion_SecondCreateRejected proves the
// unique index is the schema's whole answer to "what is a preference": two
// rows for one (tenant, recipient, type) are refused with a duplicate-key
// error -- the same error PreferenceService.Set's upsert retry loop reads
// when it loses a concurrent first-write race.
func TestPreferenceRepository_DuplicateQuestion_SecondCreateRejected(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)
	ctx := tenantCtx("tenant-acme")

	first := newTestPreference("pref-000001")
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	second := newTestPreference("pref-000002")
	if err := repo.Create(ctx, second); err == nil {
		t.Fatal("Create(same recipient and type, new id) succeeded, want a duplicate-key error")
	} else if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("Create error = %v, want errors.Is(err, gorm.ErrDuplicatedKey)", err)
	}

	got, err := repo.ByUserAndType(ctx, first.RecipientUserID, first.TypeKey)
	if err != nil {
		t.Fatalf("ByUserAndType: %v", err)
	}
	if got == nil || got.ID != first.ID {
		t.Errorf("surviving row = %v, want the first write %q to be untouched", got, first.ID)
	}
}

// TestPreferenceRepository_SameQuestion_TwoTenants_TwoAnswers proves a
// preference is meaningful only inside the tenant it was set in: the same
// person may want different channels in different tenants, and each tenant's
// row is an independent answer -- visible to its own tenant, invisible to the
// other. (Different tenants may set different channel lists for the same
// type; nothing about the matrix is cross-tenant.)
func TestPreferenceRepository_SameQuestion_TwoTenants_TwoAnswers(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)

	acmePref := newTestPreference("pref-acme-1")
	if err := repo.Create(tenantCtx("tenant-acme"), acmePref); err != nil {
		t.Fatalf("Create(tenant-acme): %v", err)
	}
	brightPref := newTestPreference("pref-bright-1")
	brightPref.Channels = channelsJSON([]string{ChannelSMS})
	if err := repo.Create(tenantCtx("tenant-bright"), brightPref); err != nil {
		t.Fatalf("Create(tenant-bright): %v", err)
	}

	acmeGot, err := repo.ByUserAndType(tenantCtx("tenant-acme"), "user-7", "clinic.appointment_reminder")
	if err != nil {
		t.Fatalf("ByUserAndType(tenant-acme): %v", err)
	}
	if acmeGot == nil || string(acmeGot.Channels) != `["in_app","email"]` {
		t.Errorf("tenant-acme read = %v, want its own [in_app, email] row", acmeGot)
	}
	brightGot, err := repo.ByUserAndType(tenantCtx("tenant-bright"), "user-7", "clinic.appointment_reminder")
	if err != nil {
		t.Fatalf("ByUserAndType(tenant-bright): %v", err)
	}
	if brightGot == nil || string(brightGot.Channels) != `["sms"]` {
		t.Errorf("tenant-bright read = %v, want its own [sms] row", brightGot)
	}
}

// TestPreferenceRepository_ListByUser_OrderedAndScoped drives the settings
// roster through the custom finder: one recipient's rows only, ordered by
// type_key, with the other tenant's rows for the same recipient and this
// tenant's rows for other recipients both absent. The rows were created in a
// deliberately shuffled order, so the assertion is about the ORDER clause,
// not about insertion order.
func TestPreferenceRepository_ListByUser_OrderedAndScoped(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)
	ctx := tenantCtx("tenant-acme")

	order := []string{
		"clinic.security_alert",
		"clinic.appointment_reminder",
		"clinic.result_ready",
	}
	for i, typeKey := range order {
		pref := newTestPreference(fmt.Sprintf("pref-%06d", i+1))
		pref.TypeKey = typeKey
		if err := repo.Create(ctx, pref); err != nil {
			t.Fatalf("Create(%s): %v", typeKey, err)
		}
	}

	// A row for a different recipient in the same tenant, and a row for the
	// same recipient in a different tenant: neither may appear in user-7's
	// acme list.
	other := newTestPreference("pref-other-1")
	other.RecipientUserID = "user-8"
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("Create(other recipient): %v", err)
	}
	cross := newTestPreference("pref-cross-1")
	if err := repo.Create(tenantCtx("tenant-bright"), cross); err != nil {
		t.Fatalf("Create(tenant-bright): %v", err)
	}

	rows, err := repo.ListByUser(ctx, "user-7")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	var keys []string
	for i := range rows {
		keys = append(keys, rows[i].TypeKey)
	}
	want := []string{"clinic.appointment_reminder", "clinic.result_ready", "clinic.security_alert"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("ListByUser type keys = %v, want %v (ordered by type_key)", keys, want)
	}
	for _, row := range rows {
		if row.RecipientUserID != "user-7" {
			t.Errorf("list contains a row for %q, want user-7's rows only", row.RecipientUserID)
		}
		if row.TenantID != "tenant-acme" {
			t.Errorf("list contains a row for tenant %q, want tenant-acme's rows only", row.TenantID)
		}
	}
}

// TestPreferenceRepository_RoundTrip pins that every column written through
// Create comes back unchanged through ByUserAndType -- including the channels
// bytes, whose exact JSON form (canonical order, never NULL) delivery relies
// on. The empty TenantModel doubles as an assertion that the read-back row
// reports the context's tenant, not an empty string.
func TestPreferenceRepository_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)
	ctx := tenantCtx("tenant-acme")

	pref := newTestPreference("pref-000001")
	if err := repo.Create(ctx, pref); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.ByUserAndType(ctx, pref.RecipientUserID, pref.TypeKey)
	if err != nil {
		t.Fatalf("ByUserAndType: %v", err)
	}
	if got.ID != pref.ID {
		t.Errorf("ID = %q, want %q", got.ID, pref.ID)
	}
	if got.TenantID != "tenant-acme" {
		t.Errorf("TenantID = %q, want the context tenant %q", got.TenantID, "tenant-acme")
	}
	if got.RecipientUserID != pref.RecipientUserID {
		t.Errorf("RecipientUserID = %q, want %q", got.RecipientUserID, pref.RecipientUserID)
	}
	if got.TypeKey != pref.TypeKey {
		t.Errorf("TypeKey = %q, want %q", got.TypeKey, pref.TypeKey)
	}
	if string(got.Channels) != `["in_app","email"]` {
		t.Errorf("Channels = %s, want the stored JSON [\"in_app\",\"email\"]", got.Channels)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; autoCreateTime did not run")
	}
}

// TestPreferenceRepository_NoTenantInContext_FailsClosed proves the custom
// finders keep the repository's fail-closed property on the two paths they
// own: a context carrying no tenant is refused before any row is touched.
// Repository-promoted calls are already covered by the AssertIsolated suite;
// ByUserAndType and ListByUser run outside Repository[T]'s own re-verification
// (they are built on the plain *gorm.DB, see preference_repository.go's doc
// comment), so their no-tenant behaviour needs its own proof.
func TestPreferenceRepository_NoTenantInContext_FailsClosed(t *testing.T) {
	db := newTestDB(t)
	repo := NewPreferenceRepository(db)
	noTenant := context.Background()

	if err := repo.Create(noTenant, newTestPreference("pref-000001")); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("Create(no tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
	if _, err := repo.ByUserAndType(noTenant, "user-7", "clinic.appointment_reminder"); err == nil {
		t.Error("ByUserAndType(no tenant) succeeded, want an error")
	}
	if _, err := repo.ListByUser(noTenant, "user-7"); err == nil {
		t.Error("ListByUser(no tenant) succeeded, want an error")
	}
}
