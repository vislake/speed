# notification

`go/notification` is the platform's outbound-message module: it delivers a
tenant's notifications to the people it serves, on the channels those people
want (the in-app inbox, email, SMS), and -- for contacts who are not users of
any tenant -- only where consent has been verified first. This file is the
module-level operating guide that ships to consuming projects.

## Status

Implemented end to end: the in-app inbox, the per-type channel preference
matrix, the external-contact consent ledger with double opt-in and business
attestation, the type-scoped opt-out (a verified contact narrows itself
out of one notification type while staying reachable for the rest --
contact_type_unsubscribe.go), the async delivery pipeline with its
per-attempt send records,
the platform-blacklist table (schema and read path; writers deferred, see
below), the module's own OpenAPI fragment with a generated, compile-checked
HTTP handler, the per-replica realtime hub behind the inbox stream, and the
dual-dialect migration set. The reference app (`examples/reference-app`) is
the mandatory first consumer: `internal/app/server.go` wires `notification.NewModule`
through `Kernel.Bootstrap`, `internal/app/demo_notification.go` supplies the
host-side demo seams, and `flowtests/notification_flow_test.go` drives the
composed HTTP stack through the module's surfaces.

Nothing here is a stub. The "Not implemented" section below is the complete,
honest list of what the module does not ship.

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
  re-dispatch onto the already-settled record -- an identical re-dispatch
  absent a fresh per-occurrence marker (`Dispatch.OccurrenceID`), which a
  deliberate resend sets so its delivery is a new occurrence with its own
  record; a dispatch's locale participates in the derived key the same
  way, so a resend rendered in a new locale delivers in it; nothing
  aggregates or rate-limits routine deliveries (the module's
  rate limits cover verification-code sends and the consent-create path
  only) -- those delivery-path shapes are absent (see below).
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
- `contact.go`, `contact_code.go`, `contact_type_unsubscribe.go`,
  `repository`-adjacent queries -- the
  consent ledger: `VerifiedContact`, the status machine, the double opt-in
  code lifecycle, attestation, unsubscribe/mark-bounced, `EnsureDeliverable`,
  and the audit emission of every consent transition. The type-scoped
  opt-out half lives in `contact_type_unsubscribe.go`: the
  `contact_type_unsubscribes` table (one terminal row per
  (tenant, contact, type)), the write path `UnsubscribeType` (verified
  contacts only, validated against the live taxonomy and the type's
  `Unsubscribable` declaration), and the delivery gate
  `EnsureDeliverableForType` -- the type-aware twin of `EnsureDeliverable`,
  which both answers the whole-contact status gate and probes the opt-out
  rows, so the delivery job re-checks a contact's per-type narrowing at
  send time exactly as it re-checks the contact's status.
- `blacklist.go` -- `PlatformBlacklist` (platform data) and its repository's
  cross-tenant `IsBlacklisted` read.
- `send_record.go` -- `SendRecord` (platform data) and its repository
  (`ByTenantAndKey` probe, upsert `Save`), the outcome log the delivery job
  converges on.
- `delivery.go` -- `Dispatch` and the delivery pipeline: the async
  `DeliveryService.Dispatch`, the single registered queue handler
  (`jobTypeDeliver` = `"notification.deliver"`), send-time rechecks,
  rendering, per-channel attempt and record settling.
- No SMS file of its own: the SMS transport is the pkgcore seam, consumed
  not declared -- the delivery pipeline and the contact service send
  `pkgcore.SMS` values through the host-wired `pkgcore.SMSSender`
  (`WithSMSSender`), with `pkgcore.NewConsoleSMSSender` as the
  zero-external-dependency implementation that doubles as the module's
  test double (see "The SMS seam is pkgcore's").
- `render.go` -- the template-render seam: per-channel part shapes and the
  `<type_key>.<channel>.<part>` id convention.
- `staticaddr/` -- the subpackage implementing `UserAddressResolver` over
  a fixed per-user table (`staticaddr.New(map[string]notification.UserAddresses)`,
  copied at construction, read-only and concurrency-safe thereafter). It
  serves the deployment shapes whose addresses are statically held -- demos,
  a single-tenant install whose operator holds the accounts, tests -- and
  its package doc carries the seam's obligation: the table is the
  operator's declaration of VERIFIED addresses, and a host whose addresses
  change (users rebinding, a verification flow) must implement its own
  resolver over its own address store. An implementation never shares a
  package with the interface it implements, so it lives beside the module
  root, not in it.
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
  marking a hard transport failure calls), the type-scoped opt-out write
  `UnsubscribeType` (see "Type-scoped opt-out" below), and the delivery
  gates `EnsureDeliverable` / `EnsureDeliverableForType`.
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
stream opens, and the connection subscribes to the hub scoped to that exact
pair (`SubscribeFor`), so the hub itself drops every announcement for another
recipient or another tenant at publish time, before it could occupy the
connection's buffer -- a flood of other tenants' announcements can never crowd
the caller's own out of its buffer. The route carries no heartbeat (see
"Known limitations").

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
- Audit: four consent-transition actions under `notification.contact.*` --
  `attested`, `verified`, `unsubscribed` and the type-scoped
  `type_unsubscribed` (whose record names the narrowed type in its change,
  so the trail can reconstruct which type a contact opted out of) --
  registered on `reg.AuditActions`
  in `Register`. The audit `Resource`'s display name is the channel plus the
  address's blind index: the plaintext address must never reach the audit
  trail, whose records outlive the row.
- Permissions: none declared, and the module mounts no admin surface.
- Types: none declared (see "What it deliberately does NOT own").

### Tables and data domains

Four migrations per dialect (`sqlite/`, `postgres/`), each tested to apply
from zero against a real PostgreSQL server in the module's integration tier:

| Table | Data domain | Isolation proof |
| --- | --- | --- |
| `in_app_messages` | tenant | `tenancytest.AssertIsolated` |
| `notification_preferences` | tenant | `tenancytest.AssertIsolated` |
| `verified_contacts` | tenant | `tenancytest.AssertIsolated` |
| `contact_type_unsubscribes` | tenant | `tenancytest.AssertIsolated` |
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

- `WithSMSSender` -- the SMS transport (see "The SMS seam is pkgcore's").
- `WithMailFrom` -- the from-address the contact-email path sends as.
- `WithContactEmailIndexer`, `WithContactPhoneIndexer` -- the blind indexers
  that make an encrypted contact address queryable (see "Separate index keys
  from the cipher key"). A host builds them with `dbkit.NewBlindIndexer`
  over the module's exported `AddressIndexColumn` -- the blind-index
  column's exact SQL name, carried as a referenced constant rather than a
  hand-typed string (see that constant's doc comment for why dbkit refuses
  an empty name but cannot guard a wrong one; the reference app is the
  mandatory consumer). The HMAC key behind them is declared, once, on the
  `Registry.Bootstrap` seat as `notification.contact_index_key` (`hexkey`,
  Sensitive) -- one key serves both indexers, exactly as the options'
  contract allows, and it stays separate from every cipher key. Register
  states the contract; the host resolves the value and injects the indexers,
  and the module never reads the environment.
- `WithDeliveryQueue` -- the `jobs.Queue` delivery jobs are enqueued on and
  the registered handler consumes.
- `WithUserAddressResolver` -- the host's read of a user recipient's
  outbound addresses at send time (see "Every consent and address decision is
  re-checked at send time"). A host whose addresses are statically held can
  wire the module's own `staticaddr` subpackage (`staticaddr.New`); a host
  whose addresses change implements the interface over its own store.

The host also supplies structural, no-import seams the module consumes as
interfaces it declares -- never as imported packages: `SubjectResolver` (the
HTTP caller's identity, per operation, with `SubjectResolverFunc` as its
func-to-interface adapter so a host wires a closure), `UserLocaleResolver`
(OPTIONAL, via `WithUserLocaleResolver`: the caller's stored locale, the
type directory's fallback tier behind the request's Accept-Language -- a
nil or failing resolver skips the tier with the platform default behind
it, never a request failure; the reference app wires its authn adapter),
`UserAddressResolver`
(above), the encrypted-address serializer registration (the module's own
`RegisterContactAddressSerializer(cipher)` binds the exported
`ContactAddressSerializerName` before any contact row is read or written,
following org's and pki's precedent, and refuses a nil cipher rather than
registering one that would store plaintext), the two blind-index
constructors (`NewContactEmailIndexer`/`NewContactPhoneIndexer`, each over
the module's own `AddressIndexColumn` and channel normalizer), and the
registry slices (`handlerHost`/`contactHost`/`deliveryHost`) through which
the services read the merged catalog, the bus and the mailer. The host
implements, the module consumes; a hand-built host in a test satisfies the
same interfaces.

## Delivery pipeline

`Dispatch` is async by construction: `Dispatch()` only validates and
enqueues (the payload is also the job payload, so its JSON shape is part of
the queue contract), and every decision that can change between enqueue and
delivery -- the recipient's channel preferences, an external contact's
consent and status, the addresses on file -- is re-checked at send time by
the job, never frozen into the payload. The type registry is part of that
send-time recheck on BOTH recipient paths: a user delivery resolves its
channels through `ResolveForDelivery` (which refuses an undeclared type
before any channel exists), and a contact delivery consults the same live
registry before anything renders, terminal-refusing an undeclared type and
recording the refusal under the contact's own channel -- a message that
went out for a type nobody declared would bypass the preference matrix,
the unsubscribe decision and the declared default channel strategy the
declaration owns, and the refusal cannot wait for the renderer, because an
undeclared type whose templates happened to exist would render fine. The
job renders the type's copy for the recipient's locale from the merged
catalog (send time, never enqueue time; a render can never precede the
recheck that might have skipped it), writes the in-app row and/or drives
the email/SMS transports, and settles one `send_records` row per attempted
channel.

The params channel is the one dispatch surface that reaches the recipient
verbatim: `Dispatch.Params` persists into the in-app inbox row and comes
back out through the inbox API, so it may carry only what the type's own
copy interpolates -- never delivery-internal context (an operator's
free-text justification, an actor's user id). Every type now states that
surface explicitly through its declaration's `RecipientVisibleParams`
(pkgcore): a non-nil list, the empty one included, is
authoritative, and `Dispatch` refuses any dispatch carrying a parameter
outside it before anything is enqueued (`ErrDispatchParamsNotAllowed`,
code `notification.dispatch_params_not_allowed`, naming the type in
"type_key" and the offending keys in "params"); the delivery path
independently narrows a payload that nevertheless reaches it -- a job
enqueued before the declaration restricted its params -- down to the
declared set (delivery.go's `recipientVisibleOnly`, applied before
anything renders, before the delivery key is derived and before the row
is written), so a stale in-flight payload can no more leak internal
context than a fresh dispatch can, and the derived key never depends on a
parameter the declaration has forbidden. A type whose declaration leaves
the list nil (the pre-annotation legacy value) stays unrestricted, byte
for byte as before. The delivery suite pins both boundaries:
`TestDelivery_Dispatch_RefusesParamsOutsideRecipientVisibleDeclaration`
covers the dispatch refusal, and
`TestDelivery_StalePayloadParams_NarrowedBeforeRowAndKey` the stale-payload
narrowing; the wire code and its HTTP status ride errors_test.go's
literal code table.

Copy governance is structural rather than prose: a parameter no copy
template of the type references renders into nothing yet would still
round-trip through the row, so `Dispatch` refuses it before anything is
enqueued (`ErrDispatchParamsUnreferenced`, code
`notification.dispatch_params_unreferenced`, naming the type in "type_key"
and the offending keys in "params") -- applied to every DECLARED type
whether or not its declaration restricts its recipient-visible list
(declaration governs exposure, copy governs use, and a parameter must
satisfy both; a type the registry does not declare is not judged here,
its delivery being refused anyway by the delivery path's own
undeclared-type gate) -- and a user delivery's channel leg narrows a
stale in-flight payload that reaches it -- a job enqueued before its
type's copy gate restricted the params -- down to the parameters that
channel's own copy renders (render.go's `copyParamsForChannel`, a removal
probe: a parameter whose removal leaves every rendered part byte-identical
is dropped) before the delivery key derives or anything renders or
persists, so the in-app row stores exactly the parameters its own copy was
rendered from and the key never depends on a copy-inert parameter.
Distinguishing two otherwise identical deliveries is `OccurrenceID`'s
first-class job, never a copy-inert parameter's. The delivery suite pins
both boundaries: `TestDelivery_Dispatch_RefusesParamsNoTemplateReferences`
covers the dispatch refusal over an unrestricted AND an over-declared type
(a restricted declaration that itself lists the unreferenced parameter),
and `TestDelivery_StalePayloadParams_UnreferencedParam_DroppedBeforeRowAndKey`
the stale-payload narrowing.

Record semantics (`send_record.go`): `succeeded` is written only after the
transport accepted the send, `failed` after a failure exhausted an attempt,
`skipped` after a deliberate non-send whose reason will not change by
retrying -- no address on file, an external contact whose consent lapsed.
`Error` carries only the module's bounded vocabularies: on failed records one
of the `failureReason*` classifications the settle site chose (delivery.go's
fail-and-retry / fail-and-stop paths), on skipped records a short
`skipReason*` phrase, and the empty sentinel on succeeded ones. The raw text
of a failure is deliberately never stored, whatever its origin: no
caller-side redaction stands between transport errors and the column,
because a transport echoes the address in whatever form IT chose, not
reliably the normalized form the module handed it, so substring-based
redaction would systematically miss the most likely inputs and the column
could still carry plaintext PII. The record therefore stores a
classification instead of any text (delivery.go's `classifiedError` keeps
the original cause reachable through Unwrap for the job's
`errors.Is`/`apperr.As` signals); `send_records` is a platform table with
no deletion path, and its operator-facing reads see only the bounded
vocabulary. The module's tests pin the classification values, never
transport strings.

At-most-once, honestly stated: across the RETRIES of one delivery the
pipeline converges without a second send -- the job probes the record's
`succeeded` state (and, for the inbox, the row's dedupe key) before any
attempt, and the UNIQUE `(tenant_id, idempotency_key)` index turns a
duplicate insert into the already-delivered answer. The guarantee does NOT
cover every crash and concurrency shape: a process that dies after the
transport accepted a send but before the record write leaves no `succeeded`
row, and the retry re-sends; two concurrently running attempts of one
dispatch can both probe before either writes. Those double-send windows are
the price of the record being written after the transport call, and they
stay open (a provider receipt id column already exists on `SendRecord`; no
transport returns one).

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
  plaintext (see "The verification code rides on the contact row"). A code
  send whose transport refuses is reported through
  `ErrContactCodeDeliveryFailed` carrying the bounded transport
  classification as its cause text (contact.go's `sendCode`, through the
  same `classifyTransportCause` the delivery paths use -- the raw cause
  stays reachable through Unwrap, so the permanent signal survives
  `errors.Is`): the code-send payload carries the plaintext code to a
  not-yet-verified address, so its error -- and any future diagnostic log
  that renders it -- must never carry the address in any form either.
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

The user-recipient path deliberately carries none of this ledger: a
user's addresses are identity data the host's own authn half owns and
verifies, and this module never imports authn, so it reads them at
send time through the `UserAddressResolver` seam and holds no
user-address consent or verification state of its own. The seam's
contract (delivery.go's `Resolve` doc comment) requires the resolver
to return the host's own verified addresses for that user, and this
module performs no consent check on user addresses, unlike the
`VerifiedContact` path above, whose full ledger exists precisely
because an external contact has no host-side identity store to hold
verification. The asymmetry is written down because it is the module's
one delegated-away safety obligation: the
never-send-to-an-unverified-address rule is enforced in code on the
contact side and by contract on the user side, and a host whose
resolver serves unverified addresses bypasses it with nothing in this
module detecting the bypass.

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
(`complaint`, `hard_bounce`). The table, the repository and the
cross-tenant `IsBlacklisted` read ship; no writer and no caller exist yet
(see "Not implemented").

## Adjudications

The consent, verification and seam decisions below are recorded in the
module's own words, so every part of the package states the same rule.

### Unsubscribe is permanent for the contact as a whole

An unsubscribe is for the contact -- one address on one channel -- for good:
`unsubscribed` is terminal, and the row keeps the consent facts of its
former life so a re-attestation cannot silently resurrect a messenger the
recipient told to stop. Delivery's send-time
gate (`EnsureDeliverable`) reads the terminal status fresh on every attempt.

### Type-scoped opt-out: terminal per (contact, type), never a whole-contact status

The consent ledger's second, finer shape (contact_type_unsubscribe.go)
answers "this type, not that one": a verified contact keeps receiving
everything but one notification type. The shape is a per-(tenant, contact,
type) row in its own table, `contact_type_unsubscribes`, because a contact
may narrow many types one at a time and each narrowing is its own consent
fact with its own audit record -- the per-(entity, type) row shape
`notification_preferences` already uses for per-user type state, never a
column on `verified_contacts` (a set-in-a-cell would need a read-modify-write
on a shared row, the race the module's row-per-fact tables exist to avoid).
The rows are terminal for as long as the contact row lives -- no re-enable
path exists, mirroring the whole-contact rule, and re-consent is a fresh
contact cycle. UnsubscribeType (the write path) accepts only a VERIFIED
contact: a pending contact has no consent to narrow, a whole-unsubscribed
or bounced contact is already covered by its broader terminal status
(`ErrContactNotVerified`/`ErrContactUnsubscribed`/`ErrContactBounced`
respectively), a type nobody declared is refused (`ErrTypeNotFound`), and a
declared type whose declaration does not permit opting out -- `Unsubscribable`
false -- is refused (`ErrContactTypeOptoutNotAllowed`), the identical
taxonomy discipline the preference matrix applies to a user's per-type
opt-out. The whole-contact unsubscribe remains the one exit that predates
this shape and is not touched by it. The write transition is idempotent
(the repeat emits no second row and no second audit event) and is audited
under `notification.contact.type_unsubscribed` with the narrowed type named
in the change. Delivery consults the rows through
`EnsureDeliverableForType`, the type-aware twin of `EnsureDeliverable` the
delivery job calls for every contact send: a type-scoped opt-out landing
between enqueue and attempt refuses the delivery at send time exactly as a
whole-contact status change does, and the job settles the refusal as a
skipped record under its own skip reason ("contact unsubscribed from this
type"), so an operator reading the send records can tell a per-type
narrowing from a whole-contact withdrawal.

### Every consent and address decision is re-checked at send time

A dispatch must not fail because an address, a consent or a preference is
missing at ENQUEUE time: enqueue validates only what the payload itself
requires (type key, recipient class and id, the user recipient's locale).
Everything the delivery depends on that lives outside the payload -- the
host's address resolution for a user, the contact's status and consent for
an external recipient, the recipient's channel preferences -- is read by
the delivery job at SEND time. The platform blacklist is deliberately not
among them: its writers are unbuilt and nothing in the delivery pipeline
consults it (see "Not implemented"). This is what makes a static-table
demo resolver and a real profile-store resolver interchangeable for the
module, and what makes the module's own gates the ones that actually
protect the recipient.

### The verification code rides on the contact row

The design keeps exactly one pending code per contact, riding on the
`verified_contacts` row itself: `verification_codes` is deliberately not a
table. A row in `pending` carries the code's SHA-256 hash and expiry in its
own columns; every other status leaves those columns as inert dead data --
the status gate, never the columns, is what makes a consumed code unusable.
One row, one code, no join, and no table whose rows can outlive the contact
they verify.

### The verify budget charges before the code is judged

`VerifyCode` (contact.go) pays the per-address and per-tenant verify
budgets (`checkCodeVerifyLimit`) before the typed code is compared: status
gate, then the budget charge, then code shape, expiry and hash. The order
is deliberate, and it is pinned at the HTTP surface by the reference app's
acceptance test `TestNotificationFlow_VerifyCodeRateLimit_FailsClosed`
(`examples/reference-app/flowtests/notification_flow_test.go`): ten wrong
guesses are ten 400s that each spend one of the address's ten guesses per
code lifetime (`contactCodeVerifyPerAddress`), and the eleventh attempt --
carrying the correct code -- is refused 429 rather than honored. A resend
that issues a fresh code changes nothing: the budget is the address's
guess allowance, not the code instance's. Charging per ATTEMPT rather than
per wrong attempt is the budget-side companion of the identical-failure
rule -- the module never classifies an attempt's failure to decide
anything, budget included -- and it keeps the brute-force bound of a
6-digit code honest for the outsider the code protects against.

Sharing's access endpoint charges its per-token budget only after a wrong
credential is judged; this module deliberately does NOT follow, and the
divergence is the surface, not the mechanism. Sharing's access endpoint is
anonymous: anyone who holds or guesses a share token can burn that token's
budget with no account at all, so the rate-limit shape is the whole
defense against a denial that needs no identity. Notification's verify
endpoint is not anonymous: every `/api/v1/notifications` operation reads
the tenant from the request context, `VerifyCode`'s own tenant-scoped read
fails closed without one, and in the compositions this module documents
(the reference-app chain, whose tenancy allowlist names no notifications
path) that context resolves only from a verified principal -- the caller
who can reach and exhaust an address's budget is an authenticated member
of the tenant that owns the address. Sustained denial by such a member is
a harm inside the tenant's own trust boundary, borne by the tenant's
governance of its members and by the module's audited consent
transitions, not by the rate-limit shape. That is the ruling this module
records, stated honestly: for an anonymous or pre-auth surface the shape
would be indefensible, which is exactly why sharing's is fixed and this
one is not.

The ruling flips if the surface changes: a verify endpoint moved to an
unauthenticated surface or placed on a pre-auth allowlist makes the
mechanism identical to sharing's, and it must then be fixed the way
sharing fixed it -- not re-ruled here.

### The SMS seam is pkgcore's

The SMS transport this module sends verification codes and sms-channel
deliveries through is pkgcore's own seam -- `pkgcore.SMS` and
`pkgcore.SMSSender` -- shared with go/authn's phone-login flow, so a host
wires ONE implementation to both modules' sender options
(`authn.WithSMSSender` here and `WithSMSSender` there) with neither module
importing the other. The implementations are pkgcore's too, all reachable
below this module: `pkgcore.NewConsoleSMSSender` (the
zero-external-dependency console transport, which doubles as this module's
test double), `pkgcore.NewHTTPSMSSender` (an operator-run JSON gateway, the
distributed-mode transport the reference app wires), and the three real
carrier adapters `pkgcore/sms/aliyun`, `pkgcore/sms/tencent` and
`pkgcore/sms/twilio` (each a host-constructed `NewSender`). The seam has no
pkgcore registry, preset or capability seat -- `pkgcore.SMSSender`'s own
doc comment records why: no consumer resolves its SMS transport from the
kernel, host injection through module options is the whole wiring, and each
consumer keeps its own wiring-time requirement on the sender. This module's
requirement is the strictest of the two: `Register` refuses to boot without
a wired sender (`ErrSMSSenderRequired`), where authn's refusal applies only
under the distributed deployment mode.

### The copy language comes from the recipient's chain, never a guess

Every rendered copy resolves its language through one of two chains, and
the platform default (`en-US`, `platformDefaultLocale`) is always the
terminal tier:

- Recipient is a user: the `Dispatch`'s `Locale` -- the recipient's stored
  language, which the caller (the host's profile store) knows -- is
  REQUIRED (validate refuses an empty one: a delivery in the wrong language
  is worse than a failed one, and the module's copy rule forbids silent
  fallback).
- Recipient is an external contact: a contact row carries no locale, so
  the `Dispatch`'s `Locale` is the language of the REQUEST that created
  the dispatch -- the producer captures the requester's language at
  creation time and passes it through, because the requester is present
  and the contact cannot speak for itself. An empty value (a producer with
  no request behind the dispatch: a scheduled batch, an internal job)
  renders the platform default.
- The verification code's synchronous sends (create, resend) apply the
  same requester rule at their call sites: the HTTP layer captures the
  request's negotiated language into `ContactCreateInput.Locale` /
  `ResendCodeInput.Locale`, and an empty capture renders the platform
  default (`renderContactCode`).
- The type directory (requester is recipient) runs header → stored
  profile (`UserLocaleResolver`) → platform default -- see
  `directoryLocale`.

The delivery dedupe key keys on `deliveryLocale(d)` -- the locale the copy
actually renders in -- so a language change between two otherwise
identical dispatches is the distinct delivery it is, and an unset locale
and an explicit platform-default locale are ONE delivery (both render the
same copy, both derive the same key: no migration, no double-send).

Per-contact locale negotiation beyond the captured requester language
(the contact choosing its own language, and the reconciliation of
already-rendered copy) is not implemented (see below).

### Separate index keys from the cipher key

The blind-index key that makes an encrypted contact address queryable lives
on the indexers the host injects (`WithContactEmailIndexer` /
`WithContactPhoneIndexer`) and must never be the encryption key of the
module's address cipher: a key compromise must not silently hand over both
confidentiality and queryability. The rule is mirrored from go/authn and
go/org, which store the same shape of identity data under the same two-key
discipline, and it holds for every rate-limit key and dedupe key the module
derives: they name the index hex, never the plaintext address.

## Not implemented

Each entry below records functionality the module does not implement. What
is NOT listed is not absent: if it is not in this section and not in
"Known limitations", the module claims it works.

- **Platform-blacklist writers and bounce remediation.** Nothing writes
  `platform_blacklist`, and nothing in the delivery pipeline consults it:
  the delivery job marks the TENANT'S OWN contact bounced (`MarkBounced`)
  on a permanent transport failure, leaving the platform list untouched.
  The writers -- a complaint webhook and the delivery job's hard-failure
  leg -- do not exist, and neither does the remediation story for a
  bounced address: how an address that proved undeliverable is later
  re-proved (a fresh consent cycle, a platform-side review) is unsettled,
  so `bounced` stays terminal and the errors the attestation path raises
  on a bounced address stand (the "contact is bounced" refusal). The
  record's `reason` vocabulary (`complaint`, `hard_bounce`) is shipped so
  the schema does not move when the writers land.
- **Per-contact locale negotiation.** External contacts render in the
  language captured from the dispatching request (see Adjudications); a
  contact has no negotiated locale of its own -- no `locale` column, no
  negotiation path -- and no reconciliation exists for copy already
  rendered in an earlier language.
- **The platform-staff push consumer.** No platform-staff push consumer
  exists for the hub's per-connection `Subscribe` connections (such a
  consumer would subscribe unscoped and do its own recipient routing,
  exactly as the inbox stream's `SubscribeFor` scoping keeps one
  recipient's stream to its own announcements). What ships is the hub
  itself, its `EventInboxCreated` subscription, and the HTTP stream that
  reads it per replica.
- **Tenant-enforced preference tiers.** The preference matrix ships two of
  the design's three tiers (personal settings > tenant-enforced policy >
  type default): a recipient's own per-type channel choices, and the
  type's declared defaults when a recipient has none. The middle tier -- a
  tenant administrator forcing a channel for a type across all of that
  tenant's recipients (security alerts, billing overdues), where personal
  settings must not win -- is not implemented: there is no tenant-scoped
  override table, no merge at preference-read time, and no write surface
  beyond the current single-key update. The matrix's own two tiers are
  all the rows express, and a type that must reach its recipients is
  declared unsubscribable.
- **Same-type aggregation and delivery rate limiting.** Nothing aggregates
  or rate-limits the delivery path: the module's rate limits gate
  verification-code sends and the consent-create path, and the delivery
  job sends every dispatch through as rendered (replay dedupe collapses
  identical re-dispatches only; a deliberate resend dispatches under a
  fresh per-occurrence marker and delivers). Same-type aggregation -- a
  short burst of one type coalescing into few messages, a bulk-import
  failure must not mean five hundred emails -- and the per-type delivery
  limits that go with it are not implemented: the aggregation window, the
  merged message shape, and where the limits sit are all unsettled.
- **Admin template editing.** A type's channel templates live in its
  declaring module's own locale files (see the Copy rule above), declared
  next to the type and rendered from the host's merged catalog at send
  time; nothing stores them here for an operator to edit. No
  operations-console editing and preview surface exists for that copy:
  there is no template store the declaration ids can be re-pointed at, and
  no registry-driven preview. The registry's other two driven surfaces --
  the preference page's automatic rendering and the documentation
  generation -- are likewise absent with the frontend below.
- **The `@speed/notification-ui` frontend.** No notification-center
  frontend package exists -- no bell with the unread badge, no drop-down
  list, no message-center page, no preference-matrix table; the module's
  generated client half and the reference app's Go-side flows are what
  ship.

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
- **The language of a rendered copy is decided by the recipient's chain,
  never guessed, and the platform default is its last tier.** The two
  chains (user recipient: the dispatch's required `Locale`; external
  contact: the producer-captured requester language, empty meaning the
  platform default) are stated in Adjudications; what the rule adds is
  what a future surface must NOT do: invent a locale, read one from a
  header for a recipient who is not the requester, or skip the terminal
  default. The type directory's chain is header → stored profile → default,
  and its response carries `Vary: Accept-Language` so shared caches key on
  the request language; any future response whose copy varies by request
  language must carry the same header.
- **Addresses stay encrypted, indexed, and out of every sink.** The
  plaintext contact address never appears in a WHERE clause, a response
  body, an audit record, a log line, or a rate-limit key -- the blind index
  is the only form any of those see, and even the index never travels in a
  response body: a rate-limit refusal reports the denied dimension's NAME
  ("address" or "tenant", each entry's `name` field), never the KV key that
  embeds the index -- ErrContactRateLimited's doc is the protected
  contract, since a key echoed into params would turn the HMAC into an
  online oracle. The handler's contact-list response serves
  id/channel/status/created_at only.
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
rate-limit dimensions with their refusals pinning the name-not-key
reporting contract over service and HTTP surfaces alike, address-at-rest
encryption, terminal-state permanence), `contact_type_unsubscribe_test.go`
(the type-scoped opt-out table's own `AssertIsolated` suite, the write
path's verified-only and taxonomy refusals, the idempotent repeat that
emits nothing, the tenant boundary both ways, the type-aware gate's status
answers, and the opt-out's audit record naming the narrowed type),
`blacklist_test.go` and
`send_record_test.go`
(`tenancytest.AssertNotTenantScoped` over the platform tables),
`delivery_test.go` (the retry/converge/skip/deferral semantics, transport
permanence marking contacts bounced, the type-scoped opt-out's
enqueue-between-attempt refusal skipping exactly the narrowed type's
delivery while other types still deliver, the resolver-failure and
tenant-less-worker paths), `handler_test.go` and `hub_http_test.go` (the
HTTP surface, driven through a real httptest server), `hub_test.go`,
`module_test.go` (Register's validation and wiring), `errors_test.go` and
the per-file suites. Godoc `ExampleInboxMessage` and
`ExamplePreferenceService` compile and run in the unit suite, pinning the
documented host wiring (six seams, a hand-built `pkgcore.NewRegistry` host)
against the real API. The `staticaddr` subpackage carries its own suite
(`staticaddr_test.go`: table resolution, missing rows, the
construction-time copy, concurrent reads under `-race`) and its own
runnable `example_test.go`.

The Docker-backed integration tier lives in `integration_test/` (run as
`go test -tags=integration ./integration_test/...` from the module dir): a
PostgreSQL leg that applies the module's postgres migration set from zero,
re-runs the isolation suites against a real server, and executes the
delivery log's guarded rewrite (`SendRecordRepository.SaveGuarded`) through
the repository against real PostgreSQL -- the save path's only second-
dialect execution, pinning the statement's guard semantics and its
literal-bound succeeded sentinel on the second dialect -- and a Redis leg
proving an inbox delivery announced on one replica's bus reaches the other
replica's hub (the cross-replica shape the unit tier cannot compose). The
same directory carries the `clinic` demo module, an in-tree consumer that
declares its own notification type and exercises the module the way a
business module would. The reference app's `flowtests/notification_flow_test.go` is
the end-to-end consumer proof through the composed HTTP stack. `go vet`,
`golangci-lint run ./...` and `tools/scan_cjk.py` (no CJK outside
`docs/internal/`) apply to this module like every other.

## Known limitations

- The inbox stream (`GET /api/v1/notifications/stream`) sends no heartbeat:
  a connection that survives with no announcements is indistinguishable from
  a dead one until a proxy or the client times it out. Heartbeats are a
  deliberate non-goal; the route's doc comment says so.
- At-most-once holds across a single delivery's retries (see "Delivery
  pipeline"); the crash-between-transport-and-record and
  concurrent-attempt double-send windows are recorded there and in
  `send_record.go`, not silently designed around.
- Audit emit is synchronous with the consent transition's commit, and the
  idempotent repeat paths of the contact operations emit nothing (they are
  not state changes); a transition whose emit failed is returned to the
  caller as an internal error and is not re-emitted by a retry of the
  transition itself.
