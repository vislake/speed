module example.com/smile-studio

go 1.25.0

// Speed modules are released in lockstep: every require below moves to one
// version together. The transition-state pin (v0.0.0-00010101000000-
// 000000000000) means the module is never fetched remotely; every speed
// module resolves through a replace directive to a local speed checkout,
// and a later `saasctl upgrade` run rewrites the whole block to one release
// version.
require (
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000 // indirect
	github.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/redis/go-redis/v9 v9.14.1 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	gorm.io/gorm v1.31.2 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)

// Replace directives survive an upgrade untouched: a consumer project keeps
// building against its own speed checkout until it is ready to pull the
// released modules from the module proxy.
replace github.com/vislake/speed/go/authn => /opt/speed/go/authn

replace github.com/vislake/speed/go/config => /opt/speed/go/config

replace github.com/vislake/speed/go/dbkit => /opt/speed/go/dbkit

replace github.com/vislake/speed/go/observability => /opt/speed/go/observability

replace github.com/vislake/speed/go/org => /opt/speed/go/org

replace github.com/vislake/speed/go/pkgcore => /opt/speed/go/pkgcore

replace github.com/vislake/speed/go/ratelimit => /opt/speed/go/ratelimit

replace github.com/vislake/speed/go/rbac => /opt/speed/go/rbac

replace github.com/vislake/speed/go/tenancy => /opt/speed/go/tenancy
