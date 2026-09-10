package app

// objectstore_preset_test.go pins the host-level S3 ObjectStore wiring: the
// preset entry BuildServer installs for a configured APP_S3_* composition
// carries exactly the configured values under the key names the
// "objectstore.s3" registration reads, and that entry resolves through the
// registration itself -- so a wrong key (a value the registration never
// sees) or a wrong value fails here, rather than silently falling back to
// the registration's defaults at boot.

import (
	"maps"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

func TestS3ObjectStoreSeamPreset_CarriesTheConfiguredValuesAndResolves(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want pkgcore.Config
	}{
		{
			name: "a fully configured composition",
			cfg: ServerConfig{
				S3Endpoint:     "minio.example.test:9000",
				S3Bucket:       "clinic-media",
				S3AccessKey:    "access-key",
				S3SecretKey:    "secret-key",
				S3Region:       "eu-central-1",
				S3UseSSL:       true,
				S3BucketLookup: "path",
			},
			want: pkgcore.Config{
				"endpoint":      "minio.example.test:9000",
				"bucket":        "clinic-media",
				"access_key":    "access-key",
				"secret_key":    "secret-key",
				"region":        "eu-central-1",
				"use_ssl":       "true",
				"bucket_lookup": "path",
			},
		},
		{
			name: "the unset optional fields stay empty strings",
			cfg: ServerConfig{
				S3Endpoint:  "minio.example.test:9000",
				S3Bucket:    "clinic-media",
				S3AccessKey: "access-key",
				S3SecretKey: "secret-key",
			},
			want: pkgcore.Config{
				"endpoint":      "minio.example.test:9000",
				"bucket":        "clinic-media",
				"access_key":    "access-key",
				"secret_key":    "secret-key",
				"region":        "",
				"use_ssl":       "false",
				"bucket_lookup": "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := s3ObjectStoreSeamPreset(tc.cfg)
			if entry.Implementation != "objectstore.s3" {
				t.Fatalf("Implementation = %q, want %q", entry.Implementation, "objectstore.s3")
			}
			if !maps.Equal(entry.Config, tc.want) {
				t.Errorf("preset Config = %v, want exactly the configured values under the registration's key names %v", entry.Config, tc.want)
			}
			store, capabilities, err := pkgcore.ObjectStoreRegistry.Build(entry.Implementation, entry.Config)
			if err != nil {
				t.Fatalf("the configured composition does not resolve through the %q registration: %v", entry.Implementation, err)
			}
			if store == nil {
				t.Error("resolution answered a nil ObjectStore")
			}
			if capabilities != objectstores3.Capabilities {
				t.Errorf("resolved capabilities = %v, want the registration's declared %v", capabilities, objectstores3.Capabilities)
			}
		})
	}
}

// TestS3ObjectStoreSeamPreset_UnrecognizedBucketLookupFailsResolution is the
// wrong-key half of the pin: the configured bucket_lookup value must reach
// the registration under the key it reads, so a value outside its accepted
// set fails resolution naming that key -- were the value dropped under a
// misspelled key, resolution would silently succeed on the default.
func TestS3ObjectStoreSeamPreset_UnrecognizedBucketLookupFailsResolution(t *testing.T) {
	cfg := ServerConfig{
		S3Endpoint:     "minio.example.test:9000",
		S3Bucket:       "clinic-media",
		S3AccessKey:    "access-key",
		S3SecretKey:    "secret-key",
		S3BucketLookup: "dns",
	}
	entry := s3ObjectStoreSeamPreset(cfg)
	_, _, err := pkgcore.ObjectStoreRegistry.Build(entry.Implementation, entry.Config)
	if err == nil {
		t.Fatal("a bucket_lookup outside the registration's accepted set resolved, want a refusal naming the key")
	}
	if !strings.Contains(err.Error(), "bucket_lookup") {
		t.Errorf("refusal %q does not name the bucket_lookup key", err)
	}
}
