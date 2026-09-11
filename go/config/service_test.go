package config

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
	if !apperr.HasCode(err, want.Code) {
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
// the shape a host's cipher key has. Used by this file and by http_test.go.
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
	reg := componenttest.NewRegistryWithBus(bus)
	moduleOpts := []Option{WithPollInterval(0)}
	if cipher != nil {
		moduleOpts = append(moduleOpts, WithCipher(cipher))
	}
	moduleOpts = append(moduleOpts, opts...)
	// Attach runs inside the same Init stage: it wires the Service and
	// installs its subscriptions, and both are seat writes the window owns.
	var svc *Service
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(items...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(flags...) },
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := NewModule(db, moduleOpts...).Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			return nil
		},
	); err != nil {
		t.Fatalf("declare and attach: %v", err)
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

// capturedLogs is a slog.Handler tests install on a context (through
// obs.WithLogger) so a Warn the service emitted on a request path can be
// asserted. Records are appended on the calling goroutine only, so no
// locking is needed.
type capturedLogs struct {
	records []slog.Record
}

func (c *capturedLogs) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturedLogs) Handle(_ context.Context, r slog.Record) error {
	c.records = append(c.records, r)
	return nil
}

func (c *capturedLogs) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturedLogs) WithGroup(string) slog.Handler      { return c }

// noWarn fails the test if any captured record sits at Warn level or above.
func (c *capturedLogs) noWarn(t *testing.T) {
	t.Helper()
	for _, r := range c.records {
		if r.Level >= slog.LevelWarn {
			t.Fatalf("captured an unexpected %s record: %s", r.Level, r.Message)
		}
	}
}

// warnedAbout fails the test unless a captured Warn record carries an
// "item" attribute equal to wantItem.
func (c *capturedLogs) warnedAbout(t *testing.T, wantItem string) {
	t.Helper()
	for _, r := range c.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		var item string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "item" {
				if s, ok := a.Value.Any().(string); ok {
					item = s
				}
			}
			return true
		})
		if item == wantItem {
			return
		}
	}
	t.Fatalf("want a Warn naming item %q, captured records: %v", wantItem, c.records)
}

// errorAboutPanic fails the test unless a captured Error record carries an
// "item" attribute equal to wantItem, a "watcher" attribute equal to
// wantWatcher and a "panic" attribute equal to wantPanic -- the exact shape
// the recovery log of a panicking Watch callback has (see cache.go's fire).
func (c *capturedLogs) errorAboutPanic(t *testing.T, wantItem string, wantWatcher int, wantPanic string) {
	t.Helper()
	for _, r := range c.records {
		if r.Level != slog.LevelError {
			continue
		}
		var item string
		var watcher int
		var panicked string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "item":
				if s, ok := a.Value.Any().(string); ok {
					item = s
				}
			case "watcher":
				if n, ok := a.Value.Any().(int64); ok {
					watcher = int(n)
				}
			case "panic":
				if s, ok := a.Value.Any().(string); ok {
					panicked = s
				}
			}
			return true
		})
		if item == wantItem && watcher == wantWatcher && panicked == wantPanic {
			return
		}
	}
	t.Fatalf("want an Error record naming item %q, watcher %d, panic %q; captured records: %v", wantItem, wantWatcher, wantPanic, c.records)
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

func TestService_TenantDuration_NoRowReportsUnconfigured(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	got, ok, err := svc.TenantDuration(context.Background(), "brand.welcome_interval", "tenant-a")
	if err != nil {
		t.Fatalf("TenantDuration: %v", err)
	}
	if ok || got != 0 {
		t.Fatalf("TenantDuration with no row = (%v, %v), want (0, false): the schema default must be reported as unconfigured, never echoed as a value", got, ok)
	}
}

func TestService_TenantDuration_ResolvesTenantThenSystemThenDefault(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.welcome_interval", Value{Data: 2 * time.Minute}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}

	// System row: every tenant sees it, reported as configured.
	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		got, ok, err := svc.TenantDuration(context.Background(), "brand.welcome_interval", tenant)
		if err != nil {
			t.Fatalf("TenantDuration under %s: %v", tenant, err)
		}
		if !ok || got != 2*time.Minute {
			t.Fatalf("TenantDuration under %s = (%v, %v), want (2m, true)", tenant, got, ok)
		}
	}

	// Tenant row: the override wins for that tenant only.
	if err := svc.Set(tenantA(), ScopeTenant, "brand.welcome_interval", Value{Data: 30 * time.Second}, "alice"); err != nil {
		t.Fatalf("tenant Set: %v", err)
	}
	if got, ok, err := svc.TenantDuration(context.Background(), "brand.welcome_interval", "tenant-a"); err != nil || !ok || got != 30*time.Second {
		t.Fatalf("TenantDuration under tenant-a = (%v, %v, %v), want (30s, true, nil)", got, ok, err)
	}
	if got, ok, err := svc.TenantDuration(context.Background(), "brand.welcome_interval", "tenant-b"); err != nil || !ok || got != 2*time.Minute {
		t.Fatalf("TenantDuration under tenant-b = (%v, %v, %v), want the system row (2m, true, nil)", got, ok, err)
	}
}

func TestService_TenantDuration_TakesTheTenantFromTheParameter(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "brand.welcome_interval", Value{Data: 30 * time.Second}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// tenant-a carries its own override in ctx, but the lookup asks for
	// tenant-b: the parameter decides, so the ctx tenant's row is invisible
	// and the answer falls through to "unconfigured".
	got, ok, err := svc.TenantDuration(tenantA(), "brand.welcome_interval", "tenant-b")
	if err != nil {
		t.Fatalf("TenantDuration: %v", err)
	}
	if ok || got != 0 {
		t.Fatalf("TenantDuration(ctx tenant-a, tenant-b) = (%v, %v), want (0, false): the parameter must override the ctx tenant", got, ok)
	}
}

func TestService_TenantDuration_RejectsWrongTypesAndUnknownKeys(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	if _, _, err := svc.TenantDuration(context.Background(), "brand.site_name", "tenant-a"); err == nil {
		t.Fatal("TenantDuration on a string item succeeded")
	} else {
		assertCode(t, err, ErrTypedValueMismatch)
	}
	if _, _, err := svc.TenantDuration(context.Background(), "brand.nonexistent", "tenant-a"); err == nil {
		t.Fatal("TenantDuration on an unknown key succeeded")
	} else {
		assertCode(t, err, ErrUnknownKey)
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

func TestService_Watch_RecoversPanickingCallbackAndLogsIt(t *testing.T) {
	// The regression for fire's containment: a host watcher that panics on
	// a change must surface as an Error-level log naming the item, the
	// callback's position in registration order and the panic value -- on
	// the publishing Set's own context logger, so tenant and trace
	// correlation survive -- while the watchers registered after it still
	// receive the value. A fire that recovered the panic into the blank
	// identifier would make a buggy host callback disappear on every change
	// with zero diagnostics, contradicting cache.go's claim of the same
	// logged robustness the in-memory bus gives its subscribers.
	svc := attachDefaultServiceForTest(t)
	logs := &capturedLogs{}
	ctx := obs.WithLogger(tenantA(), slog.New(logs))
	delivered := make(chan Value, 1)
	if err := svc.Watch("brand.site_name", func(Value) { panic("watch-one panicked") }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := svc.Watch("brand.site_name", func(v Value) { delivered <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := svc.Set(ctx, ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	logs.errorAboutPanic(t, "brand.site_name", 0, "watch-one panicked")
	select {
	case v := <-delivered:
		if v.Data != "Studio A" || v.Scope != ScopeTenant {
			t.Fatalf("delivery to the watcher after the panicking one = %+v, want the tenant-tier change value", v)
		}
	default:
		t.Fatal("the watcher registered after the panicking one must still receive the value")
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

// A diamond-shaped dependency graph -- two flags sharing one dependency --
// is legal: Attach's detectFlagCycles proves acyclicity over the whole
// graph, so the runtime walk must not re-report a shared dependency as a
// cycle when it is reachable through two chains. IsEnabled must answer true
// for every flag and EnabledFlags must not fail wholesale (it is what both
// features endpoints consume). Regression: a walk that marks a flag for
// the whole traversal instead of for its own subtree trips
// ErrFeatureFlagDependencyCycle on the second chain into a shared flag,
// failing every consumer of the flag surface on a legal declaration.
func TestService_IsEnabled_SharedDependencyThroughTwoChainsIsNotACycle(t *testing.T) {
	svc, _ := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), nil, []pkgcore.FeatureFlag{
		{Key: "billing.base", Default: true, Description: "Base billing, shared by the export and analytics chains"},
		{Key: "billing.export", Default: true, Description: "Exports invoices", DependsOn: []string{"billing.base"}},
		{Key: "billing.analytics", Default: true, Description: "Revenue analytics", DependsOn: []string{"billing.base"}},
		{Key: "billing.reporting", Default: true, Description: "The reporting suite", DependsOn: []string{"billing.export", "billing.analytics"}},
	})

	for key, want := range map[string]bool{
		"billing.base":      true,
		"billing.export":    true,
		"billing.analytics": true,
		"billing.reporting": true,
	} {
		got, err := svc.IsEnabled(tenantA(), key)
		if err != nil {
			t.Fatalf("IsEnabled(%s): %v", key, err)
		}
		if got != want {
			t.Fatalf("IsEnabled(%s) = %v, want %v", key, got, want)
		}
	}

	flags, err := svc.EnabledFlags(tenantA())
	if err != nil {
		t.Fatalf("EnabledFlags: %v", err)
	}
	wantFlags := []string{"billing.analytics", "billing.base", "billing.export", "billing.reporting"}
	if len(flags) != len(wantFlags) {
		t.Fatalf("EnabledFlags = %v, want %v", flags, wantFlags)
	}
	for i := range wantFlags {
		if flags[i] != wantFlags[i] {
			t.Fatalf("EnabledFlags = %v, want %v", flags, wantFlags)
		}
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

func TestService_PublicSnapshot_SkipsAnItemWithNoValueAnywhere(t *testing.T) {
	// A Public item declared without a Default is a legal declaration
	// (pkgcore's registration validation allows a nil Default for every
	// type: "the module serves no value until one is set"), so until a row
	// exists at some scope the item has no value to serve. The snapshot
	// must omit that key, never fail the whole response: the endpoint's
	// contract is platform-defaults fallback, never an error, and any
	// module declaring such an item must not take the pre-auth login
	// surface down with a 404 for every tenant while ops has not written
	// the row yet.
	items := []pkgcore.ConfigItem{
		{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
		{Key: "brand.support_phone", Type: "string", Public: true, Description: "The tenant's support phone", Group: "brand"},
		{Key: "support.reply_email", Type: "string", Sensitive: true, Description: "The address support replies come from", Group: "support"},
	}
	svc, _ := attachServiceForTest(t, openServiceTestDB(t), buildTestCipher(t), items, nil)

	values, features, err := svc.PublicSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PublicSnapshot with an unset Public item: %v", err)
	}
	if values["brand.site_name"] != "Smile Studio" {
		t.Fatalf("snapshot brand.site_name = %#v, want the schema default", values["brand.site_name"])
	}
	if _, present := values["brand.support_phone"]; present {
		t.Fatal("an item with no row and no default must be omitted from the snapshot, not present")
	}
	if len(features) != 0 {
		t.Fatalf("snapshot features = %v, want none", features)
	}

	// Once a row exists, the item joins the snapshot like any other.
	if err = svc.Set(systemWriteCtx(t), ScopeSystem, "brand.support_phone", Value{Data: "+1-555-0100"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	values, _, err = svc.PublicSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PublicSnapshot after the row landed: %v", err)
	}
	if values["brand.support_phone"] != "+1-555-0100" {
		t.Fatalf("snapshot brand.support_phone = %#v, want the written row", values["brand.support_phone"])
	}
}

func TestService_PublicSnapshot_SkipsAnItemWhoseStoredRowDoesNotDecode(t *testing.T) {
	// The tolerance the unset case above earns is owed to its sibling
	// bad-data shape: a stored row whose canonical text cannot be decoded
	// under the item's declared type. That corrupt row must not take the
	// pre-auth login surface down with an error for every tenant either --
	// the item is skipped, its key absent from the snapshot, with a Warn
	// naming it -- while a caller that must see the corruption reads the
	// item through Get, which still reports it. The row cannot be written
	// through Set (validation refuses a value that does not canonicalize),
	// so it is planted directly in the table, the way a writer that
	// bypassed Set would have left it.
	items := []pkgcore.ConfigItem{
		{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
		{Key: "billing.retry_limit", Type: "int", Default: int(3), Public: true, Description: "How many payment retries an invoice gets", Group: "billing"},
	}
	db := openServiceTestDB(t)
	svc, _ := attachServiceForTest(t, db, nil, items, nil)
	if err := db.Create(&row{
		Key:       "billing.retry_limit",
		Scope:     string(ScopeSystem),
		TenantID:  "",
		Value:     "not-an-int",
		UpdatedBy: "legacy-writer",
		UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("planting the corrupt row: %v", err)
	}

	logs := &capturedLogs{}
	ctx := obs.WithLogger(context.Background(), slog.New(logs))
	values, features, err := svc.PublicSnapshot(ctx)
	if err != nil {
		t.Fatalf("PublicSnapshot with an undecodable stored row: %v", err)
	}
	if _, present := values["billing.retry_limit"]; present {
		t.Fatal("an item whose stored row does not decode must be omitted from the snapshot, not present")
	}
	if values["brand.site_name"] != "Smile Studio" {
		t.Fatalf("snapshot brand.site_name = %#v, want the schema default", values["brand.site_name"])
	}
	if len(features) != 0 {
		t.Fatalf("snapshot features = %v, want none", features)
	}
	logs.warnedAbout(t, "billing.retry_limit")
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

// skewedWriteFixture seeds svc's cache with "Studio A" for
// brand.site_name/tenant-a, advances the watermark past that write via a
// real Refresh, then lands a second write behind the resulting watermark
// through a direct store.put -- standing in for a replica whose
// config.item.changed publish never arrived here (lost) and whose own
// clock sits behind the watermark this process already advanced to (a
// genuine cross-replica clock-skew scenario, since UpdatedAt is an
// application-supplied now() in service.go, never a database-generated
// value). It returns the watermark at the moment the skewed row landed, so
// a caller can reason about how many further Refresh cycles are needed.
func skewedWriteFixture(t *testing.T, svc *Service) {
	t.Helper()
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := svc.Get(tenantA(), "brand.site_name"); v.Data != "Studio A" {
		t.Fatalf("warm-up Get = %#v, want the cached value seeded", v.Data)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh (warm-up): %v", err)
	}
	watermarkAfterWarmup := svc.watermark
	// The warm-up Refresh's own changedSince(zero-value watermark) call
	// selects the row Set just wrote (every row is ">= the zero time"),
	// which invalidates -- not populates -- the cache entry: Refresh
	// invalidates unconditionally, whatever produced the watermark. Read
	// it back once so the cache is holding "Studio A" again, the value
	// that must stay stuck once the skewed write lands.
	if v, _ := svc.Get(tenantA(), "brand.site_name"); v.Data != "Studio A" {
		t.Fatalf("re-warm Get after the warm-up Refresh = %#v, want the cache repopulated with the old value", v.Data)
	}

	tSkewed := watermarkAfterWarmup.Add(-1 * time.Hour)
	if err := svc.st.put(context.Background(), row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Stuck Forever", UpdatedBy: "skewed-instance", UpdatedAt: tSkewed,
	}); err != nil {
		t.Fatalf("direct store.put (skewed): %v", err)
	}
}

// TestService_Refresh_IncrementalSweepAloneNeverRecoversASkewedWrite
// reproduces the mechanism-level gap: changedSince's
// ">= watermark" predicate (store.go) can never select a row whose
// UpdatedAt lands behind an already-advanced watermark, on any future
// Refresh call, however many -- the watermark only ever grows forward, so
// the incremental sweep by itself would leave that row invisible forever,
// not just until the next tick. This is true independent of Refresh's own
// periodic full-reconciliation fallback (fullReconcileEvery), which this
// test deliberately stays under (fullReconcileEvery-1 cycles) so it
// isolates the incremental sweep's own limit rather than the fallback that
// bounds it.
func TestService_Refresh_IncrementalSweepAloneNeverRecoversASkewedWrite(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	skewedWriteFixture(t, svc)

	// Repeated Refresh calls -- standing in for many poller ticks, but
	// deliberately fewer than fullReconcileEvery -- must never converge
	// through the incremental sweep alone: proving this once (the very
	// next Refresh) would leave open the possibility of an eventual,
	// merely delayed recovery from that same mechanism; proving it across
	// several iterations is what isolates "the incremental sweep itself
	// cannot do this" from "the fallback hasn't fired yet". skewedWriteFixture
	// already spent one Refresh cycle (the warm-up), so this loop stops two
	// short of fullReconcileEvery rather than one, to stay strictly under
	// the threshold where the periodic full reconciliation would fire.
	for i := 0; i < fullReconcileEvery-2; i++ {
		if err := svc.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh iteration %d: %v", i, err)
		}
		v, err := svc.Get(tenantA(), "brand.site_name")
		if err != nil {
			t.Fatalf("Get after Refresh iteration %d: %v", i, err)
		}
		if v.Data == "Stuck Forever" {
			t.Fatalf("iteration %d: the skewed-clock write converged through the incremental sweep alone", i)
		}
	}

	// The write genuinely landed in storage; only the incremental sweep
	// can never observe it, which is exactly the gap the periodic full
	// reconciliation (tested separately) exists to bound.
	stored, err := svc.st.get(context.Background(), ScopeTenant, "tenant-a", "brand.site_name")
	if err != nil {
		t.Fatalf("store.get: %v", err)
	}
	if stored == nil || stored.Value != "Stuck Forever" {
		t.Fatalf("stored row = %#v, want the skewed write to have actually landed in storage", stored)
	}
}

// TestService_Refresh_PeriodicFullReconciliation_RecoversASkewedWrite
// proves the bound the periodic reconciliation provides: Refresh's
// every-fullReconcileEvery-th full
// cache eviction (valueCache.invalidateAll) bounds how long the row
// TestService_Refresh_IncrementalSweepAloneNeverRecoversASkewedWrite
// proves the incremental sweep alone can never recover -- by the
// fullReconcileEvery-th Refresh call, the cache has been flushed at least
// once and the next read observes the row's true, current value.
func TestService_Refresh_PeriodicFullReconciliation_RecoversASkewedWrite(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	skewedWriteFixture(t, svc)

	for i := 0; i < fullReconcileEvery; i++ {
		if err := svc.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh iteration %d: %v", i, err)
		}
	}

	v, err := svc.Get(tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("Get after %d Refresh cycles: %v", fullReconcileEvery, err)
	}
	if v.Data != "Stuck Forever" {
		t.Fatalf("Get after %d Refresh cycles = %#v, want the periodic full reconciliation to have recovered the skewed write", fullReconcileEvery, v.Data)
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

// TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh is a
// deterministic regression test for the deadlock Close's design must
// avoid: Close must not hold pollMu across its block on <-pollDone. The
// poller's own ticker-triggered Refresh call needs pollMu to finish and
// let the poller's next select observe the closed pollStop -- so if the
// poller's select ever picked <-ticker.C over the already-closed
// <-pollStop while Close was mid-wait, the poller would block forever
// trying to re-acquire pollMu for that tick's Refresh call, and Close,
// still holding pollMu, would wait forever for a pollDone that could now
// never close. Circular wait. Close avoids it by releasing pollMu before
// the wait (see Close's doc comment); this test pins that the lock is
// never held across the wait.
//
// The one piece of that race this test cannot pin down directly is which
// case Go's select picks when both channels are simultaneously ready --
// that choice is a uniform, unforceable coin flip by language design.
// Everything else the deadlock needs is forced here, deterministically,
// via the afterRefreshLock hook rather than hoped for through timing: a
// poller-triggered Refresh call provably holding pollMu at the exact
// moment Close is invoked, and (through an extremely short poll interval)
// a second tick provably pending in the poller's ticker by the time that
// Refresh call returns. That leaves only the coin flip, so the scenario
// is repeated enough times that never landing the bad half of it is
// vanishingly unlikely against a Close that held the lock across the
// wait, while the Close under test cannot deadlock on either outcome of
// that flip -- every iteration passes. Each iteration carries its own
// hard wall-clock timeout, so a reappearing deadlock fails fast and
// names the iteration instead of hanging the whole test binary.
func TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh(t *testing.T) {
	const iterations = 25
	for i := 0; i < iterations; i++ {
		refreshLocked := make(chan struct{})
		proceedRefresh := make(chan struct{})
		var signalOnce sync.Once
		hook := func() {
			signalOnce.Do(func() { close(refreshLocked) })
			<-proceedRefresh
		}

		// The hook is installed before Attach starts the poller goroutine
		// (via the unexported withAfterRefreshLockForTest, forwarded onto
		// the Service before startPoller runs), never assigned onto the
		// Service after the fact -- the poller's first tick can otherwise
		// land before or during a post-construction assignment, racing it.
		svc := attachDefaultServiceForTest(t, WithPollInterval(200*time.Microsecond), withAfterRefreshLockForTest(hook))

		// Wait for the poller's own ticker-triggered Refresh call to
		// actually acquire pollMu -- the precondition a real deadlock
		// needs, forced here instead of hoped for.
		select {
		case <-refreshLocked:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the poller never entered Refresh within 5s", i)
		}

		closeDone := make(chan error, 1)
		go func() { closeDone <- svc.Close() }()

		// Give Close a moment to reach pollMu.Lock() and block there
		// behind the still-held Refresh call, and let the short poll
		// interval queue up a second, pending tick in the meantime.
		time.Sleep(20 * time.Millisecond)

		// Release the in-flight Refresh. Close releases pollMu before
		// waiting on pollDone, so a poller whose next select picks the
		// now-pending tick over the already-closed pollStop still finishes
		// that tick's Refresh -- it can re-acquire pollMu -- and reaches
		// the next loop iteration, where it observes the closed pollStop
		// and exits. Holding pollMu across the wait is exactly the
		// deadlock Close's doc comment in service.go rules out.
		close(proceedRefresh)

		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("iteration %d: Close: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Close deadlocked against an in-flight poller tick -- see Close's doc comment in service.go", i)
		}
	}
}

// TestService_Close_EveryConcurrentCallerWaitsForThePollerExit pins a
// per-caller guarantee of Close: two concurrent Close calls each observe
// the poller goroutine exited, not just the caller that requested the
// stop. Two properties of Close hold that guarantee together: it releases
// pollMu before waiting on pollDone (a poller-triggered Refresh needs that
// same mutex to finish and let the poller observe the closed pollStop --
// see TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh),
// and it never clears pollDone, so a Close arriving while an earlier one
// still waits on <-pollDone still finds a terminal signal to wait on
// instead of seeing a nil pollStop and returning with the poller possibly
// still running. The Service stays usable after Close (direct reads and
// writes keep working), so an early return would otherwise be invisible in
// the module's own tests; but a host that shuts the poller down while
// draining request traffic relies on Close's return meaning the background
// goroutine is gone before it tears down whatever that goroutine reads
// from.
//
// Close leaves pollDone in place as the terminal signal every caller
// waits on; only pollStop is closed and cleared, by whichever caller finds
// it non-nil. This test pins the per-caller guarantee: each Close
// result, as it arrives, must find the poller's done channel already
// closed. The choreography makes a second Close provably race a still-alive
// poller, the only situation an early return could surface in: the
// poller is parked inside a ticker-triggered Refresh (the afterRefreshLock
// hook, holding pollMu), both Close calls queue behind it, and the release
// is timed so the poller -- which owns the CPU the instant the parked
// Refresh returns, before either queued Close can be scheduled -- selects
// the provably pending second tick and re-enters another Refresh. A second
// Close returning while the poller is still queued inside that Refresh
// would violate the per-caller guarantee; instead it waits on pollDone
// like the first caller, and only the poller's own exit releases both. The
// one piece this cannot pin down is the rare case where the first Close is
// scheduled before the poller's post-release select and the poller's select
// then picks the closed stop over the pending tick -- an unforceable coin
// flip by language design -- so the scenario is repeated enough times that
// never once landing in the failing half is vanishingly unlikely, while
// the implementation passes every iteration deterministically.
func TestService_Close_EveryConcurrentCallerWaitsForThePollerExit(t *testing.T) {
	const iterations = 25
	for i := 0; i < iterations; i++ {
		refreshLocked := make(chan struct{}, 1)
		proceedRefresh := make(chan struct{})
		hook := func() {
			select {
			case refreshLocked <- struct{}{}:
			default:
			}
			<-proceedRefresh
		}
		svc := attachDefaultServiceForTest(t, WithPollInterval(200*time.Microsecond), withAfterRefreshLockForTest(hook))

		// Capture the poller's terminal signal before any Close runs: the
		// field has no concurrent writer at this instant (Close is its only
		// writer and none has been called yet), so the read needs no lock --
		// and a lock would in fact deadlock once the poller parks below,
		// holding pollMu for the whole park.
		done := svc.pollDone
		if done == nil {
			t.Fatalf("iteration %d: the service attached without a poller", i)
		}

		// Wait until the poller's own ticker-triggered Refresh holds pollMu,
		// parked on the hook: the precondition both Close calls queue behind.
		select {
		case <-refreshLocked:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the poller never entered Refresh within 5s", i)
		}

		closeResults := make(chan error, 2)
		go func() { closeResults <- svc.Close() }()
		go func() { closeResults <- svc.Close() }()

		// Give both Close calls time to queue on pollMu behind the parked
		// Refresh, and the short poll interval time to buffer a second,
		// pending tick -- the release choreography below needs both.
		time.Sleep(20 * time.Millisecond)
		close(proceedRefresh)

		// Every Close result, as it arrives, must find the poller already
		// exited -- a second caller returning while the poller is still
		// queued inside the Refresh it re-entered above would be the lost
		// guarantee.
		for received := 0; received < 2; received++ {
			select {
			case err := <-closeResults:
				if err != nil {
					t.Fatalf("iteration %d: Close: %v", i, err)
				}
				select {
				case <-done:
				default:
					t.Fatalf("iteration %d: a Close() returned while the poller goroutine was still running -- every Close caller must wait for the poller's exit, not just the one that closed the stop channel", i)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("iteration %d: a concurrent Close never returned within 5s", i)
			}
		}
	}
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

func TestService_Get_AnAbsentRowIsReadFromTheStoreOnceThenServedFromCache(t *testing.T) {
	// Negative-caching regression: resolving an absent key performs one
	// store lookup per consulted scope tier on the first read and caches the
	// confirmed absence, so the second read -- the shape every pre-auth
	// PathPublic and PathSystemFeatures request produces while no
	// override row exists, where falling back to platform defaults is the
	// normal answer, not an error path -- never touches the database again.
	// Without negative caching every read of an absent key would pay those
	// store lookups again; the query counter below (a gorm callback registered
	// before Attach, so the count is deterministic, -count=1) proves the
	// second read pays none. The multiplier stays fixed by the declared
	// schema: only the store rows the resolve walk consults are ever
	// counted, never a caller-supplied key list.
	db := openServiceTestDB(t)
	var reads atomic.Int64
	if err := db.Callback().Query().Before("gorm:query").Register("config:test:count-row-reads", func(*gorm.DB) { reads.Add(1) }); err != nil {
		t.Fatalf("registering the row-read counter: %v", err)
	}
	svc, _ := attachServiceForTest(t, db, buildTestCipher(t), serviceTestSchemaItems, serviceTestSchemaFlags)

	// billing.retry_limit has a declared Default but no row at any scope, so
	// a tenant read falls through the tenant tier and the system tier to the
	// default: two store lookups on the first read, none on the second.
	ctx := tenantA()
	if v, err := GetTyped[int64](svc, ctx, "billing.retry_limit"); err != nil {
		t.Fatalf("first Get: %v", err)
	} else if v != 3 {
		t.Fatalf("first Get = %d, want the schema default 3", v)
	}
	if reads.Load() == 0 {
		t.Fatal("the first read of an absent key must consult the store")
	}
	afterFirst := reads.Load()

	if v, err := GetTyped[int64](svc, ctx, "billing.retry_limit"); err != nil {
		t.Fatalf("second Get: %v", err)
	} else if v != 3 {
		t.Fatalf("second Get = %d, want the schema default 3", v)
	}
	if got := reads.Load(); got != afterFirst {
		t.Fatalf("a repeated read of an absent key performed %d more store lookups -- the confirmed absence is not cached (got %d reads, want %d)", got-afterFirst, got, afterFirst)
	}

	// A Set that creates the row must supersede the cached absence: the
	// writer's own put lands at the exact triple the sentinel occupied, so
	// the very next read serves the new value.
	if err := svc.Set(ctx, ScopeTenant, "billing.retry_limit", Value{Data: int64(9)}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := GetTyped[int64](svc, ctx, "billing.retry_limit"); err != nil {
		t.Fatalf("Get after Set: %v", err)
	} else if v != 9 {
		t.Fatalf("Get after Set = %d, want the row Set just wrote -- the cached absence outlived the row's creation", v)
	}
}

func TestService_RemoteDelivery_InvalidatesFromTheWireMap(t *testing.T) {
	// Regression test for the cross-replica delivery path: pkgcore's
	// distributed bus reconstructs a remote event's payload as the JSON
	// decoded map (encoding/json into interface{}), never as the concrete
	// ItemChangedEvent. The subscriber must read that shape -- a subscriber
	// that dropped it would leave a replica with a warm cache serving the
	// stale value until the anti-loss poller happened to sweep it, making
	// the event path dead in the very deployment mode it exists for.
	svc := attachDefaultServiceForTest(t)
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Warm the cache, then write the new value behind the service's back --
	// the way another replica's Set lands on the shared table.
	if name, err := GetTyped[string](svc, tenantA(), "brand.site_name"); err != nil || name != "Studio A" {
		t.Fatalf("warm read = %q, %v", name, err)
	}
	now := time.Now().Truncate(time.Second).UTC()
	if err := svc.st.put(context.Background(), row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Studio A2", UpdatedBy: "carol", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("direct store.put (the remote write): %v", err)
	}

	// Deliver the change exactly as the distributed bus would: the struct
	// marshaled to JSON and decoded into any.
	raw, err := json.Marshal(ItemChangedEvent{
		Key: "brand.site_name", Scope: ScopeTenant, TenantID: "tenant-a",
		Actor: "carol", OldValue: "Studio A", NewValue: "Studio A2",
		ChangedAt: now,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var wire any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("json.Unmarshal into any: %v", err)
	}

	delivered := make(chan Value, 1)
	if err = svc.Watch("brand.site_name", func(v Value) { delivered <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err = svc.onItemChanged(context.Background(), pkgcore.Event{
		Type: EventConfigItemChanged, TenantID: "tenant-a", Payload: wire,
	}); err != nil {
		t.Fatalf("onItemChanged on a remote-shaped payload: %v", err)
	}

	// The cache entry the stale read warmed must be gone: the next read
	// re-resolves the row and serves the remote write. (The store.put above
	// wrote a canonical value directly; a GetTyped re-reads it fresh.)
	name, err := GetTyped[string](svc, tenantA(), "brand.site_name")
	if err != nil {
		t.Fatalf("re-read after remote delivery: %v", err)
	}
	if name != "Studio A2" {
		t.Fatalf("re-read after remote delivery = %q, want %q -- the remote event did not invalidate the cache", name, "Studio A2")
	}
	select {
	case v := <-delivered:
		if v.Data != "Studio A2" || v.Scope != ScopeTenant {
			t.Fatalf("remote watch delivery = %+v, want the remote change value", v)
		}
	default:
		t.Fatal("no watch delivery from the remote-shaped event")
	}
}

func TestService_RemoteDelivery_JudgesSensitivityByTheLocalSchema(t *testing.T) {
	// The subscriber must classify a changed item as sensitive from its
	// own frozen schema, never from the payload's Sensitive flag alone:
	// the publisher classifies from its own schema copy, and during a
	// rolling upgrade the copies disagree. An old copy that still believes
	// a key plaintext can publish the key's real value in the clear with
	// Sensitive:false; a copy that already believes the key Sensitive must
	// not decode that wire value and hand it to its watchers as plaintext.
	// The local judgment wins -- the delivery is redacted, the wire value
	// is never decoded -- and the divergence itself is Warned, because it
	// means the replicas' schema snapshots have drifted apart. (The decode
	// decision follows the payload's Sensitive flag nowhere: a skewed peer
	// publishing a key's clear-text value with Sensitive:false must not
	// reach watchers labeled Redacted:false, which a payload-side judgment
	// would allow.)
	svc := attachDefaultServiceForTest(t)

	// deliver sends one change through the subscriber exactly as the
	// distributed bus would: the struct marshaled to JSON and decoded into
	// any, so the payload travels through itemChangedFromJSONMap.
	deliver := func(t *testing.T, ctx context.Context, key, newValue string, payloadSensitive bool) {
		t.Helper()
		now := time.Now().Truncate(time.Second).UTC()
		raw, err := json.Marshal(ItemChangedEvent{
			Key: key, Scope: ScopeTenant, TenantID: "tenant-a",
			Actor: "carol", OldValue: "", NewValue: newValue,
			Sensitive: payloadSensitive, ChangedAt: now,
		})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var wire any
		if err = json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("json.Unmarshal into any: %v", err)
		}
		if err := svc.onItemChanged(ctx, pkgcore.Event{
			Type: EventConfigItemChanged, TenantID: "tenant-a", Payload: wire,
		}); err != nil {
			t.Fatalf("onItemChanged: %v", err)
		}
	}

	// Leg 1, the rolling-skew shape: support.reply_email is Sensitive in
	// this process's schema, but the remote change claims Sensitive:false
	// and carries a value in the clear. The local judgment wins: the
	// watcher receives a redacted delivery and the wire value never
	// becomes Data, and the divergence is logged as a Warn naming the key.
	const leaked = "ops-leak@example.com"
	logsSkew := &capturedLogs{}
	ctxSkew := obs.WithLogger(context.Background(), slog.New(logsSkew))
	deliveredSkew := make(chan Value, 1)
	if err := svc.Watch("support.reply_email", func(v Value) { deliveredSkew <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	deliver(t, ctxSkew, "support.reply_email", leaked, false)
	select {
	case v := <-deliveredSkew:
		if !v.Redacted || v.Data != nil {
			t.Fatalf("skewed delivery = %+v, want Redacted:true with Data nil -- the wire value %q must not reach watchers as plaintext", v, leaked)
		}
		if v.Scope != ScopeTenant {
			t.Fatalf("skewed delivery scope = %q, want %q", v.Scope, ScopeTenant)
		}
	default:
		t.Fatal("no watch delivery for the skewed change")
	}
	logsSkew.warnedAbout(t, "support.reply_email")

	// Leg 2, agreement on sensitive: the remote agrees the key is
	// Sensitive and redacts itself; the delivery stays redacted and no
	// divergence Warn fires.
	logsAgreeSensitive := &capturedLogs{}
	ctxAgreeSensitive := obs.WithLogger(context.Background(), slog.New(logsAgreeSensitive))
	deliveredAgreeSensitive := make(chan Value, 1)
	if err := svc.Watch("support.reply_email", func(v Value) { deliveredAgreeSensitive <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	deliver(t, ctxAgreeSensitive, "support.reply_email", redactedMarker, true)
	select {
	case v := <-deliveredAgreeSensitive:
		if !v.Redacted || v.Data != nil {
			t.Fatalf("sensitive agreement delivery = %+v, want a redacted value (Data nil)", v)
		}
	default:
		t.Fatal("no watch delivery for the sensitive agreement change")
	}
	logsAgreeSensitive.noWarn(t)

	// Leg 3, agreement on plaintext: a key this process's schema marks
	// non-sensitive arrives claimed non-sensitive; the value decodes into
	// Data, Redacted stays false and no Warn fires.
	logsAgreePlain := &capturedLogs{}
	ctxAgreePlain := obs.WithLogger(context.Background(), slog.New(logsAgreePlain))
	deliveredAgreePlain := make(chan Value, 1)
	if err := svc.Watch("brand.site_name", func(v Value) { deliveredAgreePlain <- v }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	deliver(t, ctxAgreePlain, "brand.site_name", "Studio A2", false)
	select {
	case v := <-deliveredAgreePlain:
		if v.Redacted || v.Data != "Studio A2" {
			t.Fatalf("plain agreement delivery = %+v, want the plaintext value decoded", v)
		}
	default:
		t.Fatal("no watch delivery for the plain agreement change")
	}
	logsAgreePlain.noWarn(t)
}

func TestService_ConcurrentReadBackfill_NeverOutlivesTheWritersInvalidate(t *testing.T) {
	// The read-through backfill (resolveRow) must not plant a value a
	// concurrent Set already superseded: a reader whose store read completed
	// before a write could otherwise land its pre-write backfill after the
	// Set's own cache put and its invalidate, leaving the older value
	// cached -- and served -- until the next event for that key or the
	// periodic full reconciliation evicted it. There is no poller here
	// (WithPollInterval(0)), so nothing but the write path could heal such a
	// cache.
	//
	// After the writer's last Set has returned, the final value is served
	// deterministically: a reader backfill that began before that write
	// either read the final row or was dropped by the generation guard
	// (valueCache.putIfUnchanged -- see its doc comment for the exact
	// invariant), so no pre-write value can be cached past the write's own
	// invalidate. The precise interleaving is exercised deterministically
	// by the valueCache-level tests in cache_test.go; this test drives the
	// same invariant through the composed service under real concurrency.
	svc := attachDefaultServiceForTest(t)
	const key = "brand.site_name"
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, key, Value{Data: "v0"}, "ops-1"); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = svc.Get(context.Background(), key)
				}
			}
		}()
	}

	const writes = 200
	for i := 1; i <= writes; i++ {
		if err := svc.Set(systemWriteCtx(t), ScopeSystem, key, Value{Data: fmt.Sprintf("v%d", i)}, "ops-1"); err != nil {
			t.Fatalf("Set v%d: %v", i, err)
		}
	}
	close(stop)
	readers.Wait()

	v, err := svc.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get after the writer finished: %v", err)
	}
	if v.Data != fmt.Sprintf("v%d", writes) {
		t.Fatalf("Get after the writer finished = %#v, want v%d (a read-through backfill outlived the last write's invalidate)",
			v.Data, writes)
	}
}
