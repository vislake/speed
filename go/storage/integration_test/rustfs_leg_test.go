//go:build integration

package storage_test

// This file is the RustFS/S3 leg of the storage module's integration tier:
// the module's full object lifecycle driven against a real S3-compatible
// object store, through the objectstore/s3 subpackage's NewObjectStore, the
// implementation the distributed deployment mode composes. It exists
// because the seam's own contract is proven in that subpackage's own
// integration tier (go/pkgcore/objectstore/s3/integration_test); what
// needs a real store here is the module's use of the seam -- that the
// bytes an uploader streams really land under the object's key on an S3
// server, and that a delete really removes them from it. The metadata
// half lives in SQLite (testutil.NewSQLite); the store half is the point
// of this leg.
//
// Every assertion about the store's physical contents is made through a
// raw minio-go client, never through the module's own read paths, so
// nothing the module believes about its writes goes unchecked: a PutObject
// that only pretended to write would pass an OpenContent round-trip but
// fail the raw client's byte-for-byte check here. minio-go itself stays
// the client library (it speaks plain S3 API, not anything MinIO-specific);
// only the server this leg spins up changed, from MinIO to RustFS
// (https://github.com/rustfs/rustfs).
//
// The composition under test is the ordinary one for a deployment that
// keeps its metadata in-process and its bytes in real object storage: a
// standalone-mode kernel (the module's own register/bootstrap path, its
// unit tier's exact recipe) whose ObjectStore the host overrides with the
// S3-backed implementation via WithObjectStore -- the same injectable
// seam a distributed-mode host wires, exercised against real RustFS.
//
// Lifecycle coverage in one pass: nothing exists under the object's key
// before the upload, the streamed bytes appear under it exactly as sent
// (size and content), Complete's revalidation pipeline reads them back
// and finalizes the row with the declared digest intact (the test JPEG
// carries no metadata, so sanitize leaves the bytes untouched and the
// finalized checksum must equal the declared one), OpenContent returns
// the same bytes, and LifecycleService.Delete removes both the row and
// the bytes -- the raw client reports the key gone afterwards, and the
// module reports storage.object_not_found for the deleted id.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/storage/internal/testutil"
	"github.com/vislake/speed/go/storage/migrations"
)

// rustfsImage pins the RustFS image this leg runs against to the same
// release tag pkgcore's own ObjectStore tier uses, so both tiers exercise
// the same server behavior. RustFS (https://github.com/rustfs/rustfs) has
// no dedicated testcontainers-go module the way MinIO did (tcminio), so
// the container is started through testcontainers' own generic
// ContainerRequest instead -- see startRustfsStore below.
const rustfsImage = "rustfs/rustfs:1.0.0-rc.5"

// noopQueue is the do-nothing jobs.Queue this leg's module gets, mirroring
// the unit tier's stubQueue: the module requires that a queue EXISTS for
// Register, and Complete enqueues a thumbnail-derive task this leg never
// drains (the recording would be empty either way -- nothing here runs a
// worker). Register-only wiring, exactly like the unit tier.
type noopQueue struct{}

func (noopQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (noopQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (noopQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

// compile-time check that noopQueue satisfies jobs.Queue.
var _ jobs.Queue = noopQueue{}

// startRustfsStore starts a disposable RustFS container, creates a fresh
// bucket on it, and returns an S3-backed ObjectStore pointed at that
// bucket, the raw client (for the independent assertions this file's
// header promises) and the bucket name. The container is terminated via
// t.Cleanup on test completion, pass or fail. The bucket is created here
// rather than by the store: provisioning a bucket is a hosting operation,
// and s3.NewObjectStore deliberately never provisions its own. The shape
// mirrors go/pkgcore/objectstore/s3/integration_test's own
// startRustfsObjectStore helper; the raw client is the addition this leg
// needs.
func startRustfsStore(t *testing.T, ctx context.Context) (pkgcore.ObjectStore, *minio.Client, string) {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        rustfsImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"RUSTFS_ACCESS_KEY":     "rustfsadmin",
			"RUSTFS_SECRET_KEY":     "rustfsadmin",
			"RUSTFS_ADDRESS":        ":9000",
			"RUSTFS_CONSOLE_ENABLE": "false",
		},
		Cmd:        []string{"/data"},
		WaitingFor: wait.ForHTTP("/health").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start rustfs testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate rustfs testcontainer: %v", terminateErr)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("rustfs testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("rustfs testcontainer mapped port: %v", err)
	}
	endpoint := fmt.Sprintf("%s:%s", host, mappedPort.Port())

	const bucket = "objects"
	const accessKey = "rustfsadmin"
	const secretKey = "rustfsadmin"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio-go client for %q: %v", endpoint, err)
	}
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the rustfs testcontainer: %v", bucket, err)
	}

	return s3.NewObjectStore(s3.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	}), client, bucket
}

// TestObjectLifecycle_RoundTripsThroughS3 drives one object through the
// module's full lifecycle against real RustFS, asserting at each step on
// what the raw client can see of the store.
func TestObjectLifecycle_RoundTripsThroughS3(t *testing.T) {
	ctx := context.Background()
	store, client, bucket := startRustfsStore(t, ctx)

	db := testutil.NewSQLite(t, "storage", migrations.FS)
	module := storage.NewModule(db, storage.WithQueue(noopQueue{}))
	if _, err := pkgcore.NewKernel(pkgcore.WithObjectStore(store,
		pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart)).Bootstrap(ctx, module); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	tenant := pkgcore.TenantID("rustfs-leg-tenant")
	tctx := pkgcore.WithTenant(ctx, tenant)

	jpeg := testutil.JPEG(t, 64, 64)
	digest := sha256Hex(jpeg)

	svc := module.ObjectService()
	life := module.LifecycleService()

	created, err := svc.Create(tctx, storage.CreateParams{
		DeclaredSize:     int64(len(jpeg)),
		DeclaredType:     "image/jpeg",
		DeclaredChecksum: digest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.State != storage.ObjectStateUploading {
		t.Fatalf("state after Create = %q, want %q", created.State, storage.ObjectStateUploading)
	}
	objectKey, err := storage.ObjectKey(tenant, created.ID)
	if err != nil {
		t.Fatalf("ObjectKey(%q, %q): %v", tenant, created.ID, err)
	}
	assertS3Missing(t, client, bucket, objectKey)

	length := int64(len(jpeg))
	if err := svc.Upload(tctx, created.ID, &length, bytes.NewReader(jpeg)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// The streamed bytes must exist under the object's key on the real
	// store -- same size, same content -- before any completion runs.
	assertS3Bytes(t, client, bucket, objectKey, jpeg)

	completed, err := svc.Complete(tctx, created.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.State != storage.ObjectStateCompleted {
		t.Fatalf("state after Complete = %q, want %q", completed.State, storage.ObjectStateCompleted)
	}
	if completed.Size == nil || *completed.Size != int64(len(jpeg)) {
		t.Errorf("finalized size = %v, want %d", completed.Size, len(jpeg))
	}
	if completed.MIME == nil || *completed.MIME != "image/jpeg" {
		t.Errorf("finalized mime = %v, want image/jpeg", completed.MIME)
	}
	// The test JPEG carries no metadata, so Complete's sanitize pass
	// leaves the bytes untouched: the finalized digest must equal the
	// checksum declared at create time, byte for byte.
	if completed.ChecksumSHA256 == nil || *completed.ChecksumSHA256 != digest {
		t.Errorf("finalized checksum = %v, want %s", completed.ChecksumSHA256, digest)
	}
	if completed.Width == nil || *completed.Width != 64 || completed.Height == nil || *completed.Height != 64 {
		t.Errorf("finalized dimensions = %vx%v, want 64x64", completed.Width, completed.Height)
	}

	obj, rc, err := svc.OpenContent(tctx, created.ID)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read opened content: %v", err)
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close opened content: %v", closeErr)
	}
	if !bytes.Equal(got, jpeg) {
		t.Errorf("OpenContent returned %d bytes that differ from the uploaded %d", len(got), len(jpeg))
	}
	if obj.ID != created.ID {
		t.Errorf("OpenContent returned object %q, want %q", obj.ID, created.ID)
	}

	if err := life.Delete(tctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The delete protocol must remove the bytes from the real store, not
	// just the row: the raw client must report the key gone, and the
	// module must report the object gone. The code comparison follows the
	// module's own convention (its unit tier's assertCode): apperr errors
	// carry no errors.Is semantics, so a decorated instance -- Get decorates
	// with the id -- is matched by code, never by identity.
	assertS3Missing(t, client, bucket, objectKey)
	if _, err := svc.Get(tctx, created.ID); err == nil {
		t.Fatalf("Get after Delete succeeded, want code %q", storage.ErrObjectNotFound.Code)
	} else if !apperr.HasCode(err, storage.ErrObjectNotFound.Code) {
		t.Errorf("Get after Delete = %v, want code %q", err, storage.ErrObjectNotFound.Code)
	}
}

// sha256Hex returns the lowercase hex SHA-256 of b, the digest form the
// module's DeclaredChecksum and ChecksumSHA256 columns carry.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// assertS3Bytes fails t unless the store holds exactly want under key, and
// returns the stored bytes.
func assertS3Bytes(t *testing.T, client *minio.Client, bucket, key string, want []byte) {
	t.Helper()
	rc, err := client.GetObject(context.Background(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("raw GetObject(%q): %v", key, err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("raw read of %q: %v", key, err)
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("raw close of %q: %v", key, closeErr)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("raw store contents of %q: %d bytes, want %d (byte-for-byte equal)", key, len(got), len(want))
	}
}

// assertS3Missing fails t unless the store holds nothing under key.
func assertS3Missing(t *testing.T, client *minio.Client, bucket, key string) {
	t.Helper()
	_, err := client.StatObject(context.Background(), bucket, key, minio.StatObjectOptions{})
	if err == nil {
		t.Errorf("raw store still holds %q, want it gone", key)
		return
	}
	if resp := minio.ToErrorResponse(err); resp.Code != "NoSuchKey" {
		t.Errorf("raw StatObject(%q): %v, want NoSuchKey", key, err)
	}
}
