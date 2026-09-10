module github.com/vislake/speed/go/app

go 1.26.0

// This module composes the platform's modules, so it requires the sibling
// modules whose types its exported surface names (Verifier, GrantLookup,
// the mounted-route type, the seam adapters' two ends). Their current API
// is not in a released version yet, so each resolves from the sibling
// checkout here. A replace directive in a dependency is ignored by
// consumers, so these affect this module's own standalone builds only.

replace github.com/vislake/speed/go/ai-gateway => ../ai-gateway

replace github.com/vislake/speed/go/authn => ../authn

replace github.com/vislake/speed/go/billing => ../billing

replace github.com/vislake/speed/go/config => ../config

replace github.com/vislake/speed/go/dbkit => ../dbkit

replace github.com/vislake/speed/go/jobs => ../jobs

replace github.com/vislake/speed/go/notification => ../notification

replace github.com/vislake/speed/go/ratelimit => ../ratelimit

replace github.com/vislake/speed/go/rbac => ../rbac

replace github.com/vislake/speed/go/storage => ../storage

replace github.com/vislake/speed/go/compliance => ../compliance

replace github.com/vislake/speed/go/metering => ../metering

replace github.com/vislake/speed/go/observability => ../observability

replace github.com/vislake/speed/go/org => ../org

replace github.com/vislake/speed/go/pkgcore => ../pkgcore

replace github.com/vislake/speed/go/sharing => ../sharing

replace github.com/vislake/speed/go/tenancy => ../tenancy

require (
	github.com/vislake/speed/go/ai-gateway v0.0.1
	github.com/vislake/speed/go/authn v0.0.1
	github.com/vislake/speed/go/billing v0.0.1
	github.com/vislake/speed/go/config v0.0.1
	github.com/vislake/speed/go/metering v0.0.1
	github.com/vislake/speed/go/observability v0.0.1
	github.com/vislake/speed/go/org v0.0.1
	github.com/vislake/speed/go/pkgcore v0.0.1
	github.com/vislake/speed/go/sharing v0.0.1
	github.com/vislake/speed/go/tenancy v0.0.1
)

require (
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-oidc/v3 v3.16.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/nicksnyder/go-i18n/v2 v2.6.1 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/vislake/speed/go/dbkit v0.0.1 // indirect
	github.com/vislake/speed/go/jobs v0.0.1 // indirect
	github.com/vislake/speed/go/ratelimit v0.0.1 // indirect
	github.com/vislake/speed/go/storage v0.0.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	gorm.io/datatypes v1.2.7 // indirect
	gorm.io/driver/mysql v1.5.6 // indirect
	gorm.io/gorm v1.31.2 // indirect
)
