// Package s3 is the distributed deployment mode's ObjectStore, reaching any
// S3-compatible object service (MinIO, Aliyun OSS, AWS S3) through the
// minio-go client. It is split out of go/pkgcore's own package -- rather
// than living beside the ObjectStore interface as pkgcore.s3ObjectStore once
// did -- so that a consumer which never wires an S3-backed store does not
// inherit minio-go in its dependency graph: Go resolves dependencies per
// package, and an interface package that also carries one implementation
// hands every importer that implementation's whole dependency closure
// (the implementation-registry section of docs/internal/03-deployment-
// modes.md measures the cost and names the boundary).
//
// Importing this package registers "objectstore.s3" on pkgcore's shared
// ObjectStoreRegistry as a side effect (see register.go), the name
// pkgcore.PresetDistributed already names for the "objectstore" seam -- the
// same database/sql-style driver-registration pattern pkgcore's other
// built-in implementations use, now applied across a package boundary. A
// distributed-mode host that wants it either blank-imports this package
// (`import _ ".../pkgcore/objectstore/s3"`) so
// WithPreset(PresetDistributed) resolves it, or calls NewObjectStore
// directly and wires it with pkgcore.WithObjectStore.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/vislake/speed/go/pkgcore"
)

// Config names one bucket on an S3-compatible object service and the
// credentials that reach it. Endpoint is the service address as host or
// host:port, without a scheme: UseSSL decides whether the store talks plain
// HTTP or HTTPS to it, and a real distributed-mode host sets UseSSL for
// anything beyond a local MinIO. Bucket must already exist: creating buckets
// is a hosting operation, not a store operation, so the store never
// provisions its own bucket. Region is the signing region; MinIO ignores it,
// while AWS S3 and Aliyun OSS require the region their bucket lives in for
// the request signature to validate, so a host pointed at either sets it.
type Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

// objectStore is the distributed deployment mode's ObjectStore: an
// S3-compatible object service (MinIO, Aliyun OSS or AWS S3) reached through
// the minio-go client, one bucket serving the whole store. minio-go speaks
// the S3 API dialect all three services accept, and this wrapper keeps the
// seam's semantics: objects are streams, keys follow the shared grammar, and
// the S3 store's own error vocabulary is mapped onto the interface's
// sentinels so callers never see a backend-specific error.
//
// PutObject hands minio-go a stream of unknown length, which it uploads as a
// multipart upload with bounded memory; overwrites are atomic on the service,
// matching the local store's rename. DeleteObject relies on the S3 DELETE
// being idempotent -- removing a key that has no object succeeds -- and maps
// a stray NoSuchKey onto that same success. GetObject issues the request
// before returning, because minio-go defers it until the first read: a
// missing key is then reported by GetObject itself as
// pkgcore.ErrObjectNotFound, the same timing the local store's open gives,
// not on some later read.
type objectStore struct {
	client *minio.Client
	bucket string
}

// NewObjectStore returns the distributed deployment mode's pkgcore.ObjectStore,
// storing objects in cfg.Bucket on the S3-compatible service at cfg.Endpoint.
// Nothing is dialed here: the client is assembled in process and the service
// is contacted on the first operation, so constructing a store never blocks
// and never fails on a service that is down, and a store wired at startup
// works whether or not the service is reachable yet. An unusable
// configuration (an empty endpoint, bucket or credential) panics instead,
// because it is an unrecoverable wiring error at startup, the same failure
// mode pkgcore's SMTP mailer uses for an unusable configuration. The caller
// never imports minio-go: the store builds its own client and the minio
// types never cross this seam.
func NewObjectStore(cfg Config) pkgcore.ObjectStore {
	if cfg.Endpoint == "" {
		panic("pkgcore/objectstore/s3: NewObjectStore requires a non-empty Config.Endpoint")
	}
	if cfg.Bucket == "" {
		panic("pkgcore/objectstore/s3: NewObjectStore requires a non-empty Config.Bucket")
	}
	if cfg.AccessKey == "" {
		panic("pkgcore/objectstore/s3: NewObjectStore requires a non-empty Config.AccessKey")
	}
	if cfg.SecretKey == "" {
		panic("pkgcore/objectstore/s3: NewObjectStore requires a non-empty Config.SecretKey")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		panic(fmt.Sprintf("pkgcore/objectstore/s3: NewObjectStore: %v", err))
	}
	return &objectStore{client: client, bucket: cfg.Bucket}
}

// PutObject implements pkgcore.ObjectStore.PutObject by streaming r to the
// service. The size of r is unknown, so minio-go uploads it as a multipart
// upload whose parts stream from r as they are read; memory use is bounded
// by the part buffer, never by the object's size, and the context is
// honoured throughout, so a cancelled upload stops and is aborted
// server-side.
func (s *objectStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pkgcore.ValidateObjectKey(key); err != nil {
		return err
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("pkgcore/objectstore/s3: put: %w", err)
	}
	return nil
}

// GetObject implements pkgcore.ObjectStore.GetObject. minio-go's GetObject
// defers the request until the object's first read or Stat call, so the
// store calls Stat before returning: the request goes out now, a missing
// key comes back now as pkgcore.ErrObjectNotFound, and the caller receives a
// reader that is already streaming the object's bytes, whose own reads fail
// with the context's error once that context is done. The returned reader is
// minio-go's own, and the caller closes it exactly as the interface
// requires.
func (s *objectStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := pkgcore.ValidateObjectKey(key); err != nil {
		return nil, err
	}
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("pkgcore/objectstore/s3: get: %w", err)
	}
	if _, err := object.Stat(); err != nil {
		// Close releases the request the failed Stat started; without it the
		// reader goroutine minio-go spun up for this object would keep
		// waiting for reads that will never come. The close is best-effort:
		// the Stat error is the failure this call reports, and nothing
		// downstream can act on a Close failure here.
		_ = object.Close()
		if isObjectNotFound(err) {
			return nil, pkgcore.ErrObjectNotFound
		}
		return nil, fmt.Errorf("pkgcore/objectstore/s3: get: %w", err)
	}
	return object, nil
}

// DeleteObject implements pkgcore.ObjectStore.DeleteObject. S3 DELETE is
// idempotent: removing a key that has no object is a 204 success on every
// compatible service, and a stray NoSuchKey (the odd gateway) is mapped onto
// the same success, keeping the interface's promise that deleting a missing
// key never fails.
func (s *objectStore) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pkgcore.ValidateObjectKey(key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if isObjectNotFound(err) {
			return nil
		}
		return fmt.Errorf("pkgcore/objectstore/s3: delete: %w", err)
	}
	return nil
}

// isObjectNotFound reports whether err is the S3 NoSuchKey error every
// compatible service uses for a missing object (MinIO and Aliyun OSS include
// AWS's code in their S3-mode responses). minio-go can wrap the error, so
// the check walks the chain.
func isObjectNotFound(err error) bool {
	var response minio.ErrorResponse
	return errors.As(err, &response) && response.Code == "NoSuchKey"
}
