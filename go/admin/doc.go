// Package admin provides the operations-console backend:
// docs/internal/23-admin.md's design, both landed rounds (AGENTS.md's
// "Status: round 2 of 2 landed") -- the operator-facing tenant ledger with
// tenant-suspension enforcement (D3+D4), the impersonation pipeline (D5, in
// full), cross-tenant user search (D6), the audit-query HTTP shell plus its
// asynchronous export leg (D7), role management wrapping rbac.Service (D8),
// the cross-tenant usage/billing dashboard (D9) and notification
// send-record search (D10). See AGENTS.md for the module's wiring contract
// and what remains unbuilt.
package admin
