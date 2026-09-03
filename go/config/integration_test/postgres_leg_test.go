//go:build integration

// Package config_test holds go/config's integration tier: the module's
// proof surface re-run against real servers -- PostgreSQL for the configs
// table semantics (migrations, upsert, the watermark query, encrypted
// values at rest) and Redis for the cross-replica hot-update path (the
// distributed bus's JSON map delivery and the cache invalidation that must
// follow it). It is physically separate from go/config's unit tests (all
// of which live in package config itself, one file per source file, per
// the backend coding standard's testing layout rule) and carries the
// "integration" build tag: a plain "go test ./..." never compiles or runs
// anything in this directory; it is invoked explicitly with "go test
// -tags=integration ./...". This mirrors the identical convention of
// go/dbkit/integration_test and go/jobs/integration_test.
//
// Every test here spins up its own disposable container and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path, matching the other modules' tiers.
package config_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// pgItems and pgFlags are the schema this tier's tests fold into their
// registries, mirroring the unit tier's fixtures: a public string with a
// default, a Sensitive string without one, an int with bounds, and the
// two-flag dependency chain. They are shared by the PostgreSQL and the
// Redis legs below.
var pgItems = []pkgcore.ConfigItem{
	{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
	{Key: "support.reply_email", Type: "string", Sensitive: true, Description: "The address support replies come from", Group: "support"},
	{Key: "billing.retry_limit", Type: "int", Default: int(3), Min: int(1), Max: int64(10), Description: "How many payment retries an invoice gets", Group: "billing"},
}

var pgFlags = []pkgcore.FeatureFlag{
	{Key: "ai.smile_preview", Default: false, Description: "Lets tenants try smile previews"},
	{Key: "ai.premium_upsell", Default: true, Description: "Shows the premium upsell", DependsOn: []string{"ai.smile_preview"}},
}

// pgCipher returns the AES-GCM cipher the tier's services are attached
// with, over a random 32-byte key. The test that decrypts a stored value
// holds the same instance the service holds, so decrypting is done through
// the host's own key rather than a second copy of it.
func pgCipher(t *testing.T) *dbkit.Cipher {
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

// startPostgresContainer starts a disposable PostgreSQL 16 container for
// one test, already registered for termination via t.Cleanup. It follows
// go/dbkit/integration_test's helper almost line for line.
func startPostgresContainer(t *testing.T, ctx context.Context) *postgres.PostgresContainer {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("config"),
		postgres.WithUsername("config"),
		postgres.WithPassword("config"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(pgContainer); err != nil {
			t.Errorf("terminate postgres testcontainer: %v", err)
		}
	})
	return pgContainer
}

// openConfigPostgres opens the container's database through dbkit.Open --
// the only sanctioned way to obtain a *gorm.DB -- and applies the config
// module's own migrations to it with the dialect they ship for, exactly
// the way a host applies them at startup. The returned *gorm.DB is what
// the tests hand to config.NewModule.
func openConfigPostgres(t *testing.T, ctx context.Context, pgContainer *postgres.PostgresContainer) *gorm.DB {
	t.Helper()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres): %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(config.NewModule(db)); err != nil {
		t.Fatalf("registering the config migrations: %v", err)
	}
	if err := migrations.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
		t.Fatalf("applying the config migrations on PostgreSQL: %v", err)
	}
	return db
}

// attachConfigService folds pgItems and pgFlags into a fresh registry over
// the given bus and returns the Service Attach produced. Register's one
// process-global side effect is replicated up front -- the module's system
// purpose is declared -- mirroring what a real Bootstrap performs before
// Attach, so systemWriteContext works even in a test that never registers
// a module (none do, but the helper stands alone).
func attachConfigService(t *testing.T, db *gorm.DB, bus pkgcore.EventBus, cipher *dbkit.Cipher) *config.Service {
	t.Helper()

	pkgcore.RegisterSystemPurpose(config.SystemPurposeSystemWrite)
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Config.Add(pgItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(pgFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	module := config.NewModule(db, config.WithCipher(cipher), config.WithPollInterval(0))
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return svc
}

// systemWriteContext returns a context whose system reason carries the
// module's own audited purpose, the entitlement a ScopeSystem write
// requires. Attach (via Register) made the purpose valid process-wide.
func systemWriteContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: config.SystemPurposeSystemWrite,
	})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	return ctx
}

func tenantContext(tenant string) context.Context {
	return pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenant))
}

// TestPostgres_Service_ServesTheScopeHierarchy is the service's scope
// contract on a real PostgreSQL server: the system row serves every
// tenant, a tenant override beats it for its own tenant only, a repeated
// Set upserts on the primary key instead of duplicating rows, and the
// feature-flag chain resolves per tenant.
func TestPostgres_Service_ServesTheScopeHierarchy(t *testing.T) {
	ctx := context.Background()
	db := openConfigPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachConfigService(t, db, pkgcore.NewMemoryEventBus(), pgCipher(t))

	if err := svc.Set(systemWriteContext(t), config.ScopeSystem, "brand.site_name", config.Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	// The second Set of the same triple must upsert, not duplicate: the
	// primary key is (key, scope, tenant_id) and the table is shared.
	if err := svc.Set(systemWriteContext(t), config.ScopeSystem, "brand.site_name", config.Value{Data: "Global Co 2"}, "ops-1"); err != nil {
		t.Fatalf("second system Set: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var rows int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM configs WHERE key = $1 AND scope = $2`,
		"brand.site_name", "system").Scan(&rows); err != nil {
		t.Fatalf("counting system rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the configs table holds %d system rows for brand.site_name after two Sets, want 1 (the upsert must replace, never insert)", rows)
	}

	for name, tenant := range map[string]string{"tenant-a": "tenant-a", "tenant-b": "tenant-b"} {
		v, err := svc.Get(tenantContext(tenant), "brand.site_name")
		if err != nil || v.Data != "Global Co 2" {
			t.Fatalf("%s reads brand.site_name = %+v, %v; want the system row", name, v, err)
		}
	}
	if err := svc.Set(tenantContext("tenant-a"), config.ScopeTenant, "brand.site_name", config.Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("tenant-a Set: %v", err)
	}
	if v, err := svc.Get(tenantContext("tenant-a"), "brand.site_name"); err != nil || v.Data != "Studio A" || v.Scope != config.ScopeTenant {
		t.Fatalf("tenant-a reads brand.site_name = %+v, %v; want its own override", v, err)
	}
	if v, err := svc.Get(tenantContext("tenant-b"), "brand.site_name"); err != nil || v.Data != "Global Co 2" {
		t.Fatalf("tenant-b reads brand.site_name = %+v, %v; the tenant-a override must stay isolated", v, err)
	}

	// The flag chain is off by default; tenant-a's own write turns it on
	// for tenant-a alone.
	if enabled, err := svc.IsEnabled(tenantContext("tenant-a"), "ai.premium_upsell"); err != nil || enabled {
		t.Fatalf("IsEnabled before the tenant write = %v, %v; want false", enabled, err)
	}
	if err := svc.Set(tenantContext("tenant-a"), config.ScopeTenant, "ai.smile_preview", config.Value{Data: true}, "alice"); err != nil {
		t.Fatalf("flag Set: %v", err)
	}
	if enabled, err := svc.IsEnabled(tenantContext("tenant-a"), "ai.premium_upsell"); err != nil || !enabled {
		t.Fatalf("IsEnabled after the tenant write = %v, %v; want true", enabled, err)
	}
	if enabled, err := svc.IsEnabled(tenantContext("tenant-b"), "ai.premium_upsell"); err != nil || enabled {
		t.Fatalf("tenant-b IsEnabled = %v, %v; the tenant-a flag write must not leak", enabled, err)
	}

	// Int bounds are enforced against the declared schema on the real
	// server too. The int item's value shape is int (not int64), exactly as
	// the unit tier pins it; 11 sits above the declared Max of 10.
	if err := svc.Set(systemWriteContext(t), config.ScopeSystem, "billing.retry_limit", config.Value{Data: 11}, "ops-1"); err == nil {
		t.Fatal("a Set above the declared Max succeeded; bounds must fail closed on PostgreSQL as on SQLite")
	}
}

// TestPostgres_SensitiveValue_LiesEncryptedAtRest proves the at-rest
// property on the real server: the row the service wrote for a Sensitive
// key holds base64 ciphertext, never the plaintext, and the host's cipher
// decrypts it back to exactly what Set was given -- while the service's
// own Get serves the decrypted value.
func TestPostgres_SensitiveValue_LiesEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	db := openConfigPostgres(t, ctx, startPostgresContainer(t, ctx))
	cipher := pgCipher(t)
	svc := attachConfigService(t, db, pkgcore.NewMemoryEventBus(), cipher)

	if err := svc.Set(tenantContext("tenant-a"), config.ScopeTenant, "support.reply_email", config.Value{Data: "ops@example.com"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	v, err := svc.Get(tenantContext("tenant-a"), "support.reply_email")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Data != "ops@example.com" || v.Redacted {
		t.Fatalf("an entitled Get must serve the clear value, got %+v", v)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var stored string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT value FROM configs WHERE key = $1 AND scope = $2 AND tenant_id = $3`,
		"support.reply_email", "tenant", "tenant-a").Scan(&stored); err != nil {
		t.Fatalf("reading the raw row: %v", err)
	}
	if stored == "ops@example.com" {
		t.Fatal("the configs row holds the plaintext; a Sensitive value must never be stored in the clear")
	}
	sealed, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		t.Fatalf("the stored value is not base64 ciphertext: %v", err)
	}
	plain, err := cipher.Decrypt(sealed)
	if err != nil {
		t.Fatalf("cipher.Decrypt over the stored value: %v", err)
	}
	if string(plain) != "ops@example.com" {
		t.Fatalf("decrypted stored value = %q, want the plaintext that was Set", plain)
	}
}

// TestPostgres_Refresh_HealsARowWrittenBehindTheService is the poller's
// query semantics on a real server: a row the service never wrote (here, a
// raw UPDATE, standing in for a replica that writes through a service of
// its own) is picked up by the watermark sweep -- Refresh is the poller's
// sweep without the ticker -- and the next read serves it.
func TestPostgres_Refresh_HealsARowWrittenBehindTheService(t *testing.T) {
	ctx := context.Background()
	db := openConfigPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachConfigService(t, db, pkgcore.NewMemoryEventBus(), pgCipher(t))

	if err := svc.Set(tenantContext("tenant-a"), config.ScopeTenant, "brand.site_name", config.Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := svc.Get(tenantContext("tenant-a"), "brand.site_name"); err != nil || v.Data != "Studio A" {
		t.Fatalf("warm read = %+v, %v", v, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE configs SET value = $1, updated_by = $2, updated_at = $3 WHERE key = $4 AND scope = $5 AND tenant_id = $6`,
		"Studio A2", "carol", time.Now().UTC(), "brand.site_name", "tenant", "tenant-a"); err != nil {
		t.Fatalf("raw UPDATE behind the service: %v", err)
	}

	// The stale cache still serves the old value until the sweep runs.
	if v, err := svc.Get(tenantContext("tenant-a"), "brand.site_name"); err != nil || v.Data != "Studio A" {
		t.Fatalf("pre-sweep read = %+v, %v; want the stale cached value", v, err)
	}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if v, err := svc.Get(tenantContext("tenant-a"), "brand.site_name"); err != nil || v.Data != "Studio A2" {
		t.Fatalf("post-sweep read = %+v, %v; Refresh must have invalidated the changed row", v, err)
	}
}

// TestPostgres_RawSQL_LocatesTheExactRow documents the row triple for the
// human debugging on a real server: the query above is what an operator
// inspecting the table would run, and it finds the one row the writes
// produced.
func TestPostgres_RawSQL_LocatesTheExactRow(t *testing.T) {
	ctx := context.Background()
	db := openConfigPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachConfigService(t, db, pkgcore.NewMemoryEventBus(), pgCipher(t))

	if err := svc.Set(tenantContext("tenant-a"), config.ScopeTenant, "brand.site_name", config.Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Set(systemWriteContext(t), config.ScopeSystem, "brand.site_name", config.Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM configs WHERE key = $1`, "brand.site_name").Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("the table holds %d rows for brand.site_name, want 2 (one system sentinel row, one tenant row)", n)
	}
}
