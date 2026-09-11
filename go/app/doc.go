// Package app is the speed platform's application assembly layer: the
// structure every application's own boot code is built from.
//
// A speed application composes platform components into one binary -- its
// own component set, its own bootstrap values, its own route table -- and
// that half is the host's. Everything else is the same in every application,
// and it lives in this module so the knowledge is written once instead of
// per host:
//
//   - this package (go/app): the assembly engine. The loader resolves the
//     host's configuration, the composition configuration and the declared
//     bootstrap key material before anything runs; the driver walks a
//     pkgcore.ComponentRegistry through the seven lifecycle stages
//     (Prepare, Construct, Verify, Init, Start, Stop, Close) and provides
//     the Run sugar around a context's lifetime. The engine contains no
//     HTTP assembly and no listening: a host's own application component
//     composes the routes from the declaration seats and owns the listener.
//     The host-neutral HTTP helpers the engine and its consumers share --
//     authn's mount-path constant, the serve timeouts, the pre-auth
//     allowlist set (PreAuthAllowlist) -- live beside it in the same
//     package.
//   - go/app/chain: the fixed middleware chain -- the order authn then the
//     optional impersonation decorator then tenancy with the pre-auth
//     allowlist, with the authn subtree and the admin route dispatched around
//     it by structure. chain.Standard derives the whole composition from a
//     registry's mounted routes (either registry shape answers its
//     RouteSource: the module Registry and the component assembly's
//     ComponentRegistry); chain.Chain is the direct path for a host with a
//     custom layout. Its closure is bounded by the chain's own participants
//     (authn and rbac included); it imports no module a chain-bearing host
//     does not already have.
//   - go/app/bridges: the no-import seam bridges that hand one module's
//     concrete service to another module's structurally-typed seam
//     (billing/metering onto ai-gateway's two seams, the config module's
//     lazy handle onto org's and authn's feature gates and onto sharing's
//     tenant-config reader). Separate so only hosts that wire those modules
//     pay for them.
//
// # The boundary the module holds itself to
//
//   - Structure belongs here: the assembly order, the seam wiring, the HTTP
//     face helpers and the lifecycle.
//   - Policy belongs to the host: which components compose the application,
//     which values they are configured with, which seeds they write, which
//     keys the host's own target declares. The engine ships no implicit
//     default -- a configuration target is named or the assembly fails, and
//     a value the host does not supply stays unset.
//   - Infrastructure implementations are not constructed here. It builds no
//     EventBus, KVStore, Mailer, ObjectStore and opens no database -- which
//     implementation a process runs is the assembling application's
//     decision, selected as a component in the composition configuration;
//     the repository's depguard configuration enforces this mechanically,
//     because this module sits under go/ exactly as every business module
//     does. The database connection is the host's own component's to open
//     (dbkit.Open), the dialect package imported by the host's binary.
//   - No business domain: no tables, no routes, no permissions, no events,
//     no pkgcore.Module implementation. A host's own modules never depend on
//     this one; docs/internal/01-architecture.md records the module as the
//     module discipline's explicit exception on those terms.
//
// The reference application (examples/reference-app) and the project skeleton
// `saasctl new` materializes are this module's two mandatory consumers.
package app
