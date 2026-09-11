package s3

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "objectstore.s3" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"fmt"
	"github.com/vislake/speed/go/pkgcore"
	"strings"
)

// Capabilities is what "objectstore.s3" declares about itself: any number of
// replicas may address the same bucket, and the objects live inside the
// S3-compatible service, whose durable storage outlives any one process --
// the two claims the doc comment above spells out. The built-in registration
// below and the Registration factory a host wraps a self-built Config in
// both declare this one exported value, so the declaration a host reads off
// this package and the one assembly validates cannot drift apart. A host
// injecting a hand-built store with pkgcore.WithObjectStore passes it as the
// injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// parseBucketLookup maps a Config string onto a BucketLookupType, defaulting
// to BucketLookupAuto -- the same default the zero-value Config.BucketLookup
// carries -- for an unset value.
func parseBucketLookup(raw string) (BucketLookupType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return BucketLookupAuto, nil
	case "path":
		return BucketLookupPath, nil
	case "virtual_host":
		return BucketLookupVirtualHost, nil
	default:
		return 0, fmt.Errorf("invalid \"bucket_lookup\" %q: want one of \"auto\", \"path\", \"virtual_host\"", raw)
	}
}
