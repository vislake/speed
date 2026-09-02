//go:build integration

package pkgcore_test

import (
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/vislake/speed/go/pkgcore"
)

// minioImage pins the MinIO image the ObjectStore integration tier runs
// against to the release tag testcontainers-go's own MinIO module tests use.
const minioImage = "minio/minio:RELEASE.2024-01-16T16-07-38Z"

// startMinioObjectStore starts a disposable MinIO container, creates a fresh
// bucket on it, and returns an S3-backed ObjectStore pointed at that bucket,
// the client and the container already cleaned up via t.Cleanup on test
// completion (pass or fail), so nothing leaks past its owning test. Every
// integration test calls this for its own container, keeping tests isolated
// from one another at the cost of a few seconds of startup each. The bucket
// is created here rather than by the store: provisioning a bucket is a
// hosting operation, and NewS3ObjectStore deliberately never provisions its
// own. MinIO's default root credentials, minioadmin/minioadmin, reach the
// bucket over the container's plain-HTTP endpoint.
func startMinioObjectStore(t *testing.T, ctx context.Context) pkgcore.ObjectStore {
	t.Helper()

	container, err := tcminio.Run(ctx, minioImage,
		tcminio.WithUsername("minioadmin"),
		tcminio.WithPassword("minioadmin"),
	)
	if err != nil {
		t.Fatalf("start minio testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate minio testcontainer: %v", terminateErr)
		}
	})

	endpoint, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio testcontainer connection string: %v", err)
	}

	const bucket = "objects"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(container.Username, container.Password, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio client for %q: %v", endpoint, err)
	}
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the minio testcontainer: %v", bucket, err)
	}

	return pkgcore.NewS3ObjectStore(pkgcore.S3Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: container.Username,
		SecretKey: container.Password,
	})
}
