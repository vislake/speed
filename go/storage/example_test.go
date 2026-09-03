package storage_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
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
// The repository layer this example drives never touches the host's
// ObjectStore -- the Key columns show exactly where the bytes would live.
// ExampleObjectService below walks the transfer lifecycle that does move
// them.
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

// ExampleObjectService drives the transfer lifecycle the service round ships
// end to end, the way a host would: open and migrate the module's tables,
// construct the module with the queue Register requires, and bootstrap it
// through the real kernel so the service's host seams -- the object store the
// standalone deployment mode resolves -- are the real ones, not hand-wired
// fakes. A small image is declared, streamed into the store, run through the
// completion revalidation pipeline, and read back from the store. It is
// compiled and run by the module's unit suite, so the whole example is also a
// proof that the lifecycle works over a real, migrated connection and a real
// kernel bootstrap.
//
// The queue is stubbed because this example proves the transfer lifecycle,
// not derivation; the completion pipeline enqueues a derive task onto it and
// stands regardless.
func ExampleObjectService() {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme-dental"))

	// The host opens the connection, with dbkit's isolation plugin already
	// wired, and migrates it from zero with this module's versioned files.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:storage_service_example?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}
	m := storage.NewModule(db, storage.WithQueue(noopQueue{}))
	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(m); err != nil {
		panic(err)
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}
	// Bootstrap resolves the registry's standalone object store and event bus
	// and runs the module's Register, which hands the registry to the service.
	if _, err = pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(ctx, m); err != nil {
		panic(err)
	}

	// An 8x8 PNG, encoded deterministically so the example's output is stable.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(16 * x), uint8(16 * y), 128, 255})
		}
	}
	if err = png.Encode(&buf, img); err != nil {
		panic(err)
	}
	content := buf.Bytes()

	svc := m.ObjectService()

	// Declare the upload: the object is reserved in ObjectStateUploading
	// with the id and store key the eventual bytes will be checked against.
	row, err := svc.Create(ctx, storage.CreateParams{
		DeclaredSize: int64(len(content)),
		DeclaredType: "image/png",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("created state:", row.State)

	// Stream the bytes into the host's object store, then run the completion
	// pipeline: size and checksum reconciliation, the media-type probe, the
	// metadata strip, and the finalize that makes the object readable.
	if err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(content)); err != nil {
		panic(err)
	}
	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		panic(err)
	}
	fmt.Println("completed state:", completed.State)
	fmt.Println("finalized mime:", *completed.MIME)
	fmt.Println("finalized pixels:", *completed.Width, "x", *completed.Height)

	// Reads serve the stored bytes back through the same store.
	_, rc, err := svc.OpenContent(ctx, row.ID)
	if err != nil {
		panic(err)
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		panic(err)
	}
	if err = rc.Close(); err != nil {
		panic(err)
	}
	fmt.Println("read back:", bytes.Equal(raw, content))

	// Output:
	// created state: uploading
	// completed state: completed
	// finalized mime: image/png
	// finalized pixels: 8 x 8
	// read back: true
}

// ExampleLifecycleService walks the end of the object lifecycle the way a
// host drives it: an object is declared, uploaded and completed exactly as
// in ExampleObjectService, then deleted through the lifecycle service --
// after which every read surface sees nothing -- and an upload declared
// long ago whose window closed is reclaimed by the expiry sweep. It is
// compiled and run by the module's unit suite over the same real migration,
// real kernel bootstrap and real standalone object store as
// ExampleObjectService.
//
// Delete is idempotent end to end and the sweep is the periodic cleanup a
// host schedules per tenant (EnqueueExpirySweep puts it on the queue; here
// the sweep runs synchronously).
func ExampleLifecycleService() {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme-dental"))

	// The host opens the connection and migrates it from zero, then
	// bootstraps the module through the real kernel so the services' host
	// seams -- the standalone object store, the event bus -- are real.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:storage_lifecycle_example?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}
	m := storage.NewModule(db, storage.WithQueue(noopQueue{}))
	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(m); err != nil {
		panic(err)
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}
	if _, err = pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(ctx, m); err != nil {
		panic(err)
	}
	svc := m.ObjectService()
	life := m.LifecycleService()

	// An 8x8 PNG, encoded deterministically so the example's output is
	// stable, exactly as in ExampleObjectService.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(16 * x), uint8(16 * y), 128, 255})
		}
	}
	if err = png.Encode(&buf, img); err != nil {
		panic(err)
	}
	content := buf.Bytes()

	// Declare, stream and finalize the object.
	row, err := svc.Create(ctx, storage.CreateParams{
		DeclaredSize: int64(len(content)),
		DeclaredType: "image/png",
	})
	if err != nil {
		panic(err)
	}
	if err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(content)); err != nil {
		panic(err)
	}
	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		panic(err)
	}
	fmt.Println("completed state:", completed.State)

	// Delete it: the protocol marks, removes the bytes, removes the rows.
	// Reads of the object now report not-found.
	if err = life.Delete(ctx, row.ID); err != nil {
		panic(err)
	}
	if _, _, err = svc.OpenContent(ctx, row.ID); err != nil {
		if appErr, ok := apperr.As(err); ok {
			fmt.Println("read after delete:", appErr.Code)
		} else {
			panic(err)
		}
	} else {
		panic("the deleted object still reads back")
	}

	// An upload declared long ago, whose window closed: only the sweep
	// reclaims uploading rows, and only once they are past their window.
	const staleID = "9d0e1f2a-3b4c-4d5e-9f0a-1b2c3d4e5f60"
	staleKey, err := storage.ObjectKey(pkgcore.TenantID("acme-dental"), staleID)
	if err != nil {
		panic(err)
	}
	stale := storage.Object{
		TenantModel:     dbkit.TenantModel{TenantID: "acme-dental"},
		ID:              staleID,
		Key:             staleKey,
		State:           storage.ObjectStateUploading,
		DeclaredSize:    1 << 20,
		DeclaredType:    "image/png",
		UploadExpiresAt: time.Now().Add(-time.Hour),
	}
	if err = m.Objects().Create(ctx, &stale); err != nil {
		panic(err)
	}
	fmt.Println("stale upload state:", stale.State)
	if err = life.Sweep(ctx); err != nil {
		panic(err)
	}
	if _, err = m.Objects().FindByID(ctx, staleID); err != nil {
		if appErr, ok := apperr.As(err); ok {
			fmt.Println("after sweep:", appErr.Code)
		} else {
			panic(err)
		}
	} else {
		panic("the expired upload survived the sweep")
	}

	// Output:
	// completed state: completed
	// read after delete: storage.object_not_found
	// stale upload state: uploading
	// after sweep: dbkit.record_not_found
}

// noopQueue satisfies the jobs.Queue Module.Register requires (WithQueue).
// The transfer lifecycle never needs a real queue -- the completion pipeline
// enqueues the thumbnail-derive task onto it and stands regardless -- so the
// stub records nothing.
type noopQueue struct{}

func (noopQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (noopQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (noopQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

// compile-time check that noopQueue satisfies jobs.Queue.
var _ jobs.Queue = noopQueue{}
