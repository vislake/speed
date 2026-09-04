# billing/gateway

Not yet implemented. See docs/internal/06-billing-and-metering.md for the design.

This is a subpackage of `go/billing`, not a module: the payment SDKs it will
carry stay out of every project that does not import it, which is the whole
reason the earlier `go/billing-gateway` module existed. Module-level discipline
for this code lives in `go/billing/AGENTS.md`.
