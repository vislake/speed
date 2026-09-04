module github.com/vislake/speed/go/saasctl

go 1.25.0

// The requires below are the db migrate command's real module graph,
// maintained by go mod tidy: the four migration-shipping modules whose
// migrations the command applies (authn, config, org, rbac -- direct,
// because migrate constructs each module's own NewModule), the packages
// the constructors and the command's code sit on (dbkit, pkgcore), and
// gorm.io/gorm, the dbkit handle type every module constructor takes.
// observability, ratelimit and tenancy carry // indirect exactly where
// tidy put them: required by the direct speed modules above, not imported
// here.
require (
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000 // indirect
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000 // indirect
	github.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000 // indirect
)

// golang.org/x/mod is one of the direct third-party dependencies, justified
// in internal/upgrade's package doc: rewriting a consumer go.mod must
// preserve the file the Go toolchain itself maintains (comments, blocks,
// formatting, replace directives), and modfile is the Go team's own parser
// for the job. Its version is the graph's minimum, not a saasctl choice:
// minio-go -- required in turn by pkgcore's S3-backed ObjectStore -- needs
// x/mod at this height, so MVS raises it here too.
require golang.org/x/mod v0.38.0

require gorm.io/gorm v1.31.2

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/vislake/speed/go/jobs v0.0.0-00010101000000-000000000000 // indirect
	gorm.io/datatypes v1.2.7 // indirect
	gorm.io/driver/mysql v1.5.6 // indirect
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-oidc/v3 v3.16.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/nicksnyder/go-i18n/v2 v2.6.1 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vislake/speed/go/pki v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)

// Every graph module resolves to its sibling directory in this repository
// (the transition-state shape every consumer go.mod carries: speed modules
// are never fetched remotely, so the require above only pins the version).
replace github.com/vislake/speed/go/pkgcore => ../pkgcore

replace github.com/vislake/speed/go/dbkit => ../dbkit

replace github.com/vislake/speed/go/tenancy => ../tenancy

replace github.com/vislake/speed/go/observability => ../observability

replace github.com/vislake/speed/go/config => ../config

replace github.com/vislake/speed/go/ratelimit => ../ratelimit

replace github.com/vislake/speed/go/authn => ../authn

replace github.com/vislake/speed/go/rbac => ../rbac

replace github.com/vislake/speed/go/org => ../org

replace github.com/vislake/speed/go/pki => ../pki

replace github.com/vislake/speed/go/jobs => ../jobs
