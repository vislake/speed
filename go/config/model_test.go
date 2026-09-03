package config

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// Tests for model.go's row type: the table it names, and the tenancytest
// proof that the configs table is platform data -- the tenant-isolation
// plugin must never filter it, because rows of one scope must be visible to
// every tenant the service resolves reads for.

// modelTestDBSeq numbers the in-memory SQLite databases this file's tests
// open, so parallel or repeated runs never share one.
var modelTestDBSeq atomic.Int64

func openModelTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:config_model_%d?mode=memory&cache=shared", modelTestDBSeq.Add(1))
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

func TestRow_NamesTheConfigsTable(t *testing.T) {
	if want := "configs"; (row{}).TableName() != want {
		t.Fatalf("row.TableName() = %q, want %q", (row{}).TableName(), want)
	}
}

func TestRow_IsNotTenantScoped(t *testing.T) {
	// The configs table is platform data (docs/internal/04-data-and-tenancy.
	// md): system rows are read by every tenant's resolution path, so a
	// tenant filter on this table would make configuration vanish per
	// tenant. AssertNotTenantScoped proves the model never implements
	// dbkit.TenantScoped and that reads and writes behave identically with
	// any (or no) tenant in context.
	//
	// createFn inserts a fresh row per call (the primary key would collide
	// on a second write of the same triple), findFn counts every row.
	var created int
	createFn := func(db *gorm.DB) error {
		created++
		return db.Create(&row{
			Key:       fmt.Sprintf("brand.site_%d", created),
			Scope:     string(ScopeSystem),
			TenantID:  "",
			Value:     "Smile Studio",
			UpdatedBy: "migration-test",
			UpdatedAt: time.Now(),
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Model(&row{}).Count(&n).Error
		return n, err
	}

	tenancytest.AssertNotTenantScoped(t, openModelTestDB(t), row{}, createFn, findFn)
}
