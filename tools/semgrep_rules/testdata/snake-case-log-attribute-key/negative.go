// Fixture for tools/semgrep_rules/snake-case-log-attribute-key.yml.
// Negative controls: must stay silent.
// This file is NOT shipped code -- it proves the rule stays silent.
package fixture

import (
	"context"

	"github.com/vislake/speed/go/observability"
)

func goodKeys(ctx context.Context, jobID string, n int) {
	log := observability.FromContext(ctx)
	log.Info("job succeeded", "job_id", jobID, "attempts", n, "duration_ms", 3)
	log.Warn("dotted config key stays snake per segment",
		"billing.stripe_secret_key", jobID)
	log.Error("literal value in value position is not a key", "outcome", "failed")
	log.Debug("single-pair tail", "attempts", n)
}
