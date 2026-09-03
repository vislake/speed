module example.com/smile/cli-app

go 1.25.0

require (
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
	golang.org/x/mod v0.27.0 // indirect
)

replace github.com/vislake/speed/go/config => /some/checkout/go/config
