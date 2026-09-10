package app

// objectstore_preset.go holds the "objectstore" seam's S3 preset entry:
// the host-side composition of ServerConfig's APP_S3_* fields that
// BuildServer installs on the preset when S3Endpoint names a complete
// composition.

import (
	"strconv"

	"github.com/vislake/speed/go/pkgcore"
)

// s3ObjectStoreSeamPreset builds the "objectstore" seam's preset entry for
// the configured APP_S3_* composition: the registered "objectstore.s3"
// implementation with every configured value carried under the key names
// that registration reads (pkgcore/objectstore/s3's objectStoreFromConfig:
// endpoint, bucket, access_key, secret_key, region, use_ssl spelled with
// strconv.FormatBool, and bucket_lookup, whose value the registration
// parses and refuses when unrecognized).
func s3ObjectStoreSeamPreset(cfg ServerConfig) pkgcore.SeamPreset {
	return pkgcore.SeamPreset{
		Implementation: "objectstore.s3",
		Config: pkgcore.Config{
			"endpoint":      cfg.S3Endpoint,
			"bucket":        cfg.S3Bucket,
			"access_key":    cfg.S3AccessKey,
			"secret_key":    cfg.S3SecretKey,
			"region":        cfg.S3Region,
			"use_ssl":       strconv.FormatBool(cfg.S3UseSSL),
			"bucket_lookup": cfg.S3BucketLookup,
		},
	}
}
