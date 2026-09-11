package s3

// component.go registers the "objectstore.s3" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "objectstore" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// registration (register.go) keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// objectStoreS3Config is the "objectstore.s3" component's configuration
// schema: one field per key a composition block may carry, the same seven
// settings the flat seam Config adapter reads (UseSSL typed as a bool, the
// same value the flat "true" spelling carries).
type objectStoreS3Config struct {
	Endpoint     string `json:"endpoint"`
	Bucket       string `json:"bucket"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	Region       string `json:"region"`
	UseSSL       bool   `json:"use_ssl"`
	BucketLookup string `json:"bucket_lookup"`
}

// objectStoreS3Component is the component descriptor for "objectstore.s3":
// the S3-compatible store over the configuration the component's own block
// spells out, declaring the same exported Capabilities the seam
// registration declares. Its New funnels through parseBucketLookup and
// FromConfig, the same validation and construction the flat seam adapter
// uses -- the four required settings, the bucket-lookup enum and the
// scheme-less endpoint convention all come back as errors, never panics.
// The minio-go client exposes no close, so the component owns nothing
// closable and declares no Close, exactly as the seam registration records.
var objectStoreS3Component = pkgcore.Component{
	Name:         "objectstore.s3",
	Module:       "objectstore",
	Provides:     []any{(*pkgcore.ObjectStore)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*objectStoreS3Config)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c objectStoreS3Config
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		bucketLookup, err := parseBucketLookup(c.BucketLookup)
		if err != nil {
			return nil, err
		}
		store, _, err := FromConfig(Config{
			Endpoint:     c.Endpoint,
			Bucket:       c.Bucket,
			AccessKey:    c.AccessKey,
			SecretKey:    c.SecretKey,
			Region:       c.Region,
			UseSSL:       c.UseSSL,
			BucketLookup: bucketLookup,
		})
		if err != nil {
			return nil, err
		}
		return store, nil
	},
}

func init() { pkgcore.MustRegister(objectStoreS3Component) }
