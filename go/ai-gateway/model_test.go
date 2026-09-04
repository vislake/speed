package aigateway

// Tests for model.go's credentialRow type: the table it names, and the
// tenancytest proof that ai_gateway_credentials is platform data -- the
// tenant-isolation plugin must never filter it, because a system-tier row
// must be visible regardless of which tenant (if any) is asking, exactly
// the same property go/config's own row type proves for its configs table.
//
// This file also carries the shared test-database helper every other test
// file in this package reuses (newTestDB) -- all same-package tests, so no
// separate internal/testutil package is needed for a helper nothing outside
// this package imports.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testCipherKey is the fixed 32-byte fixture key this file registers
// CredentialAPIKeySerializerName under, mirroring go/pki/repository_test.go's
// registerLocalKeySerializer pattern.
const testCipherKey = "0123456789abcdef0123456789abcdef"

var registerCredentialSerializerOnce sync.Once

// registerCredentialSerializer installs the CredentialAPIKeySerializerName
// GORM serializer once per test process -- the serializer registry is
// process-global, so the Once keeps repeated registrations from churning
// it; NewCipher can only fail on key length, and the fixture key above is
// fixed 32 bytes, so the panic branch is unreachable by construction.
func registerCredentialSerializer() {
	registerCredentialSerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(testCipherKey))
		if err != nil {
			panic(fmt.Sprintf("aigateway test: NewCipher on the fixed 32-byte fixture key: %v", err))
		}
		dbkit.RegisterEncryptedSerializer(CredentialAPIKeySerializerName, cipher)
	})
}

// testDBSeq numbers the in-memory SQLite databases this package's tests
// open, so parallel or repeated runs never share one.
var testDBSeq atomic.Int64

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero and CredentialAPIKeySerializerName
// registered.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	registerCredentialSerializer()

	dsn := fmt.Sprintf("file:aigateway_test_%d?mode=memory&cache=shared", testDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}

	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(NewModule(db)); err != nil {
		t.Fatalf("registering the ai-gateway migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the ai-gateway migrations: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestCredentialRow_NamesTheCredentialsTable(t *testing.T) {
	if want := "ai_gateway_credentials"; (credentialRow{}).TableName() != want {
		t.Fatalf("credentialRow.TableName() = %q, want %q", (credentialRow{}).TableName(), want)
	}
}

func TestCredentialRow_IsNotTenantScoped(t *testing.T) {
	// ai_gateway_credentials is platform data (docs/internal/04-data-and-
	// tenancy.md): a system-tier row must be readable regardless of which
	// tenant (if any) is asking. AssertNotTenantScoped proves the model
	// never implements dbkit.TenantScoped and that reads and writes behave
	// identically with any (or no) tenant in context.
	var created int
	createFn := func(db *gorm.DB) error {
		created++
		return db.Create(&credentialRow{
			Provider:  fmt.Sprintf("chat.provider-%d", created),
			Scope:     string(CredentialScopeSystem),
			TenantID:  "",
			APIKey:    "sk-test",
			BaseURL:   "https://example.test/v1",
			UpdatedAt: time.Now(),
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Model(&credentialRow{}).Count(&n).Error
		return n, err
	}

	tenancytest.AssertNotTenantScoped(t, newTestDB(t), credentialRow{}, createFn, findFn)
}
