// Package i18n is pkgcore's merged message catalog: it aggregates the locale
// bundles every module ships, and renders a message in the language the
// caller asks for. It is the backend half of the internationalization
// decision recorded in docs/internal/11-cross-cutting.md, built on
// nicksnyder/go-i18n (the library that design doc adopts for plural forms,
// nested messages and ecosystem maturity).
//
// # What pkgcore/i18n holds
//
// API responses are never translated on the backend: handlers return an
// apperr.Error code plus parameters ({"code":"billing.quota_exceeded",
// "params":{"limit":1000}}), and the client resolves the code through its
// own catalog. The messages this package renders are the content the
// backend generates itself -- emails (invites, password resets, over-quota
// reminders), exported invoices and bills, webhook notification text,
// audit-log descriptions -- and that content is rendered in the
// RECIPIENT's locale, never the operator's: an English-speaking customer
// must get an English invite even when the admin who triggered it works in
// a Chinese UI. Message ids reuse the "<module>.<reason>" grammar of
// apperr codes, so one id can serve both as an API error code and as the
// key of a backend-rendered message.
//
// # How a catalog is built
//
// Kernel.Bootstrap reads every registered module's Locales() embed.FS and
// feeds it to a Builder -- one Builder per bootstrap, mirroring how dbkit's
// MigrationRegistry aggregates every module's Migrations() FS. Once every
// module has been added, Build freezes the merge into an immutable
// *Catalog and Bootstrap installs it on the Registry, reachable through
// Registry.Locales(). A Catalog is therefore read-only by construction and
// safe for concurrent use; only a bootstrapped Registry carries one (a
// Registry built directly with NewRegistry has no catalog, exactly like it
// has no ObjectStore).
//
// # The locale file contract
//
// A locale file is <language>.toml, embedded flat at the FS root
// (examples/reference-app/internal/notes/locales and go/pkgcore/locales
// show the shape), where <language> is the canonical spelling of a BCP 47
// language tag. The catalog serves exactly the languages modules ship
// files for: the first module that ships files fixes the catalog's
// language set, and every later module that ships messages must ship one
// file per catalog language. Adding a language is therefore one new file
// per message-shipping module, never a change to this package --
// docs/internal/11-cross-cutting.md guarantees full zh-CN and en-US
// coverage for v1.0 but deliberately does not freeze the mechanism to
// them. The files themselves follow one rule:
//
//   - Every top-level key is a message id; the id is the whole key,
//     written quoted so the dots stay literal, for example
//     "notes.text_required" = "Note text must not be empty."
//
//   - A plain string value is a single-form message.
//
//   - A table whose keys are message keys -- zero, one, two, few, many,
//     other (case-insensitive), plus description, hash, leftdelim,
//     rightdelim and id -- is a plural message:
//
//     ["billing.quota_exceeded"]
//     description = "Sent when a tenant exceeds its monthly quota."
//     one = "{{.Count}} request remains this month."
//     other = "{{.Count}} requests remain this month."
//
//   - Grouping sections such as [errors] are NOT supported: go-i18n folds
//     the section name into the message id, so a message intended as
//     "notes.text_required" would silently load as
//     "errors.notes.text_required". One entry per message, flat, at the
//     top level. The reference-app notes files were converted to this
//     shape when pkgcore/i18n landed; the old grouped shape fails loading
//     with ErrUnsupportedShape.
//
//   - Every message id must start with "<module>." -- the module's own
//     Name plus a dot -- and AddModule rejects a module name containing a
//     dot with ErrInvalidModuleName. Together the rules keep each module's
//     id space disjoint by construction: a dotted name would nest one
//     module's prefix inside another's ("my" and "my.module" could both
//     own "my.module.user_invite"), while no id can start with two
//     different dot-free "<module>." prefixes. Two modules can therefore
//     never own the same message id, and a merge is unambiguous -- there
//     is no silent override to defend against.
//
//   - Every language a module ships must carry the same id set, or
//     AddModule fails with ErrParityMismatch. Parity is enforced as each
//     module is added, which makes every catalog that builds well-formed by
//     construction; tools/check_i18n_keys.py checks the same rule over the
//     raw zh-CN/en-US files when it is run -- no workflow runs it yet (its
//     wiring belongs to the gated docs-check pipeline, docs/internal/
//     18-cicd.md), so it executes directly, like the other tools/ checkers.
//
// # Plural categories
//
// The plural category a count falls into follows CLDR rules, which this
// package gets from go-i18n: zh-CN has a single category ("other"), so a
// Chinese plural message needs only the other form; en-US distinguishes
// "one" and "other", so an English plural message normally defines both.
// Rendering the count itself is ordinary parameterization: the plural
// message above references {{.Count}}, and the caller passes that value in
// params -- count selects the category, params supply the template data.
//
// # Errors
//
// Lookup fails loudly, returning ErrUnknownLocale for a locale this catalog
// does not ship and ErrUnknownCode for an id the locale does not have --
// there is no silent fallback to another language, so a missing message can
// never hide behind an English default. Template text is compiled by
// go-i18n at render time with text/template semantics: parameters are
// referenced as {{.Field}}, a missing parameter renders as "<no value>"
// (Go's default), and template syntax errors surface as render errors, so a
// broken message file is a loud, testable failure rather than a blank
// string.
package i18n
