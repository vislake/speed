module github.com/vislake/speed/examples/reference-app

go 1.26.0

replace github.com/vislake/speed/go/pkgcore => ../../go/pkgcore

replace github.com/vislake/speed/go/ai-gateway => ../../go/ai-gateway

replace github.com/vislake/speed/go/dbkit => ../../go/dbkit

replace github.com/vislake/speed/go/tenancy => ../../go/tenancy

replace github.com/vislake/speed/go/observability => ../../go/observability

replace github.com/vislake/speed/go/config => ../../go/config

replace github.com/vislake/speed/go/rbac => ../../go/rbac

replace github.com/vislake/speed/go/org => ../../go/org

replace github.com/vislake/speed/go/authn => ../../go/authn

replace github.com/vislake/speed/go/pki => ../../go/pki

// go/billing and go/metering reach this app transitively through
// go/admin's D9 usage-dashboard wiring (docs/internal/23-admin.md) --
// go/admin imports both directly (it sits at the top of the module
// dependency graph and is explicitly permitted to), so this app's own
// go.mod needs its own replace for them too, the identical reasoning the
// go/jobs and go/storage comment below already gives for the same shape
// of transitive local-module dependency.
replace github.com/vislake/speed/go/metering => ../../go/metering

replace github.com/vislake/speed/go/billing => ../../go/billing

// go/jobs and go/storage are imported directly by this app: buildServer
// wires the storage module's asynchronous object work onto a
// jobs.StandaloneQueue sharing the app's own database (see server.go).
// Like every other workspace-local module they carry no published version,
// so this app's own go.mod needs its own replace for them too -- root
// CLAUDE.md's per-module standalone-build rule (`GOWORK=off go build`)
// means `go mod tidy` must resolve every dependency without relying on the
// workspace.
replace github.com/vislake/speed/go/jobs => ../../go/jobs

replace github.com/vislake/speed/go/storage => ../../go/storage

// go/notification is imported directly by this app: buildServer wires
// the notification module as the round's mandatory-first-consumer proof
// (see server.go and demo_notification.go). Like every other
// workspace-local module it carries no published version, so this app's
// own go.mod needs its own replace for it too -- root CLAUDE.md's
// per-module standalone-build rule (`GOWORK=off go build`) means
// `go mod tidy` must resolve every dependency without relying on the
// workspace.
replace github.com/vislake/speed/go/notification => ../../go/notification

// go/ratelimit is not imported directly by this app; it is a transitive
// dependency of go/org (its invitation rate limiting, see
// go/org/invite.go). Like every other workspace-local module it carries no
// published version, so this app's own go.mod needs its own replace for it
// too -- root CLAUDE.md's per-module standalone-build rule (`GOWORK=off go
// build`) means `go mod tidy` must resolve every dependency, direct or
// transitive, without relying on the workspace.
replace github.com/vislake/speed/go/ratelimit => ../../go/ratelimit

// go/sharing is imported directly by this app: buildServer wires it as the
// round's mandatory-first-consumer proof (see server.go, sharing_resolver.go
// and sharing_flow_test.go). Like every other workspace-local module it
// carries no published version, so this app's own go.mod needs its own
// replace for it too -- root CLAUDE.md's per-module standalone-build rule
// (`GOWORK=off go build`) means `go mod tidy` must resolve every
// dependency without relying on the workspace.
replace github.com/vislake/speed/go/sharing => ../../go/sharing

// go/compliance and go/admin are imported directly by this app: buildServer
// wires them as go/admin round 1's mandatory-first-consumer proof (see
// server.go, demo_admin.go and admin_flow_test.go). Like every other
// workspace-local module they carry no published version, so this app's
// own go.mod needs its own replace for them too -- root CLAUDE.md's
// per-module standalone-build rule (`GOWORK=off go build`) means
// `go mod tidy` must resolve every dependency without relying on the
// workspace.
replace github.com/vislake/speed/go/compliance => ../../go/compliance

replace github.com/vislake/speed/go/admin => ../../go/admin

require (
	github.com/google/uuid v1.6.0
	github.com/minio/minio-go/v7 v7.3.0
	github.com/redis/go-redis/v9 v9.14.1
	github.com/testcontainers/testcontainers-go v0.44.0
	github.com/testcontainers/testcontainers-go/modules/minio v0.44.0
	github.com/testcontainers/testcontainers-go/modules/redis v0.44.0
	github.com/vislake/speed/go/admin v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ai-gateway v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/compliance v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/jobs v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/notification v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/pki v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/sharing v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/storage v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/otel/sdk v1.44.0
	gorm.io/gorm v1.31.2
)

require github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000 // indirect

require (
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/containerd/platforms v0.2.1 // indirect
	github.com/coreos/go-oidc/v3 v3.16.0 // indirect
	github.com/cpuguy83/dockercfg v0.3.2 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/ebitengine/purego v0.10.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20260330125221-c963978e514e // indirect
	github.com/magiconair/properties v1.8.10 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mdelapenya/tlscert v0.2.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/go-archive v0.3.0 // indirect
	github.com/moby/moby/api v1.55.0 // indirect
	github.com/moby/moby/client v0.5.0 // indirect
	github.com/moby/patternmatcher v0.6.1 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/moby/sys/user v0.4.1 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nicksnyder/go-i18n/v2 v2.6.1 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/otlptranslator v1.0.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/shirou/gopsutil/v4 v4.26.6 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/testcontainers/testcontainers-go/modules/postgres v0.44.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/vislake/speed/go/billing v0.0.0-00010101000000-000000000000 // indirect
	github.com/vislake/speed/go/metering v0.0.0-00010101000000-000000000000 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/prometheus v0.66.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gorm.io/datatypes v1.2.7 // indirect
	gorm.io/driver/mysql v1.5.6 // indirect
	gorm.io/driver/postgres v1.6.2 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)
