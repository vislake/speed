module github.com/vislake/speed/tools/configrefgen

go 1.26.0

// This tool module lives outside go/ (it is not a released module: the
// lockstep release coordinator derives its publishable set from go.work's
// use entries under go/, so this entry is a consumer-module entry -- in
// the workspace, never published). Root CLAUDE.md's per-module
// standalone-build rule (`GOWORK=off go build`) means this module's own
// go.mod must resolve every dependency -- direct or transitive -- without
// the workspace, and workspace-local modules carry no published version,
// so each one this generator pulls in gets its own replace pointing at its
// real directory, the shape every consumer module in this workspace
// carries.
replace github.com/vislake/speed/go/pkgcore => ../../go/pkgcore

replace github.com/vislake/speed/go/app => ../../go/app

replace github.com/vislake/speed/go/dbkit => ../../go/dbkit

replace github.com/vislake/speed/go/observability => ../../go/observability

replace github.com/vislake/speed/go/tenancy => ../../go/tenancy

replace github.com/vislake/speed/go/ratelimit => ../../go/ratelimit

replace github.com/vislake/speed/go/config => ../../go/config

replace github.com/vislake/speed/go/jobs => ../../go/jobs

replace github.com/vislake/speed/go/authn => ../../go/authn

replace github.com/vislake/speed/go/org => ../../go/org

replace github.com/vislake/speed/go/pki => ../../go/pki

replace github.com/vislake/speed/go/notification => ../../go/notification

replace github.com/vislake/speed/go/sharing => ../../go/sharing

replace github.com/vislake/speed/go/metering => ../../go/metering

replace github.com/vislake/speed/go/compliance => ../../go/compliance

require (
	github.com/knadh/koanf/parsers/yaml v1.1.1
	github.com/knadh/koanf/providers/file v1.2.1
	github.com/knadh/koanf/v2 v2.3.6
	github.com/vislake/speed/go/app v0.0.1
	github.com/vislake/speed/go/authn v0.0.1
	github.com/vislake/speed/go/compliance v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.1
	github.com/vislake/speed/go/dbkit v0.0.1
	github.com/vislake/speed/go/jobs v0.0.1
	github.com/vislake/speed/go/metering v0.0.1
	github.com/vislake/speed/go/notification v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.1
	github.com/vislake/speed/go/pkgcore v0.0.1
	github.com/vislake/speed/go/pki v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/sharing v0.0.1
	gorm.io/gorm v1.31.2
)

require (
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-oidc/v3 v3.16.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/nicksnyder/go-i18n/v2 v2.6.1 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vislake/speed/go/observability v0.0.1 // indirect
	github.com/vislake/speed/go/ratelimit v0.0.1 // indirect
	github.com/vislake/speed/go/tenancy v0.0.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	gorm.io/datatypes v1.2.7 // indirect
	gorm.io/driver/mysql v1.5.6 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)
