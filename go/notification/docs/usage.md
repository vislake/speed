# go/notification usage guide

The platform's messaging surface: an in-app inbox, a per-recipient channel
preference matrix, a consent-gated external-contact ledger (email/SMS
addresses that are not app users), and the asynchronous delivery pipeline
that renders and sends every one of them. This is a template for the rest
of the implemented modules' own `docs/usage.md` files (see the per-module
census in the root `CLAUDE.md` for which modules that covers) -- the shape
below (Install → Configure → Quickstart → HTTP surface → Error codes → FAQ)
is deliberately generic, not specific to notification's own domain.

For the full design rationale (why a live type registry instead of a
template store, why delivery is asynchronous, why external contacts need
double opt-in) see `AGENTS.md` in this module's root. This file is the
"how do I actually wire this up and send something" guide; `AGENTS.md` is
the "why does it work this way" one.

## Install

```
go get github.com/vislake/speed/go/notification@vX.Y.Z
```

Lockstep versioning applies (root `CLAUDE.md`'s Versioning section): pin
the same `vX.Y.Z` across every `go/*` module and `@speed/*` package you
depend on. `saasctl upgrade --version vX.Y.Z` rewrites a generated
project's `go.mod` for you.

## Requirements

notification needs six things from its host, all supplied through
`NewModule`'s `Option`s. Every one of them is **required**: `Register`
refuses to boot without any of them, naming the missing seam (see the
error table below), rather than deferring the failure to the first
dispatch at run time.

| Option | What it is | Notes |
|---|---|---|
| `WithSMSSender(sender)` | Where SMS goes | `notification.NewConsoleSMSSender(w)` prints to a writer -- fine for the standalone deployment mode and for local development; wire a real HTTP-backed sender for a distributed deployment. |
| `WithMailFrom(addr)` | The `From:` address of every email this module composes | A plain string; the module has no mail-provider config of its own -- it sends through the `pkgcore.Mailer` seam the host's `Kernel.Bootstrap` resolves. |
| `WithContactEmailIndexer(idx)` | Blind indexer for email contact addresses | `dbkit.NewBlindIndexer("email_index", key, dbkit.NormalizeEmail)`. **Must use a different key than the address cipher** -- see `AGENTS.md`'s "Separate index keys from the cipher key". |
| `WithContactPhoneIndexer(idx)` | Blind indexer for phone contact addresses | Same shape, `dbkit.NormalizePhoneE164`. |
| `WithDeliveryQueue(queue)` | The `jobs.Queue` deliveries run on | `jobs.NewStandaloneQueue(db)` for standalone, `queue/asynq.Queue` for distributed. |
| `WithUserAddressResolver(resolver)` | How to turn a user id into an email/phone | Your own type implementing `notification.UserAddressResolver` -- notification's own tables hold no identity data, so it never imports `authn`. |

One more option is optional but needed if you serve the module's HTTP
surface at all: `WithSubjectResolver(resolver)` identifies the caller of
every endpoint (the inbox and preferences are the caller's own data; the
contact roster is tenant-wide with the resolved id used only to identify
who is asking). Leave it unset and the module still boots -- every HTTP
endpoint just answers `401 notification.subject_unresolved` until you wire
one, while the Go service surface (`Deliveries()`, `Contacts()`,
`Preferences()`) works regardless.

## Configure

notification declares no dynamic `go/config` schema of its own in this
milestone -- there is nothing here that varies safely at runtime the way a
brand color or a feature flag does. Every knob above is a construction-time
Go option, resolved once at boot.

### Declaring a notification type

A **business module**, not notification itself, declares each notification
type it wants to send, during its own `Register`:

```go
var noteCreatedNotificationType = pkgcore.NotificationType{
    Key:             "notes.note.create",
    Group:           "collaboration",
    DefaultChannels: []string{"in_app", "email", "sms"},
    Unsubscribable:  true,
}

func (m *Module) Register(reg *pkgcore.Registry) error {
    // ... this module's own declarations ...
    return reg.Notifications.Add(noteCreatedNotificationType)
}
```

The copy itself is never stored in notification's tables. Your module ships
bilingual template bundles (`zh-CN.toml` / `en-US.toml`) under the id
convention `<type_key>.<channel>.<part>` -- `notes.note.create.in_app.title`,
`notes.note.create.email.subject`, `notes.note.create.sms.text`, and so on
-- rendered by delivery, at send time, in the recipient's own locale from
the host's merged catalog. A missing template id is a coded `ErrInternal`
failure, never a silent fallback to another language's copy.

## Quickstart (standalone deployment mode, ~5 minutes)

Everything below runs against an in-memory SQLite database with zero
external services -- no Docker, no Redis, no real SMTP/SMS provider. This
is condensed from `example_test.go`'s compiled, CI-run
`ExampleInboxMessage`; if this snippet and that Example ever disagree,
trust the Example (it is the one godoc and CI actually execute).

```go
package main

import (
    "context"
    "fmt"

    "github.com/vislake/speed/go/dbkit"
    "github.com/vislake/speed/go/pkgcore"

    "github.com/vislake/speed/go/notification"
)

func main() {
    ctx := context.Background()

    // 1. Open and migrate the database (standalone mode: SQLite).
    db, err := dbkit.Open(ctx, dbkit.Options{
        Dialect: dbkit.DialectSQLite,
        DSN:     "file:quickstart?mode=memory&cache=shared",
    })
    if err != nil {
        panic(err)
    }
    registry := dbkit.NewMigrationRegistry()
    mod := notification.NewModule(db) // real wiring needs the six options above
    if err := registry.Register(mod); err != nil {
        panic(err)
    }
    if err := registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
        panic(err)
    }

    // 2. Write directly to the inbox repository, the same table the
    //    delivery pipeline writes to -- see AGENTS.md for the full
    //    Dispatch-based pipeline (a real host also needs a queue, a
    //    UserAddressResolver, an SMS sender and a mail-from address wired
    //    through the module Options above before Dispatch will run).
    repo := notification.NewRepository(db)
    ctx = pkgcore.WithTenant(ctx, "tenant-acme")

    key := "delivery-note-42"
    msg := &notification.InboxMessage{
        ID:              "inbox-000001",
        RecipientUserID: "user-7",
        TypeKey:         "note.shared",
        Title:           "Note 42 was shared with you",
        Body:            "Lin shared the Caries 101 note with you.",
        DedupeKey:       &key,
    }
    if err := repo.Create(ctx, msg); err != nil {
        panic(err)
    }

    got, err := repo.FindByID(ctx, msg.ID)
    if err != nil {
        panic(err)
    }
    fmt.Println("title:", got.Title)
    // Output: title: Note 42 was shared with you
}
```

The full asynchronous dispatch path -- `Deliveries().Dispatch(ctx,
notification.Dispatch{...})`, which enqueues a `notification.deliver` job
that re-checks preferences, consent and addresses at send time, renders
the recipient's locale, and sends over the resolved transport -- needs a
real queue running (`jobs.NewStandaloneQueue(db).Start(ctx)`) plus all six
required `Option`s wired. `AGENTS.md`'s "The pipeline" section walks that
path end to end; `examples/reference-app/cmd/server/server.go`'s
`notificationModule := notification.NewModule(db, ...)` call is the real,
composed reference wiring every one of those six options together, and
`cmd/server/notification_flow_test.go` drives it through a full
create → dispatch → deliver → read cycle over the real composed HTTP
stack.

## HTTP surface

Eleven operations under `/api/v1/notifications`, generated from
`api/openapi.yaml` by the pinned oapi-codegen (`task api:gen`) into the
committed `notification-server.gen.go`:

| Path | What it does |
|---|---|
| `GET /messages` | List the caller's own inbox messages |
| `GET /messages/unread-count` | Unread count for the caller |
| `POST /messages/read-all` | Mark every message read |
| `POST /messages/{messageId}/read` | Mark one message read |
| `GET /types` | The declared notification-type directory |
| `GET /preferences` | The caller's preference matrix |
| `PUT /preferences/{typeKey}/{channel}` | Set one preference cell |
| `GET /contacts` | List the tenant's verified-contact roster |
| `POST /contacts` | Start double opt-in for a new contact |
| `POST /contacts/{contactId}/verify` | Submit a verification code |
| `POST /contacts/{contactId}/resend` | Resend a verification code |

One more endpoint exists but is deliberately absent from the OpenAPI
fragment (server-sent events are not an OpenAPI 3.0 media type):
`GET /api/v1/notifications/stream`, hand-mounted on `Handler`'s inner mux,
announcing committed inbox rows to the requesting browser in
row-then-event order.

Every endpoint reads the tenant from the request context (never a request
parameter) and the caller identity from `WithSubjectResolver`'s
`SubjectResolver` (never a trusted header).

## Error codes

Every error is a coded `*apperr.Error` -- match with `apperr.As(err)` and
compare `.Code`, never a raw string. The most commonly reached ones:

| Code | Meaning | What to do |
|---|---|---|
| `notification.type_not_found` | `Dispatch`/preference call named an undeclared type key | Confirm the declaring module's `Register` ran and called `reg.Notifications.Add` before this call |
| `notification.contact_not_verified` | Dispatch to an external contact that never completed double opt-in | Nothing sends to this address until `POST /contacts/{id}/verify` succeeds -- the verification message itself is the sole exception |
| `notification.contact_unsubscribed` / `notification.contact_bounced` | Terminal consent states | Every future delivery to this contact is refused; there is no un-bounce path in this milestone |
| `notification.preference_optout_not_allowed` | A caller tried to opt out of a transactional (non-unsubscribable) type | Expected refusal -- verification codes and similar transactional types cannot be muted |
| `notification.subject_unresolved` | An HTTP call reached the module with no `SubjectResolver` wired, or the resolver could not identify the caller | Wire `WithSubjectResolver`, or check the resolver's own logic |
| `notification.sms_sender_required` / `notification.mail_from_required` / `notification.contact_email_indexer_required` / `notification.contact_phone_indexer_required` / `notification.delivery_queue_required` / `notification.user_address_resolver_required` | `Register` refused to boot: one of the six required `Option`s above was never applied | Add the missing `With*` option to `NewModule`'s call |

The full, generated index of every error code across every implemented
module (not just this one) lives at `docs/error-codes.md` (repository
root), produced by `tools/gen_error_code_index.py` -- see that script's own
header for how to regenerate it.

## FAQ

**Can I send a message without going through the queue?**
No. Every `Dispatch` call enqueues a job; nothing in this module sends
synchronously. This is deliberate (root `CLAUDE.md`'s "Notifications are
event-driven" rule) -- the one exception anywhere in this codebase is a
synchronous verification code, and that exception lives in `authn`/`org`,
never here.

**Why did my dispatch silently produce no message?**
Check three things in order: (1) the recipient's preferences -- did they
opt out of this type/channel combination? (2) for an external contact, is
it still `pending` (unverified) or already `unsubscribed`/`bounced`? (3)
did `UserAddressResolver` actually return a non-empty address for that
channel? None of these are errors from `Dispatch`'s point of view -- the
job settles a `send_records` row with status `skipped` and a short reason,
which `AGENTS.md`'s "Failure semantics" section documents in full.

**How do I add a new channel (e.g. push notifications)?**
Not supported in this milestone -- `AGENTS.md`'s Deferred list names "a
pkgcore-level SMS seam" and "the platform-staff push consumer" as explicit,
recorded gaps, not silent omissions.

**Does this module know who a "user" is?**
No. Its tables hold zero identity data on purpose -- `UserAddressResolver`
is the one seam that crosses into identity territory, and it is host-
supplied, never a notification import of `authn`.

**Where do I look when something dead-letters?**
`go/jobs`' own `StandaloneQueue.DeadLetterJobs(ctx)` (or the equivalent
admin surface over the distributed queue) lists every job that exhausted
its retry budget, including its last `Task.Payload` -- decode it back into
a `notification.Dispatch` to see exactly what was being attempted.
