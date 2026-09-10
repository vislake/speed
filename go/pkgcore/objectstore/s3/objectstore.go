// Package s3 is the distributed deployment mode's ObjectStore, reaching any
// S3-compatible object service (MinIO, Aliyun OSS, AWS S3) through the
// minio-go client. It is split out of go/pkgcore's own package -- rather
// than living beside the ObjectStore interface in the pkgcore root -- so
// that a consumer which never wires an S3-backed store does not inherit
// minio-go in its dependency graph: Go resolves dependencies per package,
// and an interface package that also carries one implementation hands
// every importer that implementation's whole dependency closure.
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
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/vislake/speed/go/pkgcore"
)

// BucketLookupType selects how the store addresses its bucket on the
// endpoint. The zero value, BucketLookupAuto, is the default and derives
// the style from the endpoint and the bucket name.
type BucketLookupType int

const (
	// BucketLookupAuto derives the addressing style from the endpoint and
	// the bucket name, the convention every S3 client follows: an HTTPS
	// endpoint with a bucket name containing a dot addresses the bucket as
	// a path segment (host/bucket/key), because such a name as a host label
	// could not match a wildcard TLS certificate; an endpoint recognized as
	// Amazon S3, Google Cloud Storage or Alibaba OSS addresses it as the
	// host's first label (bucket.host/key); every other service -- a
	// self-hosted MinIO, RustFS or Ceph at any address -- is addressed
	// path-style, the one style all of them accept.
	BucketLookupAuto BucketLookupType = iota

	// BucketLookupPath addresses the bucket as a path segment:
	// host/bucket/key. It is the style to pin when the endpoint's wildcard
	// bucket hosts do not resolve -- a bare IP address, a host reachable
	// only under its own name -- or when the certificate covers the
	// endpoint alone.
	BucketLookupPath

	// BucketLookupVirtualHost addresses the bucket as the endpoint's first
	// host label: bucket.host/key. It is the style Amazon S3 and Aliyun OSS
	// expect, and pinning it requires the deployment's DNS and TLS
	// certificate to cover the per-bucket host names.
	BucketLookupVirtualHost
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
// BucketLookup pins how the bucket is addressed on the endpoint; the zero
// value applies the BucketLookupAuto convention.
type Config struct {
	Endpoint     string
	Bucket       string
	AccessKey    string
	SecretKey    string
	Region       string
	UseSSL       bool
	BucketLookup BucketLookupType
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
// configuration (an empty endpoint, bucket or credential, an unknown
// BucketLookup) panics instead, because it is an unrecoverable wiring error
// at startup, the same failure mode pkgcore's SMTP mailer uses for an
// unusable configuration. The caller never imports minio-go: the store
// builds its own client and the minio types never cross this seam.
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
	store, err := newObjectStore(cfg, nil)
	if err != nil {
		panic(fmt.Sprintf("pkgcore/objectstore/s3: NewObjectStore: %v", err))
	}
	return store
}

// newObjectStore assembles the store for an already field-checked cfg: it
// maps cfg.BucketLookup onto minio-go's own enum, builds the client that
// addresses the bucket in that style, and returns the store over it.
// transport, when non-nil, is the RoundTripper the client dials through --
// minio-go's default transport otherwise; production passes nil and the
// line-shape tests inject a stand-in that observes the URL a lookup mode
// produces without a service. An unknown BucketLookup or an endpoint
// minio-go itself rejects comes back as an error, which the two callers
// report in their own conventions: NewObjectStore's panic, FromConfig's
// returned error.
func newObjectStore(cfg Config, transport http.RoundTripper) (pkgcore.ObjectStore, error) {
	lookup, ok := minioBucketLookup(cfg.BucketLookup)
	if !ok {
		return nil, fmt.Errorf("unknown Config.BucketLookup %d: want BucketLookupAuto, BucketLookupPath or BucketLookupVirtualHost", cfg.BucketLookup)
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, err
	}
	return &objectStore{client: client, bucket: cfg.Bucket}, nil
}

// minioBucketLookup maps a BucketLookupType onto minio-go's own
// BucketLookupType, the client's addressing-style selector. ok is false for
// a value outside this package's enum, which newObjectStore reports as an
// error.
func minioBucketLookup(lookup BucketLookupType) (minio.BucketLookupType, bool) {
	switch lookup {
	case BucketLookupAuto:
		return minio.BucketLookupAuto, true
	case BucketLookupPath:
		return minio.BucketLookupPath, true
	case BucketLookupVirtualHost:
		return minio.BucketLookupDNS, true
	default:
		return minio.BucketLookupAuto, false
	}
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
