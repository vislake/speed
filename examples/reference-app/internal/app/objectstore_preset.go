package app

// objectstore_preset.go holds the "objectstore" seam's S3 configuration
// block: the composition values ServerConfig's APP_S3_* fields resolve into
// when S3Endpoint names a complete composition.

import (
	"github.com/vislake/speed/go/pkgcore"
)

// s3ObjectStoreConfig builds the "objectstore.s3" component's configuration
// block for the configured APP_S3_* composition: every configured value
// carried under the key names that component reads (pkgcore/objectstore/s3's
// own schema: endpoint, bucket, access_key, secret_key, region, use_ssl, and
// bucket_lookup, whose value the component parses and refuses when
// unrecognized). An empty bucket_lookup reads as the store's
// endpoint-derived auto default.
func s3ObjectStoreConfig(cfg ServerConfig) pkgcore.ComponentConfig {
	return pkgcore.ComponentConfig{}.
		With("endpoint", cfg.S3Endpoint).
		With("bucket", cfg.S3Bucket).
		With("access_key", cfg.S3AccessKey).
		With("secret_key", cfg.S3SecretKey).
		With("region", cfg.S3Region).
		With("use_ssl", cfg.S3UseSSL).
		With("bucket_lookup", cfg.S3BucketLookup)
}
