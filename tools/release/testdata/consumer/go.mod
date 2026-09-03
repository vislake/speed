// A consumer go.mod (examples/reference-app shape): its module is outside
// the publishable prefix, so the first-release cleanup leaves it entirely
// alone -- consumers keep their replaces after every release, whatever
// their shape.
module github.com/vislake/speed/examples/reference-app

go 1.25.0

replace github.com/vislake/speed/go/dbkit => ../../go/dbkit

replace (
	github.com/vislake/speed/go/observability => ../../go/observability
	github.com/vislake/speed/go/pkgcore => ../../go/pkgcore
	github.com/vislake/speed/go/tenancy => ../../go/tenancy
)

require (
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
)
