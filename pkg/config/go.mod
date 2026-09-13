module github.com/vislake/speed/pkg/config

go 1.26.0

// pkg/core has no released version yet, so this module resolves it from the
// sibling checkout. A replace directive in a dependency is ignored by
// consumers, so it affects this module's own standalone builds only.
replace github.com/vislake/speed/pkg/core => ../core

require github.com/vislake/speed/pkg/core v0.0.0
