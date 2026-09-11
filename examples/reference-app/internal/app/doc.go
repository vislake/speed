// Package app is the reference app's assembly library: the composition and
// host wiring the application runs on. The application assembles through the
// config-driven component assembly: this package contributes the host's own
// components (component.go, host_wiring.go) -- the step components wrapping
// attach.go's and serve.go's assembly steps, the override components that
// construct a module with the host's own wiring, and the provider components
// whose products satisfy the seam tokens the assembled modules declare --
// and drives them with the engine's Assemble (server.go). Everything
// importable lives here -- BuildServer and its route handlers, ServerConfig
// and ConfigFromEnv's loader-driven bootstrap (bootstrap.go), the host-side
// route guards, the self-service provisioner, the path constants and
// write-error helpers the HTTP surfaces share, and the development key
// defaults (devkeys.go) -- while the demonstration layer (the fixed demo
// identities, the route authorization table and the boot-time demo seeds)
// lives in the internal/app/demo package beside it, and cmd/server's
// main.go keeps only the process glue a process needs: signal handling,
// http.Server start/stop, the healthcheck re-invocation and
// observabilityOptions.
//
// # Why the package exists
//
// Go forbids importing a package named main, so an assembly the app's test
// surfaces must exercise can never live in the command's package: nothing
// outside the command could import the code under test. internal/app is
// that assembly's importable home. The command runs it -- cmd/server's
// main.go boots it as the thin process shell -- and the app's test
// surfaces import it: the assembly-flow suites exercise the composed
// server from the app-level test directory the test-layout rule assigns
// them (examples/reference-app/flowtests, package flowtests), while the
// suites that remain in cmd/server are the command's own main-shell tests
// plus the recorded white-box and unit-level exceptions that exercise
// this package from the command's package.
//
// # The exported surface is the test-entry surface
//
// The exported names of this package are deliberately the surface the
// assembly-flow tests exercise: every name a suite or the command needs is
// exported, each with a doc comment stating what it is for, and names no
// suite or the command needs stay unexported. The package is internal --
// its surface serves cmd/server and the app's own test directories, not a
// published module's frozen API.
//
// # What the command keeps
//
// cmd/server retains the genuinely main-only surface: run (signal-driven
// http.Server lifecycle), runHealthcheck (the Dockerfile HEALTHCHECK
// re-invocation), observabilityOptions and their constants, all exercised
// by cmd/server/main_test.go in the command's package.
package app
