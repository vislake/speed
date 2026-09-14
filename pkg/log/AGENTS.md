# pkg/log

The logging module: the capability modules take up from each other, the
handler chain the configuration assembles, the redaction layer every record
passes through, and the storage of a logger in a context.

Everything lives in the root package, because the built-in formats and
destinations use the standard library alone: there is no third-party
dependency to isolate in a subpackage. `internal/loghost` is not part of the
product — it is the child-process host this module's end-to-end cases drive.

## Where the authority is

- What this module is and why it is shaped this way:
  [`docs/design/modules/design-log.md`](../../docs/design/modules/design-log.md).
- The decisions behind it, one file per decision:
  [`docs/adr/`](../../docs/adr/) — the calling surface, the redaction
  mechanism, the storage of a logger in a context.
- The API itself: the godoc in this package.

This file states only what neither of those states: the boundaries a
change here must not cross, and how to run it.

## Boundaries

- **The formats and the destinations are closed sets, and widening one is a
  code change in this module, not an extension point.** A name outside its
  set is rejected by a `switch` that returns a sentinel listing the legal
  values (`pkg/log/chain.go` `encoderFor`, `pkg/log/writer.go` `openWriter`).
  Nothing arrives by import, resource or registration.

- **The redaction mechanism lives here; no rule does.** The rule set starts
  empty, and a registration through `Redaction` is the only thing that fills
  it (`pkg/log/rules.go` `emptyRuleSet`, `AddKeys`, `AddPattern`). A change
  that presets a sensitive key name or a value shape is the shape to reject:
  which names and shapes are sensitive is knowledge this package does not
  have.

- **The calling surface is the standard library's `*slog.Logger`; this
  package defines no logging API of its own.** A call site writes through
  `*slog.Logger` and depends on `log/slog` rather than on this project
  (`pkg/log/log.go`). What is exported here besides that is the capability
  itself and the schema a host declares its configuration in — not a wrapper
  over the standard library's types.

- **The chain is ordered level, then redaction, then fan-out, and no logger
  derived from it slips past the redaction layer.** That layer implements
  `WithAttrs` and `WithGroup` and passes both on (`pkg/log/redact.go`); a
  derived handler that reaches the downstream without it is the shape to
  reject. One record is judged once, ahead of the fan-out, so every
  destination and format gets the same judgement.

- **The attribute key a `Named` logger binds and the text a masked value
  leaves in its place are exported constants — `ModuleAttrKey` and
  `MaskedText` — and changing either value is a host-visible change**
  (`pkg/log/module.go`, `pkg/log/rules.go`). They are read off the records
  the chain actually writes, by this module's cases and by a host's, so a new
  value ships with a `!` footer or a release-note entry, and neither value is
  spelled a second time inside this module.

## Running it

From the repository root, over every module in the workspace:

```
make build
make test
make lint
make check
```

`make check` is what CI runs. The suite includes the end-to-end cases in
`internal/loghost`, which build that host and run it as a real child process;
the tests need nothing installed beyond the Go toolchain.
