module github.com/vislake/speed/pkg/db

go 1.26.0

// Neither pkg module has a released version yet, so this module resolves both
// from the sibling checkouts. A replace directive in a dependency is ignored
// by consumers, so it affects this module's own standalone builds only.
replace github.com/vislake/speed/pkg/config => ../config

replace github.com/vislake/speed/pkg/core => ../core

require (
	github.com/vislake/speed/pkg/core v0.0.0
	gorm.io/gorm v1.31.2
)

require (
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	golang.org/x/text v0.20.0 // indirect
)
