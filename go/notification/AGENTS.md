# notification

`go/notification` is the platform's outbound-message module: it delivers a
tenant's notifications to the people it serves, on the channels those people
want (the in-app inbox, email, SMS), and -- for contacts who are not users of
any tenant -- only where consent has been verified first. The module's design,
channel taxonomy and consent rules live in `docs/internal/07-platform-services.md`;
this file is the module-level operating guide that ships to consuming projects.

## Status

Implemented end to end: the in-app inbox, the per-type channel preference
matrix, the external-contact consent ledger with double opt-in and business
attestation, the async delivery pipeline with its per-attempt send records,
the platform-blacklist table (schema and read path; writers deferred, see
below), the module's own OpenAPI fragment with a generated, compile-checked
HTTP handler, the per-replica realtime hub behind the inbox stream, and the
dual-dialect migration set. The reference app (`examples/reference-app`) is
the mandatory first consumer: `cmd/server/server.go` wires `notification.NewModule`
through `Kernel.Bootstrap`, `cmd/server/demo_notification.go` supplies the
host-side demo seams, and `cmd/server/notification_flow_test.go` drives the
composed HTTP stack through the module's surfaces.

Nothing here is a stub. The "Deferred to later rounds" section below is the
complete, honest list of what this round deliberately does not ship.

## What this module owns

- The in-app inbox: per-tenant rows in `in_app_messages` that a user reads
  inside the product, plus the delivery subscriber that renders and fills
  them. In-app is the module's first-class channel: it has zero external
  dependencies, so it works in every deployment composition.
- The per-type channel preference matrix (`notification_preferences`) that
  decides which channels a type of notification may use for a recipient, and
  the consent ledger (`verified_contacts`) that decides whether a given
  external recipient may be messaged at all.
- Delivery: the queue job that turns a published domain event (or a direct
  `Deliveries().Dispatch` call) into one rendered message per recipient
  per selected channel, with one `send_records` row per attempt as the
  replay-safe outcome log. Replay dedupe collapses an identical
  re-dispatch onto the already-settled record; nothing aggregates or
  rate-limits routine deliveries this round (the module's rate limits
  cover verification-code sends and the consent-create path only) --
  those delivery-path shapes are deferred below.
- The module's own HTTP surface under `/api/v1/notifications`, served by a
  handler compiled against the generated interface of its own OpenAPI
  fragment, plus the realtime inbox stream.

## What it deliberately does NOT own

The module never imports `authn`, `rbac` or `org`, and has no table in any of
their domains. A user is an opaque id learned from an authenticated caller or
a domain event; a tenant's organization structure is the same; addresses of
*user* recipients are identity data the host resolves through seams, never
rows here. `notification` also never declares the notification types it
delivers: a type is a declaration of the business module that emits it,
registered on the host registry with `reg.Notifications.Add`
(`pkgcore.NotificationType`), the same relationship events have with
`reg.Events.Publishes` -- and the taxonomy a preference write or a delivery
consults is the live registry list, read at call time, never a snapshot
(a business module may legitimately register its types after notification's
own `Register` ran). Business modules never depend on notification either:
they publish domain events, and notification's consumption of them is the
host's wiring, never the module's -- a host subscribes and calls
`Deliveries().Dispatch` (the reference app's `wireDemoNotification` is that
glue), while the module's own `Register` subscribes to nothing but its
inbox-created event for the local hub fan-out. That direction is what keeps
the module a leaf of the dependency graph rather than a hub every other
module must import.

## Module inventory

### Files

- `module.go` -- the `Module` (`NewModule(db, options...)`, `Register(reg)`),
  the six required host options and the optional `WithSubjectResolver`, the
  exported service accessors `Preferences()`, `Contacts()`,
  `Deliveries()`. Compile-time `var _ pkgcore.Module` pin.
- `types.go` -- the closed channel vocabulary (`ChannelInApp`/`ChannelEmail`/
  `ChannelSMS`), what a type declaration says, and the canonical sorting
  (`in_app`, `email`, `sms`) preference rows and dedupe keys are stored in.
- `model.go`, `repository.go` -- `InboxMessage` and its `Repository`
  (a named type embedding `dbkit.Repository[InboxMessage]`, adding the
  delivery path's `FindByDedupeKey` probe and the read surface
  `ListForRecipient`/`UnreadCount`/`MarkRead`/`ReadAll`).
- `preference.go`, `preference_repository.go`, `preference_service.go` --
  the preference matrix: model, repository, and the `PreferenceService`
  decision layer (validate every write against the live taxonomy, refuse
  what a type cannot honor, resolve "which channels" by folding defaults
  under absent rows).
- `contact.go`, `contact_code.go`, `repository`-adjacent queries -- the
  consent ledger: `VerifiedContact`, the status machine, the double opt-in
  code lifecycle, attestation, unsubscribe/mark-bounced, `EnsureDeliverable`,
  and the audit emission of every consent transition.
- `blacklist.go` -- `PlatformBlacklist` (platform data) and its repository's
  cross-tenant `IsBlacklisted` read.
- `send_record.go` -- `SendRecord` (platform data) and its repository
  (`ByTenantAndKey` probe, upsert `Save`), the outcome log the delivery job
  converges on.
- `delivery.go` -- `Dispatch` and the delivery pipeline: the async
  `DeliveryService.Dispatch`, the single registered queue handler
  (`jobTypeDeliver` = `"notification.deliver"`), send-time rechecks,
  rendering, per-channel attempt and record settling.
- `sms.go` -- the in-package SMS seam: the `SMS` message shape and the
  `SMSSender` interface (structurally identical to authn's own sender, so a
  host wires one implementation to both modules), with
  `NewConsoleSMSSender` as the zero-external-dependency implementation that
  doubles as the module's test double (see "SMS stays an in-package seam").
- `render.go` -- the template-render seam: per-channel part shapes and the
  `<type_key>.<channel>.<part>` id convention.
- `hub.go`, `handler.go` -- the per-replica realtime fan-out and the HTTP
  surface (fragment operations plus the hand-mounted stream route).
- `errors.go` -- the module's error catalog (see below); `events.go` -- the
  module's one published event; `doc.go` -- the package overview.
- `api/openapi.yaml` + `api/notification-server.gen.go` -- the module's
  OpenAPI fragment and its generated server interface (see "HTTP surface").
- `locales/` -- the module's bilingual copy bundle (error codes plus the
  verification-code text; see the Copy rule); `migrations/` -- one
  dual-dialect migration set per table family; `internal/testutil` --
  the shared test database helper; `integration_test/` -- the Docker-backed
  PostgreSQL leg, the Redis-bus leg, and the `clinic` demo consumer module.

### Service faces

The three exported accessors are the module's Go API for hosts that exercise
notification outside HTTP:

- `Preferences()` -- the `PreferenceService`: `Set` (validated against the
  live taxonomy), `Get`/`ListForUser` (the per-user reads),
  `NotificationTypes` (the live type directory), and
  `ResolveForDelivery`/`ResolveChannels`, the delivery-side resolution that
  folds a type's defaults under an absent row.
- `Contacts()` -- the `ContactService`: `CreateContact` (double opt-in, or
  business attestation when the input carries a `ConsentRef`), `VerifyCode`
  (the compare-and-swap), `ResendCode`, `Unsubscribe`, `MarkBounced` (the
  marking a hard transport failure calls), and the delivery gate
  `EnsureDeliverable`.
- `Deliveries()` -- the `DeliveryService`: `Dispatch` (validate + enqueue,
  async by construction) and the job handler behind the queue.

### HTTP surface

The fragment declares eleven operations under `/api/v1/notifications`
(`apiPath`), served by `Handler` behind the generated `api.ServerInterface`
(compile-time assertion in `handler.go`): the inbox reads and the mark-read
family, the unread count, the type directory, the preference get/update pair,
and the contact roster's list/create/verify/resend. Every operation is
own-data self-service and refuses without BOTH a ctx tenant (the tenancy
middleware's doing) and an identified caller resolved through the host's
`SubjectResolver` seam (`ErrSubjectUnresolved` otherwise): the module declares
no permissions, so who may reach these surfaces at all is the host
authorization layer's decision.

`GET /api/v1/notifications/stream` is the one route the fragment deliberately
omits -- server-sent events are not an OpenAPI 3.0 media type (the fragment's
header records the omission) -- hand-mounted by `NewHandler` as a method
pattern of exactly the shape the generated registrations use. It is own-data
self-service like every other route: tenant and caller are resolved before the
stream opens, and only that caller's announcements in that tenant's rows pass
the filter. The route carries no heartbeat (see "Known limitations").

### Events, jobs, audit, permissions, types

- Events: the module publishes `EventInboxCreated`
  (`notification.inbox.created`) after an inbox row commits; its payload is
  `InboxCreatedPayload{MessageID, TenantID, RecipientUserID, TypeKey}`, all
  JSON-tagged because the payload crosses the Redis bus. During `Register`
  the module also subscribes to its own event to fan announcements out to the
  local hub (see `hub.go`).
- Jobs: exactly one handler, `reg.Jobs.Handle(jobTypeDeliver,
  m.deliveries)`. One handler delivers every declared type, deciding what to
  do per payload; the type string is deliberately not the name of a
  notification type.
- Audit: three consent-transition actions under `notification.contact.*` --
  `attested`, `verified`, `unsubscribed` -- registered on `reg.AuditActions`
  in `Register`. The audit `Resource`'s display name is the channel plus the
  address's blind index: the plaintext address must never reach the audit
  trail, whose records outlive the row.
- Permissions: none declared, and the module mounts no admin surface.
- Types: none declared (see "What it deliberately does NOT own").

### Tables and data domains

Three migrations per dialect (`sqlite/`, `postgres/`), each tested to apply
from zero against a real PostgreSQL server in the module's integration tier:

| Table | Data domain | Isolation proof |
| --- | --- | --- |
| `in_app_messages` | tenant | `tenancytest.AssertIsolated` |
| `notification_preferences` | tenant | `tenancytest.AssertIsolated` |
| `verified_contacts` | tenant | `tenancytest.AssertIsolated` |
| `send_records` | platform | `tenancytest.AssertNotTenantScoped` |
| `platform_blacklist` | platform | `tenancytest.AssertNotTenantScoped` |

The platform tables deliberately do NOT implement `dbkit.TenantScoped` (a
scoped model would make the isolation plugin inject `WHERE tenant_id` into
every query -- precisely the filter this domain must never have); their real,
unenforced `tenant_id` columns exist for operator visibility, defaulted to the
empty-string sentinel, never filtered on. The one hand-written tenant filter
in the module is `SendRecordRepository.ByTenantAndKey`, which scopes by hand
what the platform model cannot scope by plugin -- the lookup that must honour
the UNIQUE index's scoped-uniqueness semantics.

### Errors

`errors.go` declares the module's `apperr` catalog, grouped by surface: the
preference group (unknown type, illegal opt-out, invalid channels), the
contact group (unknown/invalid contact, code errors, unsubscribed/bounced/
not-verified refusals, rate limits), the delivery group (`ErrDispatchInvalid`
with the offending `field` param), the inbox group (`ErrMessageNotFound` with
`message_id`), the HTTP-transport group, and the wiring group -- the six
`Err*Required` sentinels, all `apperr.Internal`, that `Register` refuses
with. Every code ships bilingual copy in `locales/` (identical id sets in
`zh-CN.toml` and `en-US.toml`, enforced by the catalog builder and by
`tools/check_i18n_keys.py`). `ErrTransportPermanent` deliberately lives OUT
of the catalog: it is not an `apperr` but a control signal between a
transport and the delivery job, matched with `errors.Is`, never surfaced to a
caller. `TestErrorCatalog_IsComplete` pins the catalog's completeness.

## Wiring and host seams

A host wires the module as: `m := notification.NewModule(db,
notification.With...(...))`, then hands `m` to `Kernel.Bootstrap`'s module
set. `Register(reg)` runs in three phases (module.go's doc comment): validate
and copy the six option seams into the services; declare events, audit
actions and the job handler; attach the registries (types, bus, catalog,
audit) and mount the HTTP surface. The attach half is why the module never
captures registry state during registration: `reg.Locales()` is nil inside
`Register` (the catalog lands only after every module registered), so every
consumer reads the catalog from the registry at call time, never earlier.

Six host-supplied options are REQUIRED -- `Register` returns the matching
`Err*Required` (all Internal) without any of them:

- `WithSMSSender` -- the SMS transport (see "SMS stays an in-package seam").
- `WithMailFrom` -- the from-address the contact-email path sends as.
- `WithContactEmailIndexer`, `WithContactPhoneIndexer` -- the blind indexers
  that make an encrypted contact address queryable (see "Separate index keys
  from the cipher key").
- `WithDeliveryQueue` -- the `jobs.Queue` delivery jobs are enqueued on and
  the registered handler consumes.
- `WithUserAddressResolver` -- the host's read of a user recipient's
  outbound addresses at send time (see "Every consent and address decision is
  re-checked at send time").

The host also supplies structural, no-import seams the module consumes as
interfaces it declares -- never as imported packages: `SubjectResolver` (the
HTTP caller's identity, per operation), `UserAddressResolver` (above), the
encrypted-address serializer registration (the exported
`ContactAddressSerializerName` the host binds through
`dbkit.RegisterEncryptedSerializer` before any contact row is read or
written, following org's precedent), and the registry slices
(`handlerHost`/`contactHost`/`deliveryHost`) through which the services read
the merged catalog, the bus and the mailer. The host implements, the module
consumes; a hand-built host in a test satisfies the same interfaces.

## Delivery pipeline

`Dispatch` is async by construction: `Dispatch()` only validates and
enqueues (the payload is also the job payload, so its JSON shape is part of
the queue contract), and every decision that can change between enqueue and
delivery -- the recipient's channel preferences, an external contact's
consent and status, the addresses on file -- is re-checked at send time by
the job, never frozen into the payload. The job renders the type's copy for
the recipient's locale from the merged catalog (send time, never enqueue
time; a render can never precede the recheck that might have skipped it),
writes the in-app row and/or drives the email/SMS transports, and settles one
`send_records` row per attempted channel.

Record semantics (`send_record.go`): `succeeded` is written only after the
transport accepted the send, `failed` after a failure exhausted an attempt,
`skipped` after a deliberate non-send whose reason will not change by
retrying -- no address on file, an external contact whose consent lapsed.
`Error` is the failure's own text: the wrapped cause message a failed attempt
returned (delivery.go's fail-and-retry / fail-and-stop paths), or a short
`skipReason*` phrase on skipped records, byte-truncated at the write site to
the column's 4000-char budget. It is raw by design -- the module's tests pin
exact transport strings such as `smtp: connection refused` -- never a stack
trace, never a message the module synthesized.

At-most-once, honestly stated: across the RETRIES of one delivery the
pipeline converges without a second send -- the job probes the record's
`succeeded` state (and, for the inbox, the row's dedupe key) before any
attempt, and the UNIQUE `(tenant_id, idempotency_key)` index turns a
duplicate insert into the already-delivered answer. The guarantee does NOT
cover every crash and concurrency shape: a process that dies after the
transport accepted a send but before the record write leaves no `succeeded`
row, and the retry re-sends; two concurrently running attempts of one
dispatch can both probe before either writes. Those double-send windows are
the price of the record being written after the transport call, and later
rounds narrow them (a provider receipt id column already exists on
`SendRecord`, unused by any transport this round).

Delivery jobs never assume worker context: the job rebuilds the tenant from
the enqueued job's own field before any repository call, exactly as the
platform's jobs discipline requires (a tenant-less attempt fails closed, and
the module's `TestDelivery_*` suite includes the tenant-less-worker case).

## Consent and verification

External recipients (a patient of a dental group, say) exist only inside the
tenant that verified them. `VerifiedContact` is one consent-gated address on
one channel (`ChannelEmail` or `ChannelSMS`; contacts are never `in_app` --
in-app recipients are users), with the address encrypted at rest and blind
indexed. Consent arrives two ways:

- Double opt-in: `CreateContact` opens a `pending` row and sends a code; the
  code is 6 decimal digits from `crypto/rand`, valid 5 minutes, one pending
  code per contact, stored as its SHA-256 hash with its expiry in columns on
  the verified_contacts row itself -- never a separate table, never the
  plaintext (see "The verification code rides on the contact row").
  `VerifyCode` is a compare-and-swap: only the pending row's own code
  verifies it, the row's status -- never the columns -- is what makes a
  consumed or superseded code unusable, and concurrent verifies race on the
  CAS rather than both succeeding.
- Business attestation: a host-side flow (an in-person signup, an existing
  relationship) attests an address by creating the contact with a non-empty
  `ConsentRef` (`ContactCreateInput`): the row lands already `verified`,
  the ref on its `consent_ref` column, and an audit event
  (`notification.contact.attested`) records the transition, the record
  carrying the context's actor. Attestation never re-opens an existing row:
  a duplicate create -- whatever status the existing contact holds --
  returns that row unchanged, so an attestation cannot resurrect an
  unsubscribed or bounced address (per-contact permanence), and an
  in-flight double opt-in (a `pending` row) keeps its code, never silently
  flipped.

The status machine: `pending --verify--> verified --unsubscribe-->
unsubscribed`; `pending --resend--> pending`; permanent transport failure
(`ErrTransportPermanent`) marks the tenant's own contact `bounced`. Terminal
states are per-contact (see "Unsubscribe is permanent for the contact as a
whole"). Delivery refuses unsubscribed and bounced contacts before any
transport is touched (`EnsureDeliverable`), and a user recipient whose
addresses have no email/phone simply has those channels skipped with a
recorded reason -- never failed, never a dispatch refusal at enqueue.

Every consent transition commits first, then emits its audit action through
`dbkit/audit`'s declarative `Emit`; an emit failure returns an internal error
to the caller (the transition happened but its outliving record did not --
the caller is the only sink, see Rules). The idempotent repeat of an
unsubscribe (already unsubscribed) emits nothing: it is not a state change.

Verification-code sending and verification are rate limited on two
`go/ratelimit` dimensions -- per tenant and per address, the latter keyed by
the blind index, never the plaintext. `platform_blacklist` exists so a
platform-level record of a bad address has a home before any writer needs it;
the reason vocabulary mirrors the two ways an address proves undeliverable
(`complaint`, `hard_bounce`). This round ships the table, the repository and
the cross-tenant `IsBlacklisted` read; no writer and no caller exist yet (see
"Deferred to later rounds").

## Adjudications

The consent, verification and seam decisions below were argued out during
this round's design reviews. They are restated here in the module's own
words, because code comments across the package cite this section by name
and must not need the review artifacts those decisions originally lived in.

### Unsubscribe is permanent for the contact as a whole

An unsubscribe is for the contact -- one address on one channel -- for good:
`unsubscribed` is terminal, and the row keeps the consent facts of its
former life so a re-attestation cannot silently resurrect a messenger the
recipient told to stop. There is no per-type or per-message-category
opt-out: `verified_contacts` has no `type_key` column, and type-scoped
opt-out is a deferred later-round shape (see below). Delivery's send-time
gate (`EnsureDeliverable`) reads the terminal status fresh on every attempt.

### Every consent and address decision is re-checked at send time

A dispatch must not fail because an address, a consent or a preference is
missing at ENQUEUE time: enqueue validates only what the payload itself
requires (type key, recipient class and id, the user recipient's locale).
Everything the delivery depends on that lives outside the payload -- the
host's address resolution for a user, the contact's status and consent for
an external recipient, the recipient's channel preferences -- is read by
the delivery job at SEND time. The platform blacklist is deliberately not
among them this round: its writers are deferred and nothing in the
delivery pipeline consults it (see "Platform-blacklist writers and bounce
remediation" under "Deferred to later rounds"). This is what makes a
static-table demo resolver and a real profile-store resolver
interchangeable for the module, and what makes the module's own gates the
ones that actually protect the recipient.

### The verification code rides on the contact row

The design keeps exactly one pending code per contact, riding on the
`verified_contacts` row itself: `verification_codes` is deliberately not a
table. A row in `pending` carries the code's SHA-256 hash and expiry in its
own columns; every other status leaves those columns as inert dead data --
the status gate, never the columns, is what makes a consumed code unusable.
One row, one code, no join, and no table whose rows can outlive the contact
they verify.

### SMS stays an in-package seam

The module declares its own `SMSSender` interface rather than consuming a
pkgcore seam, because pkgcore ships no SMS seam. The interface is
structurally identical to authn's own sender, so a host's wiring can hand
the same implementation to both modules without either importing the other
(the console sender and any HTTP sender a host implements satisfy both).
Promoting SMS to a pkgcore seam alongside `Mailer` -- with a registry,
presets and capability declarations -- is a later-round pkgcore change (see
"Deferred to later rounds"); until then the interface lives here and the
host's implementation is all the module ever calls.

### External contacts render in the platform default locale

A `Dispatch` carries the recipient's negotiated locale, which the caller
knows and the module never guesses. For a user recipient the locale is
REQUIRED (validate refuses an empty one: a delivery in the wrong language is
worse than a failed one, and the module's copy rule forbids silent
fallback). For an external contact the locale field is ignored: a contact
row carries no locale column, and the contact's copy renders in the
platform default locale (the same deferral `renderContactCode` documents).
Per-contact locale negotiation and its reconciliation with already-rendered
copy are a later-round change (see below).

### Separate index keys from the cipher key

The blind-index key that makes an encrypted contact address queryable lives
on the indexers the host injects (`WithContactEmailIndexer` /
`WithContactPhoneIndexer`) and must never be the encryption key of the
module's address cipher: a key compromise must not silently hand over both
confidentiality and queryability. The rule is mirrored from go/authn and
go/org, which store the same shape of identity data under the same two-key
discipline, and it holds for every rate-limit key and dedupe key the module
derives: they name the index hex, never the plaintext address.

## Deferred to later rounds

Each deferral below is recorded here so a code comment can point at it
instead of re-litigating the decision. What is NOT listed is not deferred:
if it is not in this section and not in "Known limitations", this round
claims it works.

- **Platform-blacklist writers and bounce remediation.** Nothing writes
  `platform_blacklist` this round, and nothing in the delivery pipeline
  consults it: the delivery job marks the TENANT'S OWN contact bounced
  (`MarkBounced`) on a permanent transport failure, leaving the platform
  list untouched. The writers -- the complaint webhook and the delivery
  job's hard-failure leg -- belong to later rounds, as does the
  remediation story for a bounced address: how an address that proved
  undeliverable is later re-proved (a fresh consent cycle, a platform-side
  review) is unsettled, and until it is, `bounced` stays terminal and the
  errors the attestation path raises on a bounced address stand (the
  "contact is bounced" refusal). The record's `reason` vocabulary
  (`complaint`, `hard_bounce`) is shipped now so the schema does not move
  when the writers land.
- **Type-scoped opt-out.** Unsubscribe is per contact, whole and permanent
  (see Adjudications); the finer shape -- "this type, not that one" -- needs
  a `type_key`-scoped design on the consent ledger and is deliberately not
  half-built here.
- **Per-contact locale negotiation.** External contacts render in the
  platform default locale (see Adjudications); giving a contact its own
  negotiated locale is a later-round change (a `locale` column, the
  negotiation path, and the reconciliation of copy already rendered under
  the default).
- **SMS as a pkgcore seam.** The in-package `SMSSender` interface (see
  Adjudications) moves to pkgcore alongside `Mailer` -- seam registry,
  preset entries, capability declarations and the host options that resolve
  it -- in a later pkgcore round; authn's HTTP sender and this module's
  console sender will register there and every host's wiring simplifies.
- **The platform-staff push consumer.** The hub's per-connection
  `Subscribe` returns connections a platform-staff shell will one day push
  to browsers or devices; that consumer is a later round's work. What ships
  is the hub itself, its `EventInboxCreated` subscription, and the HTTP
  stream that reads it per replica.

## Rules

Rules specific to this module, on top of the codebase-wide discipline:

- **Never import authn, rbac or org -- in any file.** A user is an opaque
  id; the org tree is unknown; host identity and addresses arrive through
  the seams above. The module's model files cite this for every id they
  store.
- **No service logging.** The module imports `go/observability` in exactly
  one place -- `handler.go`, whose `mustTenant` gate annotates every
  operation's span with the tenant. No service logs: every failure is
  RETURNED (an `apperr` code,
  a `send_records` row, an audit-emit error handed to the caller), because a
  service has no logger and the module will not hand one out. The caller is
  the only sink; the reference app's notes handler, which logs emit failures
  and returns success, is deliberately NOT this module's shape.
- **Copy ships in the declaring module's bundle.** Notification renders
  copy from the merged catalog the host assembled: the type's channel
  templates live in the declaring business module's own locale files under
  `<type_key>.<channel>.<part>` (render.go's convention) and its directory
  description under `<type_key>.description`. This module's own bundle
  (`locales/`) carries its error codes and the verification-code copy of
  contact.go's send-time rendering (`notification.contact.verify_code.*`),
  never a type's channel templates. A missing template id or a locale the
  catalog does not know is `ErrInternal.WithCause` -- never a
  fallback to another language, never a half-rendered message.
- **Addresses stay encrypted, indexed, and out of every sink.** The
  plaintext contact address never appears in a WHERE clause, a response
  body, an audit record, a log line, or a rate-limit key -- the blind index
  is the only form any of those see. The handler's contact-list response
  serves id/channel/status/created_at only.
- **Contacts are never in-app.** `verified_contacts` carries email and SMS
  only; the in-app channel belongs to user recipients (inbox rows), and the
  closed channel vocabulary is enforced at the preference boundary.
- **Preference writes are validated against the live taxonomy.** An
  unknown type, an unknown channel, a channel the type does not support,
  or an opt-out the type forbids are all refused with the preference
  group's codes -- never stored to be silently unreachable later.
- **Do not hand-write tenant filters.** The platform tables are the
  exception that proves the rule: their repositories query the plain
  `*gorm.DB` dbkit.Open returns (the documented identity/platform-data
  pattern), never `db.Table`/`db.Model`/`db.Raw`, and the ONE hand-written
  `WHERE tenant_id` in the module is `ByTenantAndKey`, which scopes by hand
  what the platform model cannot scope by plugin.

## Testing

Unit tests are per-file, run with `-race`, and cover the module's suites:
`repository_test.go` (including `tenancytest.AssertIsolated`), the
preference files' `AssertIsolated` suite, `contact_test.go` (the
`AssertIsolated` suite, the double opt-in lifecycle, the code CAS, both
rate-limit dimensions, address-at-rest encryption, terminal-state
permanence), `blacklist_test.go` and `send_record_test.go`
(`tenancytest.AssertNotTenantScoped` over the platform tables),
`delivery_test.go` (the retry/converge/skip/deferral semantics, transport
permanence marking contacts bounced, the resolver-failure and
tenant-less-worker paths), `handler_test.go` and `hub_http_test.go` (the
HTTP surface, driven through a real httptest server), `hub_test.go`,
`module_test.go` (Register's validation and wiring), `errors_test.go` and
the per-file suites. Godoc `ExampleInboxMessage` and
`ExamplePreferenceService` compile and run in the unit suite, pinning the
documented host wiring (six seams, a hand-built `pkgcore.NewRegistry` host)
against the real API.

The Docker-backed integration tier lives in `integration_test/` (run as
`go test -tags=integration ./integration_test/...` from the module dir): a
PostgreSQL leg that applies the module's postgres migration set from zero
and re-runs the isolation suites against a real server, and a Redis leg
proving an inbox delivery announced on one replica's bus reaches the other
replica's hub (the cross-replica shape the unit tier cannot compose). The
same directory carries the `clinic` demo module, an in-tree consumer that
declares its own notification type and exercises the module the way a
business module would. The reference app's `notification_flow_test.go` is
the end-to-end consumer proof through the composed HTTP stack. `go vet`,
`golangci-lint run ./...` and `tools/scan_cjk.py` (no CJK outside
`docs/internal/`) apply to this module like every other.

## Known limitations

- The inbox stream (`GET /api/v1/notifications/stream`) sends no heartbeat:
  a connection that survives with no announcements is indistinguishable from
  a dead one until a proxy or the client times it out. Heartbeats are a
  deliberate non-goal of this round; the route's doc comment says so.
- At-most-once holds across a single delivery's retries (see "Delivery
  pipeline"); the crash-between-transport-and-record and
  concurrent-attempt double-send windows are recorded there and in
  `send_record.go`, not silently designed around.
- Audit emit is synchronous with the consent transition's commit, and the
  idempotent repeat paths of the contact operations emit nothing (they are
  not state changes); a transition whose emit failed is returned to the
  caller as an internal error and is not re-emitted by a retry of the
  transition itself.
