// Package config implements the dynamic-config module: the schema-driven
// runtime configuration store that every module's ConfigItem and
// FeatureFlag declarations land in after Bootstrap, served through
// typed reads, scoped writes, change propagation and two unauthenticated
// HTTP endpoints. The module's contracts, the post-Bootstrap attach seam
// and its known limitations are documented in go/config/AGENTS.md.
//
// The module's shape, in one pass:
//
//   - Modules declare their configuration schema (items via
//     pkgcore.ConfigSchemaRegistrar, flags via pkgcore.FeatureRegistrar)
//     during Register. The runtime schema is only complete once Bootstrap
//     has registered every module, so config.NewModule's host calls
//     (*Module).Attach on the booted Registry right after Bootstrap and
//     receives the Service back. Routes are mounted during Register and
//     resolve the attached Service lazily; a request served in the window
//     between Register and Attach reports ErrServiceNotAttached rather
//     than misbehaving.
//
//   - Values live in the shared "configs" table, keyed by (key, scope,
//     tenant_id): platform-wide ScopeSystem rows and per-tenant
//     ScopeTenant overrides. Reads fall back from a tenant override to the
//     system row to the schema default; writes for ScopeTenant take the
//     tenant from the context and ScopeSystem writes demand an audited
//     system context (see go/tenancy). The future ScopeUser tier is
//     reserved and deliberately unimplemented.
//
//   - Values of Sensitive items are encrypted at rest with a dbkit.Cipher
//     the host injects (AES-256-GCM, see go/dbkit), decrypted on read and
//     never emitted on a public endpoint, in a log line or in a change
//     event: event payloads carry the "[redacted]" marker instead.
//
//   - Every successful Set publishes a config.item.changed event on the
//     shared bus (declared through pkgcore's event registrar). Config
//     itself never writes audit rows: a host that composes the optional
//     compliance module gets one audit row per set, that module
//     subscribing to the event to record who changed what. Subscribing
//     and recording need this module's own API -- the event's name, its
//     payload type and its audit action -- so the dependency runs one
//     way, and that is the point: the Set write path never needs to
//     know who keeps the ledger. Instances sharing the bus invalidate
//     their caches on it; a background poller re-reads recently-updated
//     rows as an anti-loss net for events that never arrived.
//
//   - GET /api/config/public answers a resolved tenant's effective values
//     for every Public item plus the enabled feature flag list, and
//     GET /api/system/features answers the enabled flags -- both
//     unauthenticated, since a login page must render before any sign-in
//     has happened, resolving the tenant through the host-injected
//     tenancy.Resolver and never erroring over an unmapped host.
package config
