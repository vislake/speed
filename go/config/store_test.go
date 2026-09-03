package config

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Tests for store.go's row accessor over the configs table: the exact
// (key, scope, tenant) get, the primary-key upsert, and the inclusive
// watermark query the anti-loss poller is built on.

// storeTestDBSeq numbers the in-memory SQLite databases this file's tests
// open, so parallel or repeated runs never share one.
var storeTestDBSeq atomic.Int64

func openStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:config_store_%d?mode=memory&cache=shared", storeTestDBSeq.Add(1))
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

func TestStore_GetReturnsNilForAbsentRow(t *testing.T) {
	st := &store{db: openStoreTestDB(t)}
	got, err := st.get(context.Background(), ScopeSystem, "", "brand.site_name")
	if err != nil {
		t.Fatalf("store.get: %v", err)
	}
	if got != nil {
		t.Fatalf("store.get found an unexpected row: %+v", got)
	}
}

func TestStore_PutThenGetRoundTripsTheRow(t *testing.T) {
	st := &store{db: openStoreTestDB(t)}
	ctx := context.Background()
	at := time.Now().Truncate(time.Second).UTC()
	want := row{
		Key:       "brand.site_name",
		Scope:     "system",
		TenantID:  "",
		Value:     "Smile Studio",
		UpdatedBy: "ops-1",
		UpdatedAt: at,
	}
	if err := st.put(ctx, want); err != nil {
		t.Fatalf("store.put: %v", err)
	}

	got, err := st.get(ctx, ScopeSystem, "", "brand.site_name")
	if err != nil {
		t.Fatalf("store.get: %v", err)
	}
	if got == nil {
		t.Fatal("store.get found no row after put")
	}
	if *got != want {
		t.Fatalf("round-tripped row = %+v, want %+v", got, want)
	}
}

func TestStore_PutUpsertsOnThePrimaryKey(t *testing.T) {
	st := &store{db: openStoreTestDB(t)}
	ctx := context.Background()
	first := time.Now().Truncate(time.Second).UTC()
	if err := st.put(ctx, row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Studio A", UpdatedBy: "ops-1", UpdatedAt: first,
	}); err != nil {
		t.Fatalf("first store.put: %v", err)
	}

	second := first.Add(time.Second)
	if err := st.put(ctx, row{
		Key: "brand.site_name", Scope: "tenant", TenantID: "tenant-a",
		Value: "Studio A2", UpdatedBy: "ops-2", UpdatedAt: second,
	}); err != nil {
		t.Fatalf("second store.put: %v", err)
	}

	got, err := st.get(ctx, ScopeTenant, "tenant-a", "brand.site_name")
	if err != nil {
		t.Fatalf("store.get: %v", err)
	}
	if got == nil {
		t.Fatal("store.get found no row after the upsert")
	}
	if got.Value != "Studio A2" || got.UpdatedBy != "ops-2" || !got.UpdatedAt.Equal(second) {
		t.Fatalf("upsert did not replace the row's mutable columns: %+v", got)
	}

	// A row of the same key at a different scope or tenant is a different
	// row: the upsert must not have touched it (none exists) and must not
	// collide with the system tier later.
	if err = st.put(ctx, row{
		Key: "brand.site_name", Scope: "system", TenantID: "",
		Value: "Global Co", UpdatedBy: "ops-1", UpdatedAt: second,
	}); err != nil {
		t.Fatalf("system-tier store.put: %v", err)
	}
	systemRow, err := st.get(ctx, ScopeSystem, "", "brand.site_name")
	if err != nil || systemRow == nil {
		t.Fatalf("system row after upsert: row=%+v err=%v", systemRow, err)
	}
	tenantRow, err := st.get(ctx, ScopeTenant, "tenant-a", "brand.site_name")
	if err != nil || tenantRow == nil {
		t.Fatalf("tenant row after upsert: row=%+v err=%v", tenantRow, err)
	}
	if tenantRow.Value != "Studio A2" {
		t.Fatalf("the tenant upsert was disturbed by the system write: %+v", tenantRow)
	}
}

func TestStore_ChangedSinceIsInclusiveAtTheWatermark(t *testing.T) {
	st := &store{db: openStoreTestDB(t)}
	ctx := context.Background()
	base := time.Now().Truncate(time.Second).UTC()
	t1 := base.Add(-2 * time.Second)
	t2 := base.Add(-1 * time.Second)
	t3 := base

	// Write one row per instant, in ascending order.
	for i, at := range []time.Time{t1, t2, t3} {
		if err := st.put(ctx, row{
			Key: fmt.Sprintf("brand.site_%d", i), Scope: "system", TenantID: "",
			Value: "x", UpdatedBy: "ops", UpdatedAt: at,
		}); err != nil {
			t.Fatalf("store.put: %v", err)
		}
	}

	// Nothing is older than t1's instant; a zero watermark re-reads all.
	all, err := st.changedSince(ctx, time.Time{})
	if err != nil || len(all) != 3 {
		t.Fatalf("changedSince(zero) = %d rows, err %v; want all 3", len(all), err)
	}

	// The comparison is >= on purpose (see store.go's doc comment): rows at
	// the boundary instant itself must be re-read on every sweep.
	atT2, err := st.changedSince(ctx, t2)
	if err != nil {
		t.Fatalf("changedSince(t2): %v", err)
	}
	if len(atT2) != 2 {
		t.Fatalf("changedSince(t2) = %d rows, want 2 (the t2 and t3 rows; the boundary is inclusive)", len(atT2))
	}
	for _, r := range atT2 {
		if r.Key == "brand.site_0" {
			t.Fatal("changedSince(t2) returned a row written before the watermark")
		}
	}

	strict, err := st.changedSince(ctx, t3)
	if err != nil || len(strict) != 1 || strict[0].Key != "brand.site_2" {
		t.Fatalf("changedSince(t3) = %v rows (err %v), want exactly the t3 row", len(strict), err)
	}
}
