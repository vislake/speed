// Package admin provides the operations-console backend:
// docs/internal/23-admin.md's round 1 -- the operator-facing tenant ledger
// (D3, event-driven lazy population plus manual CRUD), the impersonation
// pipeline (D5, in full), cross-tenant user search (D6) and the
// audit-query HTTP shell (D7's read side). See AGENTS.md for the module's
// wiring contract and what round 2 defers.
package admin
