//go:build integration

// Package integration_test holds go/integration's PostgreSQL integration
// tier: this round's own proof that WebhookSubscription's mark-delete/
// restore mechanism (migrations/{sqlite,postgres}/0004_add_soft_delete.sql)
// behaves identically on the engine whose collation and NULL handling
// genuinely differ from SQLite's -- the unit tier's SQLite-only proof
// (webhook_service_test.go's TestService_DeleteWebhookSubscription_
// MarksInsteadOfPhysicallyRemoving and TestService_
// RestoreWebhookSubscription_UndoesTheDelete) cannot rule out a
// PostgreSQL-specific mistake shipping unnoticed. It is physically separate
// from go/integration's unit tests (all of which live in package
// integration itself, one file per source file, per the backend coding
// standard's testing layout rule) and carries the "integration" build tag:
// a plain "go test ./..." never compiles or runs anything in this
// directory; it is invoked explicitly with
// "go test -tags=integration ./integration_test/...". This mirrors the
// identical convention of go/org, go/rbac, go/dbkit, go/jobs, go/pkgcore
// and go/config.
//
// This is go/integration's FIRST integration_test/ package at all --
// AGENTS.md's Known limitations previously recorded "no PostgreSQL
// integration tier" outright; this round adds the tier proactively for its
// own soft-delete round rather than deferring it, following go/rbac's own
// round's proactive precedent (go/org's equivalent round added its tier
// reactively, after a first review pass flagged the gap).
//
// Every test here spins up its own disposable container and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path, matching every other module's tier.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres so the dbkit.Open(DialectPostgres) call below
	// has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
)

// testWebhookCipherKey is the AES key WebhookSecretSerializerName is
// registered with, mirroring the unit tier's own testWebhookCipherKey
// (webhook_repository_test.go) -- a fixed, obviously-a-test key, distinct
// from any blind-index key this module might grow later per
// dbkit.NewCipher's own doc comment.
var testWebhookCipherKey = []byte("kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk")

// startPostgresContainer starts a disposable PostgreSQL 16 container for
// one test, already registered for termination via t.Cleanup. It follows
// go/rbac/integration_test's and go/org/integration_test's identical
// helper.
func startPostgresContainer(t *testing.T, ctx context.Context) *postgres.PostgresContainer {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("integration"),
		postgres.WithUsername("integration"),
		postgres.WithPassword("integration"),
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
		if terminateErr := testcontainers.TerminateContainer(pgContainer); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})
	return pgContainer
}

// openIntegrationPostgres opens the container's database through
// dbkit.Open -- the only sanctioned way to obtain a *gorm.DB -- registers
// WebhookSecretSerializerName (mandatory before opening a database this
// module's tables live in, per WebhookSubscription's own doc comment), and
// applies this module's own migrations to it with the dialect they ship
// for, exactly the way a host applies them at startup. Nothing here calls
// AutoMigrate; the versioned SQL under migrations/postgres, including this
// round's own 0004_add_soft_delete.sql, is what creates and alters every
// table, which is what makes this the zero-to-head proof root CLAUDE.md
// asks for on the second dialect.
func openIntegrationPostgres(t *testing.T, ctx context.Context, pgContainer *postgres.PostgresContainer) *gorm.DB {
	t.Helper()

	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(integration.WebhookSecretSerializerName, cipher)

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres): %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(integration.NewModule(db)); err != nil {
		t.Fatalf("registering the integration migrations: %v", err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
		t.Fatalf("applying the integration migrations on PostgreSQL: %v", err)
	}
	return db
}

// tenantContext is the context a host hands in after tenancy.Middleware
// has resolved the tenant.
func tenantContext(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// attachIntegrationService builds a fully Attach-ed Module/Service pair
// over db, with no EventMapping declared. This tier's own tests exercise
// Delete/Restore over rows seeded directly through SQL rather than through
// Service.CreateWebhookSubscription, because Create refuses any EventTypes
// selection naming a mapping nothing declared, and this test package
// cannot reach the unit tier's unexported withWebhookURLValidator override
// that would otherwise let it bypass ValidateWebhookURL's real SSRF
// refusal -- see the individual tests' own row-seeding SQL.
func attachIntegrationService(t *testing.T, db *gorm.DB) *integration.Service {
	t.Helper()

	m := integration.NewModule(db)
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return svc
}

// TestPostgres_Migrations_CreateEveryTableFromZero proves the second
// dialect's migration set -- 0001 through this round's own 0004 -- actually
// runs and produces every table this module's models are mapped onto,
// including the two soft-delete columns this round adds.
func TestPostgres_Migrations_CreateEveryTableFromZero(t *testing.T) {
	ctx := context.Background()
	db := openIntegrationPostgres(t, ctx, startPostgresContainer(t, ctx))

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	for _, table := range []string{"integration_api_keys", "integration_webhook_subscriptions", "integration_webhook_deliveries"} {
		var exists bool
		if err = sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists); err != nil {
			t.Fatalf("querying information_schema for %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q was not created by the PostgreSQL migration set", table)
		}
	}

	for _, column := range []string{"deleted_at", "deleted_by"} {
		var exists bool
		if err = sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'integration_webhook_subscriptions' AND column_name = $1)`,
			column).Scan(&exists); err != nil {
			t.Fatalf("querying information_schema for column %q: %v", column, err)
		}
		if !exists {
			t.Errorf("column %q was not added to integration_webhook_subscriptions by 0004_add_soft_delete.sql", column)
		}
	}
}

// TestPostgres_DeleteWebhookSubscription_MarksInsteadOfPhysicallyRemoving
// re-runs the unit tier's TestService_DeleteWebhookSubscription_
// MarksInsteadOfPhysicallyRemoving against a real PostgreSQL server:
// deleting a subscription must leave the row present, mark-deleted, never
// physically remove it.
func TestPostgres_DeleteWebhookSubscription_MarksInsteadOfPhysicallyRemoving(t *testing.T) {
	ctx := context.Background()
	db := openIntegrationPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachIntegrationService(t, db)

	seedWebhookSubscription(t, db, "sub-1", "tenant-a", true)

	if deleteErr := svc.DeleteWebhookSubscription(tenantContext("tenant-a"), "sub-1"); deleteErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", deleteErr)
	}

	sqlDB, dbErr := db.DB()
	if dbErr != nil {
		t.Fatalf("db.DB(): %v", dbErr)
	}
	var deletedAt *time.Time
	if scanErr := sqlDB.QueryRowContext(ctx,
		`SELECT deleted_at FROM integration_webhook_subscriptions WHERE id = 'sub-1'`).Scan(&deletedAt); scanErr != nil {
		t.Fatalf("reading the row back directly: %v", scanErr)
	}
	if deletedAt == nil {
		t.Fatal("deleted_at is NULL after Delete, want it set -- Delete must mark, never physically remove")
	}

	list, listErr := svc.ListWebhookSubscriptions(tenantContext("tenant-a"))
	if listErr != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", listErr)
	}
	if len(list) != 0 {
		t.Fatalf("len(list) = %d after delete, want 0", len(list))
	}
}

// TestPostgres_RestoreWebhookSubscription_UndoesTheDelete re-runs the unit
// tier's TestService_RestoreWebhookSubscription_UndoesTheDelete and
// TestService_RestoreWebhookSubscription_ForcesActiveFalse_
// EvenIfActiveAtDelete against a real PostgreSQL server: restore makes the
// row visible again, with its original URL intact and Active forced false
// regardless of the value it held at the moment of deletion.
func TestPostgres_RestoreWebhookSubscription_UndoesTheDelete(t *testing.T) {
	ctx := context.Background()
	db := openIntegrationPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachIntegrationService(t, db)

	// Seeded ACTIVE, on purpose: this is the exact scenario
	// RestoreWebhookSubscription's own doc comment argues about -- a
	// subscription that was live when it was deleted must not resume
	// fan-out just because it was restored.
	seedWebhookSubscription(t, db, "sub-1", "tenant-a", true)

	if deleteErr := svc.DeleteWebhookSubscription(tenantContext("tenant-a"), "sub-1"); deleteErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", deleteErr)
	}
	if restoreErr := svc.RestoreWebhookSubscription(tenantContext("tenant-a"), "sub-1"); restoreErr != nil {
		t.Fatalf("RestoreWebhookSubscription: %v", restoreErr)
	}

	list, err := svc.ListWebhookSubscriptions(tenantContext("tenant-a"))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d after restore, want exactly 1", len(list))
	}
	restored := list[0]
	if restored.ID != "sub-1" || restored.URL != "https://example.com/hook" {
		t.Errorf("restored = %+v, want the original id and URL intact", restored)
	}
	if restored.Active {
		t.Error("Active = true after Restore, want false -- Restore must always land a subscription paused, even one that was Active when deleted")
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var totalRows int64
	if scanErr := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM integration_webhook_subscriptions WHERE id = 'sub-1'`).Scan(&totalRows); scanErr != nil {
		t.Fatalf("counting rows: %v", scanErr)
	}
	if totalRows != 1 {
		t.Fatalf("total rows for sub-1 = %d, want exactly 1 -- delete-then-restore must never duplicate the row", totalRows)
	}
}

// TestPostgres_RestoreWebhookSubscription_LiveSubscription_ReturnsNotFound
// re-runs the unit tier's TestService_RestoreWebhookSubscription_
// LiveSubscription_ReturnsNotFound against a real PostgreSQL server: the
// collapsed not-found signal (never existed vs. exists but not deleted)
// must hold identically on both dialects.
func TestPostgres_RestoreWebhookSubscription_LiveSubscription_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	db := openIntegrationPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachIntegrationService(t, db)

	seedWebhookSubscription(t, db, "sub-1", "tenant-a", true)

	restoreErr := svc.RestoreWebhookSubscription(tenantContext("tenant-a"), "sub-1")
	if restoreErr == nil {
		t.Fatal("RestoreWebhookSubscription on a never-deleted subscription = nil, want an error")
	}
}

// seedWebhookSubscription creates one WebhookSubscription row directly
// through the exported WebhookSubscriptionRepository -- rather than a raw
// SQL INSERT -- specifically so GORM's own registered serializer encrypts
// Secret exactly as Service.CreateWebhookSubscription's own write does; a
// hand-written INSERT storing a plaintext string under the "secret" column
// would later fail to decrypt when Service reads the row back through the
// same serializer. This is this test package's own row-seeding helper, not
// Service.CreateWebhookSubscription's own validation path, because Create
// refuses any EventTypes selection this harness has not declared through
// WithEventMapping (see attachIntegrationService's own doc comment) --
// Delete/Restore's contract is about the row's state, not about how it
// came to exist, so seeding below it is a legitimate, minimal setup.
func seedWebhookSubscription(t *testing.T, db *gorm.DB, id string, tenant pkgcore.TenantID, active bool) {
	t.Helper()
	repo := integration.NewWebhookSubscriptionRepository(db)
	row := &integration.WebhookSubscription{
		ID:         id,
		URL:        "https://example.com/hook",
		EventTypes: datatypes.JSON([]byte("[]")),
		Secret:     "whsec_test",
		Active:     active,
		CreatedBy:  "user-1",
	}
	if err := repo.Create(tenantContext(tenant), row); err != nil {
		t.Fatalf("seeding a webhook subscription row: %v", err)
	}
}
