package app

// objectstore_preset_test.go pins the host-level S3 ObjectStore wiring: the
// configuration block this host builds for a configured APP_S3_*
// composition carries exactly the configured values under the key names the
// "objectstore.s3" component reads, and those values resolve through the
// registered implementation itself -- so a wrong key (a value the
// registration never sees) or a wrong value fails here, rather than
// silently falling back to the registration's defaults at boot.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// s3BlockValues reads the block's entries into the value map the flat
// registration's Build call takes, encoding use_ssl the way both readers
// spell it.
func s3BlockValues(t *testing.T, block pkgcore.ComponentConfig) pkgcore.Config {
	t.Helper()
	values := make(pkgcore.Config, len(block.Keys()))
	for _, key := range block.Keys() {
		value, ok := block.Get(key)
		if !ok {
			t.Fatalf("block.Get(%q) reports no such key", key)
		}
		switch typed := value.(type) {
		case string:
			values[key] = typed
		case bool:
			values[key] = strconv.FormatBool(typed)
		default:
			t.Fatalf("block key %q carries a %T, want a string or a bool", key, value)
		}
	}
	return values
}

func TestS3ObjectStoreConfig_CarriesTheConfiguredValuesAndResolves(t *testing.T) {
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
			values := s3BlockValues(t, s3ObjectStoreConfig(tc.cfg))
			for key, want := range tc.want {
				if got := values[key]; got != want {
					t.Errorf("block key %q = %q, want %q", key, got, want)
				}
			}
			if len(values) != len(tc.want) {
				t.Errorf("block carries %d keys (%v), want exactly %d", len(values), values, len(tc.want))
			}
			store, capabilities, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", values)
			if err != nil {
				t.Fatalf("the configured composition does not resolve through the %q registration: %v", "objectstore.s3", err)
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

// TestS3ObjectStoreConfig_UnrecognizedBucketLookupFailsResolution is the
// wrong-key half of the pin: the configured bucket_lookup value must reach
// the registration under the key it reads, so a value outside its accepted
// set fails resolution naming that key -- were the value dropped under a
// misspelled key, resolution would silently succeed on the default.
func TestS3ObjectStoreConfig_UnrecognizedBucketLookupFailsResolution(t *testing.T) {
	cfg := ServerConfig{
		S3Endpoint:     "minio.example.test:9000",
		S3Bucket:       "clinic-media",
		S3AccessKey:    "access-key",
		S3SecretKey:    "secret-key",
		S3BucketLookup: "dns",
	}
	_, _, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", s3BlockValues(t, s3ObjectStoreConfig(cfg)))
	if err == nil {
		t.Fatal("a bucket_lookup outside the registration's accepted set resolved, want a refusal naming the key")
	}
	if !strings.Contains(err.Error(), "bucket_lookup") {
		t.Errorf("refusal %q does not name the bucket_lookup key", err)
	}
}
