package config

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// service_test.go exercises the Service's public surface -- Get/GetTyped's
// scope fallback, Set's guards, the Sensitive-at-rest round trip, the
// change events and Watch deliveries, the feature-flag dependency walk, and
// the poller's anti-loss convergence -- against a real in-memory SQLite
// configs table and a real in-memory bus.
//
// assertCode and assertParam, defined here because this file asserts the
// most errors, are shared by the whole package's test files: decorated
// *apperr.Error values must be matched on their Code (see apperr.Error's
// doc comment), never by identity.

func assertCode(t *testing.T, err error, want *apperr.Error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error with code %q, got nil", want.Code)
	}
	got, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want an *apperr.Error with code %q, got %T: %v", want.Code, err, err)
	}
	if got.Code != want.Code {
		t.Fatalf("want error code %q, got %q: %v", want.Code, got.Code, err)
	}
}

func assertParam(t *testing.T, err error, param string, want any) {
	t.Helper()
	got, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want an *apperr.Error carrying param %q, got %T: %v", param, err, err)
	}
	value, present := got.Params[param]
	if !present {
		t.Fatalf("error %q carries no param %q (params: %v)", got.Code, param, got.Params)
	}
	if value != want {
		t.Fatalf("error %q param %q = %v, want %v", got.Code, param, value, want)
	}
}

// serviceTestDBSeq numbers the in-memory SQLite databases this file's tests
// open, so parallel or repeated runs never share one.
var serviceTestDBSeq atomic.Int64

func openServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:config_service_%d?mode=memory&cache=shared", serviceTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(NewModule(db)); err != nil {
		t.Fatalf("registering the config migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the config migrations: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// serviceTestSchemaItems is a small realistic declaration set the service
// tests attach: a public string with a default, a public duration with a
// default, a Sensitive string, an int with range bounds and a default, and
// a string without a default (the ErrItemUnset material).
var serviceTestSchemaItems = []pkgcore.ConfigItem{
	{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
	{Key: "brand.welcome_interval", Type: "duration", Default: 90 * time.Second, Public: true, Description: "How long the welcome banner stays", Group: "brand"},
	{Key: "support.reply_email", Type: "string", Sensitive: true, Description: "The address support replies come from", Group: "support"},
	{Key: "billing.retry_limit", Type: "int", Default: int(3), Min: int(1), Max: int64(10), Description: "How many payment retries an invoice gets", Group: "billing"},
	{Key: "brand.help_url", Type: "string", Description: "Where the help link points"},
}

// serviceTestSchemaFlags is the two-flag dependency chain the flag tests
// walk: ai.premium_upsell defaults true but depends on ai.smile_preview,
// which defaults false -- so neither is enabled until a tenant turns the
// preview on.
var serviceTestSchemaFlags = []pkgcore.FeatureFlag{
	{Key: "ai.smile_preview", Default: false, Description: "Lets tenants try smile previews"},
	{Key: "ai.premium_upsell", Default: true, Description: "Shows the premium upsell", DependsOn: []string{"ai.smile_preview"}},
}

// buildTestCipher returns a fresh AES-GCM cipher over a random 32-byte key,
// the shape a host's master key has. Used by this file and by http_test.go.
func buildTestCipher(t *testing.T) *dbkit.Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	cipher, err := dbkit.NewCipher(key)
	if err != nil {
		t.Fatalf("dbkit.NewCipher: %v", err)
	}
	return cipher
}

// attachServiceForTest folds items and flags into a fresh in-memory registry
// and returns the Service Attach produced, plus the bus the registry was
// built on (so tests can capture events). Register's one process-global
// side effect is replicated here -- the module's system purpose is declared
// -- mirroring what a real Bootstrap performs before Attach.
func attachServiceForTest(t *testing.T, db *gorm.DB, cipher *dbkit.Cipher, items []pkgcore.ConfigItem, flags []pkgcore.FeatureFlag, opts ...Option) (*Service, pkgcore.EventBus) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Config.Add(items...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(flags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	moduleOpts := []Option{WithPollInterval(0)}
	if cipher != nil {
		moduleOpts = append(moduleOpts, WithCipher(cipher))
	}
	moduleOpts = append(moduleOpts, opts...)
	svc, err := NewModule(db, moduleOpts...).Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return svc, bus
}

// attachDefaultServiceForTest attaches the shared item/flag set with a
// cipher, the common case for the scope, write and watch tests.
func attachDefaultServiceForTest(t *testing.T, opts ...Option) *Service {
	t.Helper()
	svc, _ := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), serviceTestSchemaItems, serviceTestSchemaFlags, opts...)
	return svc
}

// tenantA and tenantB are the two arbitrary tenants the isolation tests
// write overrides for.
func tenantA() context.Context { return pkgcore.WithTenant(context.Background(), "tenant-a") }
func tenantB() context.Context { return pkgcore.WithTenant(context.Background(), "tenant-b") }

// systemWriteCtx returns a context carrying the module's audited system
// purpose, the one a ScopeSystem write demands.
func systemWriteCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: SystemPurposeSystemWrite,
		Ticket:  "ticket-42",
	})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	return ctx
}

// capturedEvents is a bus subscriber tests register before a Set to assert
// the config.item.changed payload the Set published. The in-memory bus
// delivers synchronously on the publishing goroutine, so no locking is
// needed.
type capturedEvents struct {
	events []pkgcore.Event
}

func (c *capturedEvents) handler(_ context.Context, evt pkgcore.Event) error {
	c.events = append(c.events, evt)
	return nil
}

// itemChanged extracts the event's payload as ItemChangedEvent, failing the
// test on a payload of any other shape.
func (c *capturedEvents) itemChanged(t *testing.T) []ItemChangedEvent {
	t.Helper()
	out := make([]ItemChangedEvent, 0, len(c.events))
	for _, evt := range c.events {
		payload, ok := evt.Payload.(ItemChangedEvent)
		if !ok {
			t.Fatalf("event %q carries payload %T, want ItemChangedEvent", evt.Type, evt.Payload)
		}
		out = append(out, payload)
	}
	return out
}

func TestService_Get_ServesSchemaDefaults(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	v, err := svc.Get(context.Background(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Data != "Smile Studio" {
		t.Fatalf("Get(brand.site_name).Data = %#v, want %q", v.Data, "Smile Studio")
	}
	// A value that came from the schema default is served with the zero
	// Scope: there is no row tier to name.
	if v.Scope != "" || v.Redacted {
		t.Fatalf("default value served as scope=%q redacted=%v", v.Scope, v.Redacted)
	}

	if name, err := GetTyped[string](svc, context.Background(), "brand.site_name"); err != nil || name != "Smile Studio" {
		t.Fatalf("GetTyped[string] = %q, %v", name, err)
	}
	if interval, err := GetTyped[time.Duration](svc, context.Background(), "brand.welcome_interval"); err != nil || interval != 90*time.Second {
		t.Fatalf("GetTyped[duration] = %v, %v", interval, err)
	}
	if limit, err := GetTyped[int64](svc, context.Background(), "billing.retry_limit"); err != nil || limit != 3 {
		t.Fatalf("GetTyped[int64] = %d, %v", limit, err)
	}
}

func TestService_Get_ReportsUnsetItemWithoutARowOrDefault(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := getCodeHelper(svc, "brand.help_url")
	assertCode(t, err, ErrItemUnset)
	assertParam(t, err, "key", "brand.help_url")
}

// getCodeHelper discards the value, keeping only the error of one Get.
func getCodeHelper(svc *Service, key string) error {
	_, err := svc.Get(context.Background(), key)
	return err
}

func TestService_Get_RejectsUnknownKeys(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := getCodeHelper(svc, "brand.nonexistent")
	assertCode(t, err, ErrUnknownKey)
	assertParam(t, err, "key", "brand.nonexistent")
}

func TestService_ScopeFallback_SystemRowServesEveryTenant(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}

	// A system row is read by a tenant-less context and by every tenant's
	// resolution path alike.
	for name, ctx := range map[string]context.Context{"no tenant": context.Background(), "tenant a": tenantA(), "tenant b": tenantB()} {
		v, err := svc.Get(ctx, "brand.site_name")
		if err != nil {
			t.Fatalf("Get under %s: %v", name, err)
		}
		if v.Data != "Global Co" {
			t.Fatalf("Get under %s = %#v, want the system row %q", name, v.Data, "Global Co")
		}
		if v.Scope != ScopeSystem {
			t.Fatalf("Get under %s served scope %q, want %q", name, v.Scope, ScopeSystem)
		}
	}
}

func TestService_ScopeFallback_TenantOverrideBeatsSystemRow(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("tenant Set: %v", err)
	}

	v, err := svc.Get(tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get under tenant a: %v", err)
	}
	if v.Data != "Studio A" || v.Scope != ScopeTenant {
		t.Fatalf("Get under tenant a = %#v at scope %q, want the tenant override", v.Data, v.Scope)
	}

	// The override is per-tenant: tenant b and the platform still read the
	// system row, and a tenant-less context never consults the tenant tier.
	if v, err := svc.Get(tenantB(), "brand.site_name"); err != nil || v.Data != "Global Co" {
		t.Fatalf("Get under tenant b = %#v, %v; want the system row", v.Data, err)
	}
	if v, err := svc.Get(context.Background(), "brand.site_name"); err != nil || v.Data != "Global Co" {
		t.Fatalf("Get without a tenant = %#v, %v; want the system row", v.Data, err)
	}
}

func TestService_ScopeFallback_OverridesAreIsolatedBetweenTenants(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set under tenant a: %v", err)
	}
	if err := svc.Set(tenantB(), ScopeTenant, "brand.site_name", Value{Data: "Studio B"}, "bob"); err != nil {
		t.Fatalf("Set under tenant b: %v", err)
	}

	// Each tenant reads its own override; a third tenant and the platform
	// fall through to the schema default untouched.
	for name, tc := range map[string]struct {
		ctx  context.Context
		want any
	}{
		"tenant a":  {ctx: tenantA(), want: "Studio A"},
		"tenant b":  {ctx: tenantB(), want: "Studio B"},
		"tenant c":  {ctx: pkgcore.WithTenant(context.Background(), "tenant-c"), want: "Smile Studio"},
		"no tenant": {ctx: context.Background(), want: "Smile Studio"},
	} {
		v, err := svc.Get(tc.ctx, "brand.site_name")
		if err != nil {
			t.Fatalf("Get under %s: %v", name, err)
		}
		if v.Data != tc.want {
			t.Fatalf("Get under %s = %#v, want %#v", name, v.Data, tc.want)
		}
	}
}

func TestService_GetTyped_RejectsWrongGoTypes(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	// An int item is served as int64, so a generic read pinned to the Go
	// int width must report the mismatch rather than silently truncating.
	if _, err := GetTyped[int](svc, context.Background(), "billing.retry_limit"); err == nil {
		t.Fatal("GetTyped[int] on an int item succeeded; int items are served as int64 and must reject a narrower read")
	} else {
		assertCode(t, err, ErrTypedValueMismatch)
	}
	if _, err := GetTyped[string](svc, context.Background(), "billing.retry_limit"); err == nil {
		t.Fatal("GetTyped[string] on an int item succeeded")
	} else {
		assertCode(t, err, ErrTypedValueMismatch)
	}
	if _, err := GetTyped[bool](svc, context.Background(), "brand.site_name"); err == nil {
		t.Fatal("GetTyped[bool] on a string item succeeded")
	} else {
		assertCode(t, err, ErrTypedValueMismatch)
	}
	if _, err := GetTyped[float64](svc, context.Background(), "brand.site_name"); err == nil {
		t.Fatal("GetTyped[float64] succeeded; no item type decodes to a float")
	} else {
		assertCode(t, err, ErrTypedValueMismatch)
	}
}

func TestService_GetTyped_ReadsFlagsAsBools(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	on, err := GetTyped[bool](svc, context.Background(), "ai.smile_preview")
	if err != nil || on {
		t.Fatalf("GetTyped[bool](ai.smile_preview) = %v, %v; want the flag's default false", on, err)
	}
}

func TestService_Set_RejectsUnknownKeys(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := svc.Set(tenantA(), ScopeTenant, "brand.nonexistent", Value{Data: "x"}, "alice")
	assertCode(t, err, ErrUnknownKey)
	assertParam(t, err, "key", "brand.nonexistent")
}

func TestService_Set_RequiresAnActor(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "")
	assertCode(t, err, ErrActorRequired)
	assertParam(t, err, "key", "brand.site_name")
}

func TestService_Set_RejectsInvalidAndUnavailableScopes(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	err := svc.Set(tenantA(), Scope("bogus"), "brand.site_name", Value{Data: "x"}, "alice")
	assertCode(t, err, ErrInvalidScope)
	assertParam(t, err, "key", "brand.site_name")

	err = svc.Set(tenantA(), ScopeUser, "brand.site_name", Value{Data: "x"}, "alice")
	assertCode(t, err, ErrUserScopeUnavailable)
	assertParam(t, err, "key", "brand.site_name")
}

func TestService_Set_FailsClosedWithoutTheScopesEntitlement(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	// A tenant write without a tenant in the context has no owning tenant
	// to attribute the row to.
	err := svc.Set(context.Background(), ScopeTenant, "brand.site_name", Value{Data: "x"}, "alice")
	assertCode(t, err, ErrTenantScopeRequiresTenant)
	assertParam(t, err, "key", "brand.site_name")

	// A system write without an audited system context must fail: no
	// tenant-scoped request may widen a platform setting.
	err = svc.Set(tenantA(), ScopeSystem, "brand.site_name", Value{Data: "x"}, "alice")
	assertCode(t, err, ErrSystemScopeRequiresSystemContext)
	assertParam(t, err, "key", "brand.site_name")
}

func TestService_Set_ValidatesValueKindAndRange(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: int(5)}, "alice")
	assertCode(t, err, ErrInvalidValue)
	assertParam(t, err, "key", "brand.site_name")

	for _, tooLow := range []int{0, -1} {
		err = svc.Set(tenantA(), ScopeTenant, "billing.retry_limit", Value{Data: tooLow}, "alice")
		assertCode(t, err, ErrInvalidValue)
		assertParam(t, err, "key", "billing.retry_limit")
	}
	err = svc.Set(tenantA(), ScopeTenant, "billing.retry_limit", Value{Data: 11}, "alice")
	assertCode(t, err, ErrInvalidValue)

	// The declared bounds are inclusive: 1 and 10 are legal writes.
	for _, limit := range []int{1, 10} {
		if err := svc.Set(tenantA(), ScopeTenant, "billing.retry_limit", Value{Data: limit}, "alice"); err != nil {
			t.Fatalf("Set(retry_limit = %d): %v", limit, err)
		}
	}
}

func TestService_SetAndGet_RoundTripATenantOverride(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := svc.Get(tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Data != "Studio A" || v.Scope != ScopeTenant {
		t.Fatalf("round-tripped value = %#v at scope %q", v.Data, v.Scope)
	}
}

func TestService_Set_WritesSensitiveValuesEncryptedAtRest(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "support.reply_email", Value{Data: "ops@example.com"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The stored row must not carry the plaintext: the value column holds
	// base64(AES-GCM(plaintext)), decryptable only through the cipher.
	stored, err := svc.st.get(context.Background(), ScopeTenant, "tenant-a", "support.reply_email")
	if err != nil || stored == nil {
		t.Fatalf("reading the stored row: row=%v err=%v", stored, err)
	}
	if stored.Value == "ops@example.com" {
		t.Fatal("the sensitive value reached the configs table in plaintext")
	}
	sealed, err := base64.StdEncoding.DecodeString(stored.Value)
	if err != nil {
		t.Fatalf("the stored value is not valid base64: %v", err)
	}
	plain, err := svc.cipher.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypting the stored value: %v", err)
	}
	if string(plain) != "ops@example.com" {
		t.Fatalf("decrypted stored value = %q, want the written value", plain)
	}

	// The read path decrypts: Get serves the plaintext to entitled callers.
	v, err := svc.Get(tenantA(), "support.reply_email")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Data != "ops@example.com" || v.Redacted {
		t.Fatalf("Get(sensitive) = %#v redacted=%v; the read path serves the clear value", v.Data, v.Redacted)
	}
}

func TestService_Set_PublishesItemChangedEvents(t *testing.T) {
	svc, bus := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), serviceTestSchemaItems, serviceTestSchemaFlags)
	var captured capturedEvents
	bus.Subscribe(EventConfigItemChanged, captured.handler)

	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A2"}, "alice"); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	events := captured.itemChanged(t)
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2", len(events))
	}

	first, second := events[0], events[1]
	for name, evt := range map[string]ItemChangedEvent{"first": first, "second": second} {
		if evt.Key != "brand.site_name" || evt.Scope != ScopeTenant || evt.TenantID != "tenant-a" || evt.Actor != "alice" || evt.Sensitive {
			t.Fatalf("%s event = %+v", name, evt)
		}
		if evt.ChangedAt.IsZero() || evt.ChangedAt.After(time.Now()) {
			t.Fatalf("%s event ChangedAt = %v, not a plausible write time", name, evt.ChangedAt)
		}
	}
	if first.OldValue != "" || first.NewValue != "Studio A" {
		t.Fatalf("first event OldValue=%q NewValue=%q, want %q then %q", first.OldValue, first.NewValue, "", "Studio A")
	}
	if second.OldValue != "Studio A" || second.NewValue != "Studio A2" {
		t.Fatalf("second event OldValue=%q NewValue=%q, want the previous then the new value", second.OldValue, second.NewValue)
	}

	// A system-tier write is platform-wide news: the event names the system
	// scope and no tenant.
	captured.events = nil
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	systemEvent := captured.itemChanged(t)
	if len(systemEvent) != 1 {
		t.Fatalf("captured %d system events, want 1", len(systemEvent))
	}
	if systemEvent[0].TenantID != "" || systemEvent[0].Scope != ScopeSystem || systemEvent[0].Actor != "ops-1" {
		t.Fatalf("system event = %+v", systemEvent[0])
	}
}

func TestService_Set_PublishesRedactedSensitiveEvents(t *testing.T) {
	svc, bus := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), serviceTestSchemaItems, serviceTestSchemaFlags)
	var captured capturedEvents
	bus.Subscribe(EventConfigItemChanged, captured.handler)

	if err := svc.Set(tenantA(), ScopeTenant, "support.reply_email", Value{Data: "ops@example.com"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	events := captured.itemChanged(t)
	if len(events) != 1 {
		t.Fatalf("captured %d events, want 1", len(events))
	}
	evt := events[0]
	if !evt.Sensitive {
		t.Fatal("the event must mark itself Sensitive")
	}
	if evt.NewValue != redactedMarker {
		t.Fatalf("event NewValue = %q, want the marker %q -- the value never crosses the bus", evt.NewValue, redactedMarker)
	}
	if evt.OldValue != redactedMarker {
		t.Fatalf("event OldValue = %q, want the marker (no previous value exists, and a sensitive absence is reported as the marker too)", evt.OldValue)
	}

	// A second write reports the old value as the marker as well, even
	// though the row held a decryptable value: redaction happens before the
	// payload is built.
	captured.events = nil
	if err := svc.Set(tenantA(), ScopeTenant, "support.reply_email", Value{Data: "support@example.com"}, "alice"); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if evt := captured.itemChanged(t); len(evt) != 1 || evt[0].OldValue != redactedMarker || evt[0].NewValue != redactedMarker {
		t.Fatalf("second sensitive event = %+v, want both values marked", evt)
	}
}

func TestService_Set_CacheAdvancesEvenWhenPublishFails(t *testing.T) {
	svc, bus := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), serviceTestSchemaItems, serviceTestSchemaFlags)
	bus.Subscribe(EventConfigItemChanged, func(context.Context, pkgcore.Event) error {
		return fmt.Errorf("a downstream subscriber is down")
	})

	err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice")
	assertCode(t, err, ErrAuditPublishFailed)

	// The write landed and this process's own cache advanced before the
	// publish: the failed delivery must not leave the process serving the
	// value it just overwrote.
	v, err := svc.Get(tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get after failed publish: %v", err)
	}
	if v.Data != "Studio A" {
		t.Fatalf("Get after failed publish = %#v; want the value the Set wrote", v.Data)
	}
}

func TestService_Watch_FiresSynchronouslyOnSet(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	delivered := make(chan Value, 4)
	if err := svc.Watch("brand.site_name", func(v Value) { delivered <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case v := <-delivered:
		if v.Data != "Studio A" || v.Scope != ScopeTenant || v.Redacted {
			t.Fatalf("watch delivery = %+v, want the tenant-tier change value", v)
		}
	default:
		t.Fatal("no watch delivery on the in-memory bus; Set must deliver synchronously")
	}

	// A system-tier change is platform-wide news and is delivered as such,
	// whatever tier the watcher would resolve the key at for itself.
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	select {
	case v := <-delivered:
		if v.Data != "Global Co" || v.Scope != ScopeSystem {
			t.Fatalf("watch delivery = %+v, want the system-tier change value", v)
		}
	default:
		t.Fatal("no watch delivery for the system-tier change")
	}
}

func TestService_Watch_DeliversRedactedValueForSensitiveKeys(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	delivered := make(chan Value, 1)
	if err := svc.Watch("support.reply_email", func(v Value) { delivered <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := svc.Set(tenantA(), ScopeTenant, "support.reply_email", Value{Data: "ops@example.com"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case v := <-delivered:
		if !v.Redacted || v.Data != nil {
			t.Fatalf("sensitive watch delivery = %+v, want a redacted value (Data nil)", v)
		}
		if v.Scope != ScopeTenant {
			t.Fatalf("sensitive watch delivery scope = %q, want %q", v.Scope, ScopeTenant)
		}
	default:
		t.Fatal("no watch delivery for the sensitive key")
	}
}

func TestService_Watch_RejectsUnknownKeys(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := svc.Watch("brand.nonexistent", func(Value) {})
	assertCode(t, err, ErrUnknownKey)
	assertParam(t, err, "key", "brand.nonexistent")
}

func TestService_IsEnabled_WalksTheDependencyChain(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	// ai.premium_upsell defaults true but depends on ai.smile_preview,
	// which defaults false: the flag chain is off until the preview is on.
	for key, want := range map[string]bool{
		"ai.smile_preview":  false,
		"ai.premium_upsell": false,
	} {
		got, err := svc.IsEnabled(context.Background(), key)
		if err != nil {
			t.Fatalf("IsEnabled(%s): %v", key, err)
		}
		if got != want {
			t.Fatalf("IsEnabled(%s) = %v, want %v", key, got, want)
		}
	}

	// Turning the dependency on for tenant a enables the whole chain -- for
	// tenant a only.
	if err := svc.Set(tenantA(), ScopeTenant, "ai.smile_preview", Value{Data: true}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := svc.IsEnabled(tenantA(), "ai.premium_upsell")
	if err != nil || !got {
		t.Fatalf("IsEnabled(premium_upsell) under tenant a = %v, %v; want true once its dependency is on", got, err)
	}
	if got, err := svc.IsEnabled(tenantB(), "ai.premium_upsell"); err != nil || got {
		t.Fatalf("IsEnabled(premium_upsell) under tenant b = %v, %v; want false", got, err)
	}

	// A flag's own false overrides its dependencies' state: with the preview
	// on, turning the upsell off for tenant a leaves the chain off for it.
	if err := svc.Set(tenantA(), ScopeTenant, "ai.premium_upsell", Value{Data: false}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := svc.IsEnabled(tenantA(), "ai.premium_upsell"); err != nil || got {
		t.Fatalf("IsEnabled(premium_upsell) with its own override off = %v, %v; want false", got, err)
	}
}

func TestService_IsEnabled_OnlyKnowsDeclaredFlags(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	err := codeHelperIsEnabled(svc, "brand.nonexistent")
	assertCode(t, err, ErrUnknownFlag)
	assertParam(t, err, "key", "brand.nonexistent")

	// A plain ConfigItem key -- even a bool one -- has no flag semantics.
	err = codeHelperIsEnabled(svc, "brand.site_name")
	assertCode(t, err, ErrUnknownFlag)
}

// codeHelperIsEnabled discards the boolean, keeping only the error.
func codeHelperIsEnabled(svc *Service, key string) error {
	_, err := svc.IsEnabled(context.Background(), key)
	return err
}

func TestService_EnabledFlags_ReturnsSortedEnabledFlags(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	flags, err := svc.EnabledFlags(tenantA())
	if err != nil {
		t.Fatalf("EnabledFlags: %v", err)
	}
	if len(flags) != 0 {
		t.Fatalf("EnabledFlags with the chain off = %v, want none", flags)
	}

	if err = svc.Set(tenantA(), ScopeTenant, "ai.smile_preview", Value{Data: true}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	flags, err = svc.EnabledFlags(tenantA())
	if err != nil {
		t.Fatalf("EnabledFlags: %v", err)
	}
	if len(flags) != 2 || flags[0] != "ai.premium_upsell" || flags[1] != "ai.smile_preview" {
		t.Fatalf("EnabledFlags = %v, want the two enabled flags sorted", flags)
	}

	// The enablement is per-tenant: tenant b still has none.
	flags, err = svc.EnabledFlags(tenantB())
	if err != nil || len(flags) != 0 {
		t.Fatalf("EnabledFlags under tenant b = %v, %v; want none", flags, err)
	}
}

func TestService_PublicSnapshot_ServesPublicItemsOnly(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.welcome_interval", Value{Data: 2 * time.Minute}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("tenant Set: %v", err)
	}

	values, features, err := svc.PublicSnapshot(tenantA())
	if err != nil {
		t.Fatalf("PublicSnapshot: %v", err)
	}
	if values["brand.site_name"] != "Studio A" {
		t.Fatalf("snapshot brand.site_name = %#v, want the tenant override", values["brand.site_name"])
	}
	// Durations are served as their canonical text, not as an int64
	// nanosecond count JSON would render.
	if values["brand.welcome_interval"] != "2m0s" {
		t.Fatalf("snapshot brand.welcome_interval = %#v (%T), want the canonical %q", values["brand.welcome_interval"], values["brand.welcome_interval"], "2m0s")
	}
	if _, present := values["support.reply_email"]; present {
		t.Fatal("the sensitive item leaked into the public snapshot")
	}
	if len(features) != 0 {
		t.Fatalf("snapshot features = %v, want none while the chain is off", features)
	}

	// The platform's own snapshot reads the system rows; a tenant override
	// never leaks into it.
	platformValues, _, err := svc.PublicSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PublicSnapshot(no tenant): %v", err)
	}
	if platformValues["brand.site_name"] != "Global Co" {
		t.Fatalf("platform snapshot brand.site_name = %#v, want the system row", platformValues["brand.site_name"])
	}

	// Enabled flags join the snapshot once a tenant turns the chain on.
	if err = svc.Set(tenantA(), ScopeTenant, "ai.smile_preview", Value{Data: true}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, features, err = svc.PublicSnapshot(tenantA())
	if err != nil {
		t.Fatalf("PublicSnapshot: %v", err)
	}
	if len(features) != 2 {
		t.Fatalf("snapshot features = %v, want the enabled chain", features)
	}
}

func TestService_Refresh_InvalidatesRowsWrittenBehindItsBack(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := svc.Get(tenantA(), "brand.site_name"); v.Data != "Studio A" {
		t.Fatalf("warm-up Get = %#v, want the cached override", v.Data)
	}

	// A row changed by a writer this process never heard from (a direct
	// store write standing in for another instance whose event was lost)
	// leaves the cache serving the stale value until Refresh invalidates.
	changedAt := time.Now()
	if err := svc.st.put(context.Background(), row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Backdoor B", UpdatedBy: "other-instance", UpdatedAt: changedAt,
	}); err != nil {
		t.Fatalf("direct store.put: %v", err)
	}
	if v, _ := svc.Get(tenantA(), "brand.site_name"); v.Data != "Studio A" {
		t.Fatalf("pre-Refresh Get = %#v, want the still-cached value", v.Data)
	}

	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	v, err := svc.Get(tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get after Refresh: %v", err)
	}
	if v.Data != "Backdoor B" {
		t.Fatalf("Get after Refresh = %#v, want the row written behind the service's back", v.Data)
	}
}

func TestService_Poller_ConvergesAStaleCache(t *testing.T) {
	svc := attachDefaultServiceForTest(t, WithPollInterval(2*time.Millisecond))
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := svc.Get(tenantA(), "brand.site_name"); v.Data != "Studio A" {
		t.Fatalf("warm-up Get = %#v", v.Data)
	}

	if err := svc.st.put(context.Background(), row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Backdoor B", UpdatedBy: "other-instance", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("direct store.put: %v", err)
	}

	// No Refresh call happens here: only the poller's own sweeps may
	// invalidate the cache. It must converge within a bounded wait.
	eventually(t, 5*time.Second, func() bool {
		v, err := svc.Get(tenantA(), "brand.site_name")
		return err == nil && v.Data == "Backdoor B"
	})
}

// eventually polls probe until it reports true or timeout elapses. It is
// the bounded loop the poller and async-delivery tests wait on.
func eventually(t *testing.T, timeout time.Duration, probe func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probe() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition was never met within %v", timeout)
}
