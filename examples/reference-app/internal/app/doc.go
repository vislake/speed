// Package app is the reference app's assembly library: the composition and
// host wiring the application runs on. It is what cmd/server used to be
// before the test-layout migration's library-ization round split the
// command from the code it runs (docs/internal/25-test-layout-migration.md's
// Plan B, assembly-as-importable-package). Everything importable lives
// here -- BuildServer and its route
// handlers, ServerConfig and its environment parsing, the demo identity
// layer (demo subject resolvers, demo account/membership/grant seeding),
// the host-side route guards, the self-service provisioner, the path
// constants and write-error helpers the HTTP surfaces share -- while
// cmd/server's main.go keeps only the process glue only a process needs:
// signal handling, http.Server start/stop, the healthcheck re-invocation
// and observabilityOptions.
//
// # Why the split exists
//
// Go forbids importing a package named main, so an app whose assembly tests
// live in cmd/server can never move those suites to the dedicated app-level
// test directory the test-layout rule demands: nothing outside the command
// can import the code under test. With the assembly in an importable
// internal package, cmd/server stays the binary (its docker/README/Taskfile
// references are untouched) and the flow suites' migration (batch 2b-ii)
// imports this package instead.
//
// # The exported surface is the test-entry surface
//
// The exported names of this package are deliberately the surface the
// assembly-flow tests exercise, exported from the names the suites used
// while they sat in package main (buildServer -> BuildServer,
// serverConfig -> ServerConfig, demoOwnerUserID -> DemoOwnerUserID, and so
// on -- a first-letter capitalization, nothing else, so the suites' edits
// stayed mechanical). It is an internal package: this surface is for
// cmd/server and the app's test directories, and is not subject to the
// frozen-API discipline a published module's exports carry. Names no test
// or cmd/server needs stay unexported; the doc comment on each exported
// name states what the name is for.
//
// # What did not move
//
// cmd/server keeps the genuinely main-only surface: run (signal-driven
// http.Server lifecycle), runHealthcheck (the Dockerfile HEALTHCHECK
// re-invocation), observabilityOptions and their constants, all exercised
// by cmd/server/main_test.go in the command's package.
package app
