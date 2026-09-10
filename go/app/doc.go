// Package app is the speed platform's composition toolkit: the
// host-neutral assembly every application's own boot code is built from.
//
// A speed application composes platform modules into one binary -- its own
// route table, its own module set, its own bootstrap configuration -- and
// underneath that host-specific half every application recomposes the same
// few things, each of which lives in this module so the knowledge is
// written once instead of per host:
//
//   - this package (go/app): the host *kernel* -- authn's mount-path
//     constant, the serve/read-header/shutdown timeouts, the pre-auth
//     allowlist set (PreAuthAllowlist), the mounted-route label seed
//     (RegisterMountedRoutes) and the serve-and-drain lifecycle
//     (ServeUntilShutdown). Its dependency closure is deliberately
//     minimal (pkgcore, tenancy, config, observability), because every
//     composition carries it -- the smallest generated project's main.go
//     calls ServeUntilShutdown.
//   - go/app/chain: the fixed middleware chain -- the order authn then
//     the optional impersonation decorator then tenancy with the pre-auth
//     allowlist, with the authn subtree and the admin route dispatched
//     around it by structure -- assembled by chain.Chain. Its closure is
//     bounded by the chain's own participants (authn included); it
//     imports no module a chain-bearing host does not already have.
//   - go/app/bridges: the no-import seam bridges that hand one module's
//     concrete service to another module's structurally-typed seam
//     (billing/metering onto ai-gateway's two seams, the config module's
//     lazy handle onto org's and authn's feature gates and onto sharing's
//     tenant-config reader). Separate so only hosts that wire those
//     modules pay for them.
//
// What this module deliberately is not:
//
//   - Not a framework. It holds no policy defaults, keeps no state, and
//     wraps no host in a lifecycle of its own: every exported function is
//     a pure assembly step the host calls from its own boot sequence, and
//     a host remains free to assemble any of it by hand -- the functions
//     encode the composition every host needs, not a required entry
//     point.
//   - Not an infrastructure compositor. It constructs no EventBus,
//     KVStore, Mailer or ObjectStore implementation, ships no deployment
//     mode, and selects no preset; which implementations a process runs
//     is the assembling application's decision, wired through pkgcore's
//     kernel options. The repository's depguard configuration enforces
//     this mechanically: this module sits under go/, so the
//     concrete-infrastructure import bans apply to it exactly as they do
//     to every business module.
//   - Not a business domain. It owns no tables, publishes no events, and
//     declares no permissions; it exists so domain modules never have to
//     know about each other's concrete types and hosts never have to
//     restate the composition order. docs/internal/01-architecture.md
//     records it as the module discipline's explicit exception: a
//     composition-tools module with no domain of its own.
//
// The reference application (examples/reference-app) and the project
// skeleton `saasctl new` materializes both compose through this module;
// they are its two mandatory consumers.
package app
