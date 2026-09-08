// Fixture for tools/semgrep_rules/snake-case-log-attribute-key.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import (
	"context"

	"github.com/vislake/speed/go/observability"
)

func badKeys(ctx context.Context, n int, err error) {
	log := observability.FromContext(ctx)
	log.Info("message", "JobID", n)         // fires: uppercase key
	log.Warn("message", "job-id", n)        // fires: hyphen key
	log.Error("message", "tenant id", n)    // fires: space key
	log.Debug("message", "attemptCount", n) // fires: camelCase key
	log.Info("message", "job_id", n)        // snake key stays silent
}
