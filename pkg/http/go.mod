module github.com/vislake/speed/pkg/http

go 1.26.0

// None of the pkg modules has a released version yet, so this module resolves
// them from the sibling checkouts. A replace directive in a dependency is
// ignored by consumers, so it affects this module's own standalone builds only.
replace github.com/vislake/speed/pkg/config => ../config

replace github.com/vislake/speed/pkg/core => ../core

replace github.com/vislake/speed/pkg/log => ../log

require (
	github.com/vislake/speed/pkg/config v0.0.0
	github.com/vislake/speed/pkg/core v0.0.0
	github.com/vislake/speed/pkg/log v0.0.0
)
