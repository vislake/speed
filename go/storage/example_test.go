package storage_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/storage"
)

// Example walks the metadata journey this module ships: a host opens and
// migrates its own database connection, then drives the module's
// repositories -- declare an upload, finalize it into a completed object,
// attach a derived thumbnail, and read back the error shape of a miss. It
// is compiled and run by the module's unit suite, so the whole example is
// also a proof that the migration files apply from zero (AutoMigrate is
// banned) and that the exported surface is coherent end to end.
//
// The host's own ObjectStore never appears: this round owns metadata only,
// and the Key columns show exactly where the bytes would live without any
// of them ever crossing the module boundary.
func Example() {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme-dental"))

	// The host opens the connection, with dbkit's isolation plugin already
	// wired, and migrates it from zero with this module's versioned files.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:storage_example?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}
	m := storage.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(m); err != nil {
		panic(err)
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}

	// Declare an upload: an object in ObjectStateUploading whose bytes are
	// not there yet. The key the bytes will live under is internal to the
	// module's grammar and never surfaces through any API.
	const objectID = "7f0c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
	key, err := storage.ObjectKey(pkgcore.TenantID("acme-dental"), objectID)
	if err != nil {
		panic(err)
	}
	o := storage.Object{
		TenantModel:     dbkit.TenantModel{TenantID: "acme-dental"},
		ID:              objectID,
		Key:             key,
		State:           storage.ObjectStateUploading,
		DeclaredSize:    2 << 20,
		DeclaredType:    "image/png",
		UploadExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if err = m.Objects().Create(ctx, &o); err != nil {
		panic(err)
	}
	fmt.Println("declared state:", o.State)

	// The finalization pipeline has run: the row now carries the finalized
	// size, mime, digest and dimensions, and the object is readable.
	size := int64(2 << 20)
	mime := "image/png"
	checksum := strings.Repeat("0123456789abcdef", 4) // 64 lowercase hex chars
	width, height := 1200, 800
	o.State = storage.ObjectStateCompleted
	o.Size = &size
	o.MIME = &mime
	o.ChecksumSHA256 = &checksum
	o.Width = &width
	o.Height = &height
	if err = m.Objects().Update(ctx, &o); err != nil {
		panic(err)
	}
	got, err := m.Objects().FindByID(ctx, objectID)
	if err != nil {
		panic(err)
	}
	fmt.Println("finalized state:", got.State)

	// Derivation produced one thumbnail for the object.
	thumbKey, err := storage.DerivativeKey(pkgcore.TenantID("acme-dental"), objectID, storage.DerivativeKindThumbnail)
	if err != nil {
		panic(err)
	}
	derivative := storage.ObjectDerivative{
		TenantModel: dbkit.TenantModel{TenantID: "acme-dental"},
		ID:          "8a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
		ObjectID:    objectID,
		Kind:        storage.DerivativeKindThumbnail,
		Key:         thumbKey,
		MIME:        "image/jpeg",
		Size:        8192,
	}
	if err = m.Derivatives().Create(ctx, &derivative); err != nil {
		panic(err)
	}
	derivatives, err := m.Derivatives().List(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("derivatives:", len(derivatives))

	// A read of a row this tenant does not own reports the repository
	// layer's record-not-found shape, machine-matchable by its code.
	_, err = m.Objects().FindByID(ctx, "00000000-0000-4000-8000-000000000000")
	if appErr, ok := apperr.As(err); ok {
		fmt.Println("read:", appErr.Code)
		return
	}
	panic(err)

	// Output:
	// declared state: uploading
	// finalized state: completed
	// derivatives: 1
	// read: dbkit.record_not_found
}
