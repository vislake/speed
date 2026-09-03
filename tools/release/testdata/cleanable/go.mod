// A publishable go/ module in the pre-release transition state: its
// replaces are all the sibling form (=> ../<own dir>) that the first real
// release deletes, in single-line and block shapes, plus a third-party
// fork pin that is never planned for removal.
module github.com/vislake/speed/go/jobs

go 1.25.0

replace github.com/vislake/speed/go/pkgcore => ../pkgcore

replace (
	github.com/vislake/speed/go/tenancy => ../tenancy
	github.com/vislake/speed/go/observability => ../observability
)

replace github.com/vislake/speed/go/dbkit => ../dbkit

// Third-party fork pin: a replace directive, but never transitional --
// the cleanup leaves it alone.
replace github.com/someone/forked-lib => github.com/someone/upstream v1.2.3

require (
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
)
