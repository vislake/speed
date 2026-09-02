// Package jobs provides speed's asynchronous task queue: the Queue / Task /
// Job / Handler contract every runtime profile implements, and DemoQueue,
// the demo profile's in-process worker pool backed by a SQLite-persisted
// task table.
//
// See docs/internal/07-platform-services.md for the design this package
// implements, and AGENTS.md for the full API reference, the tenant-context
// rebuild trap every caller of a Handler must close, and this package's
// documented known limitations.
package jobs
