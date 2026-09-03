//go:build integration

package storage_test

// This file is the PostgreSQL leg of the storage module's integration
// tier (go test -tags=integration ./... from the module directory, the
// exact invocation pr-full.yml's integration-tiers job runs). The unit
// tier proves the module against SQLite; this leg re-proves what the
// second dialect can break.
//
// Two properties are discharged here that a SQLite-only suite cannot:
//
//   - The module's postgres/*.sql migration files apply from zero against
//     a real PostgreSQL server. testutil.NewPostgres starts the server
//     through dbkit's dbtest helper (skipping when no Docker daemon is
//     reachable) and applies the identical files every deployment runs,
//     through the real dbkit.MigrationRegistry -- the same shape as the
//     unit tier's NewSQLite, with the dialect swapped.
//
//   - The tenant-scoped repositories' isolation proof holds on the second
//     dialect: tenancytest.AssertIsolated re-runs against real PostgreSQL
//     over both repositories, with fixtures filling every NOT NULL column
//     of objects and object_derivatives exactly as the unit tier's do
//     (repository_test.go), so a dialect-specific hole in the GORM
//     tenant-scope plugin -- a cast, a collation, a nullability rule --
//     cannot hide behind SQLite's lenience.
//
// The fixtures deliberately mirror the unit tier's construction rather
// than sharing it: the unit fixtures are unexported in package storage,
// and this package is external (storage_test), so a dialect-identity
// proof would be weaker for importing them. Object keys are built with
// the exported ObjectKey/DerivativeKey grammar helpers rather than the
// unit tier's string literals, exercising the real key construction the
// services use.

import (
	"fmt"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/storage/internal/testutil"
	"github.com/vislake/speed/go/storage/migrations"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// isolationUUID returns a distinct, UUID-shaped id for index n, mirroring
// the unit tier's helper of the same name so the two tiers' fixtures stay
// comparable row for row.
func isolationUUID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// newUploadRow builds one Object in ObjectStateUploading, the state an
// upload transaction opens with, filling every NOT NULL column of the
// objects table -- the mirror of the unit tier's newUpload, with the key
// built by the real ObjectKey grammar.
func newUploadRow(id string, tenant pkgcore.TenantID, createdAt time.Time) storage.Object {
	key, err := storage.ObjectKey(tenant, id)
	if err != nil {
		panic(fmt.Sprintf("ObjectKey(%q, %q): %v", tenant, id, err))
	}
	return storage.Object{
		TenantModel:      dbkit.TenantModel{TenantID: string(tenant)},
		ID:               id,
		Key:              key,
		State:            storage.ObjectStateUploading,
		DeclaredSize:     1024,
		DeclaredType:     "image/png",
		DeclaredChecksum: "",
		UploadExpiresAt:  createdAt.Add(30 * time.Minute),
		CreatedAt:        createdAt,
	}
}

// TestObjectRepository_AssertIsolated_Postgres re-runs the shared isolation
// suite against Object on real PostgreSQL: the mandatory proof that objects
// are tenant data whose rows can neither be read, updated nor deleted
// across tenants holds on the second dialect too.
func TestObjectRepository_AssertIsolated_Postgres(t *testing.T) {
	db := testutil.NewPostgres(t, "storage", migrations.FS)
	repo := storage.NewObjectRepository(db)

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *storage.Object {
		n++
		upload := newUploadRow(isolationUUID(n), tenant, time.Now())
		return &upload
	})
}

// TestDerivativeRepository_AssertIsolated_Postgres re-runs the shared
// isolation suite against ObjectDerivative on real PostgreSQL. Derivatives
// are tenant data exactly like the objects they derive from -- the unique
// index protecting one object's thumbnail set is (tenant_id, object_id,
// kind), tenant first -- and the proof that an index or scope rule holds
// identically on both dialects is a real-server question.
func TestDerivativeRepository_AssertIsolated_Postgres(t *testing.T) {
	db := testutil.NewPostgres(t, "storage", migrations.FS)
	repo := storage.NewDerivativeRepository(db)

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *storage.ObjectDerivative {
		n++
		parent := isolationUUID(n)
		key, err := storage.DerivativeKey(tenant, parent, storage.DerivativeKindThumbnail)
		if err != nil {
			t.Fatalf("DerivativeKey(%q, %q): %v", tenant, parent, err)
		}
		return &storage.ObjectDerivative{
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			ID:          isolationUUID(100 + n),
			ObjectID:    parent,
			Kind:        storage.DerivativeKindThumbnail,
			Key:         key,
			MIME:        "image/jpeg",
			Size:        4096,
		}
	})
}
