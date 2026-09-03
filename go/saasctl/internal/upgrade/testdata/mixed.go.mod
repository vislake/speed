module example.com/smile-studio

go 1.25.0

// A consumer go.mod that drifted out of lockstep -- one require hand-edited
// to v0.1.0 while the others still carry the transition-state pin. The
// upgrade contract names this the one broken state the tool must heal and
// never produce.
require (
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000 // indirect
	github.com/vislake/speed/go/rbac v0.1.0
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
)

replace github.com/vislake/speed/go/rbac => /opt/speed/go/rbac
