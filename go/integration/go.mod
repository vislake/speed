module github.com/vislake/speed/go/integration

go 1.25.0

replace github.com/vislake/speed/go/pkgcore => ../pkgcore

replace github.com/vislake/speed/go/dbkit => ../dbkit

replace github.com/vislake/speed/go/tenancy => ../tenancy

replace github.com/vislake/speed/go/ratelimit => ../ratelimit

require (
	github.com/google/uuid v1.6.0
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
	gorm.io/datatypes v1.2.7
	gorm.io/gorm v1.31.2
)
