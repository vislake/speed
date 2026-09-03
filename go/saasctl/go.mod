module github.com/vislake/speed/go/saasctl

go 1.25.0

// saasctl's own code imports none of these yet -- the requires below declare
// the module graph the tool will build against once the later blocks of this
// round land (the reference-app-style consumer projects `saasctl new`
// generates are migrated and read through these same modules). Declaring the
// full graph up front keeps every subsequent go.mod edit additive: later
// imports only add go.sum entries, never require lines. go mod tidy is
// therefore still not run on this module -- it would delete every require
// below until the later blocks import them.
require (
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/tenancy v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/observability v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/ratelimit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/org v0.0.0-00010101000000-000000000000
)

// golang.org/x/mod is saasctl's single third-party dependency, justified in
// internal/upgrade's package doc: rewriting a consumer go.mod must preserve
// the file the Go toolchain itself maintains (comments, blocks, formatting,
// replace directives), and modfile is the Go team's own parser for the job.
// One version, pinned in step with the workspace's go.work.sum.
require golang.org/x/mod v0.27.0

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
