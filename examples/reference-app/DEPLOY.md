# Deploying to Fly.io

This is a real, validated Fly.io deployment config for `examples/reference-app` — speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section) — sized to stay as close to Fly.io's current free allowance as that allowance actually goes (see "The free-tier facts, as verified today" below: there is less free headroom than there used to be). `fly.toml` lives at the **repository root**, not this directory, for the identical reason `Dockerfile`'s own header and this README's "Running it in Docker" section give: the build context must include `go.work`, every `go/*` module, and `examples/reference-app` all at once.

This document only *describes* the commands a human operator runs. Nothing in this repository runs `fly launch`/`fly deploy`/`fly secrets set` against a real account automatically — those are live, outward-facing actions with real billing consequences, deliberately left to a human (or an agent acting with the account owner directly in the loop) to run themselves.

## Breaking change: environment variable prefix renamed `SPEED_` → `APP_`

Every environment variable name this app's own bootstrap code declares and reads was renamed from a `SPEED_` prefix to an `APP_` prefix — a pure rename, this consumer app's own naming convention rather than any framework-level requirement, with no behavior change otherwise. An existing `fly secrets` set or a previously-deployed `fly.toml` `[env]` block still using the old `SPEED_*` names will simply have no effect on the next deploy: those variables are no longer read, and every affected setting falls back to its documented default (or, if you run distributed mode, `Kernel.Bootstrap`'s capability validation fails and names the missing seam). Re-set your secrets and `[env]` values under the new names before redeploying:

| Old name | New name |
|---|---|
| `SPEED_DEPLOYMENT_MODE` | `APP_DEPLOYMENT_MODE` |
| `SPEED_DB_PATH` | `APP_DB_PATH` |
| `SPEED_CONFIG_KEY` | `APP_CONFIG_KEY` |
| `SPEED_ORG_INDEX_KEY` | `APP_ORG_INDEX_KEY` |
| `SPEED_NOTIFICATION_INDEX_KEY` | `APP_NOTIFICATION_INDEX_KEY` |
| `SPEED_REDIS_ADDR` | `APP_REDIS_ADDR` |
| `SPEED_S3_ENDPOINT` | `APP_S3_ENDPOINT` |
| `SPEED_S3_BUCKET` | `APP_S3_BUCKET` |
| `SPEED_S3_ACCESS_KEY` | `APP_S3_ACCESS_KEY` |
| `SPEED_S3_SECRET_KEY` | `APP_S3_SECRET_KEY` |
| `SPEED_S3_REGION` | `APP_S3_REGION` |
| `SPEED_S3_USE_SSL` | `APP_S3_USE_SSL` |
| `SPEED_SMTP_HOST` | `APP_SMTP_HOST` |
| `SPEED_SMTP_PORT` | `APP_SMTP_PORT` |
| `SPEED_SMTP_USERNAME` | `APP_SMTP_USERNAME` |
| `SPEED_SMTP_PASSWORD` | `APP_SMTP_PASSWORD` |
| `SPEED_SMS_GATEWAY_URL` | `APP_SMS_GATEWAY_URL` |
| `SPEED_DISABLE_QUEUE_WORKER` | `APP_DISABLE_QUEUE_WORKER` |
| `SPEED_DEMO_USERS_PASSWORD` | `APP_DEMO_USERS_PASSWORD` |
| `PORT` | `PORT` (unchanged) |

## Prerequisites

- `flyctl` installed and authenticated (`fly auth login` or `fly auth signup`). Verified present in this environment as `flyctl v0.4.99`.
- A Fly.io organization to deploy into. As of this writing, **a payment method (or prepaid credit) is required for most real, ongoing usage** — see the free-tier section below before assuming otherwise.
- Docker (or Fly's remote builder, which `fly launch`/`fly deploy` fall back to automatically when no local Docker daemon is available) to build the image.

## Deploy command sequence

Run every command below from the **repository root** (where `fly.toml` lives):

```bash
# 1. Confirm/rename the app and region. "speed-reference-app" in fly.toml is a
#    placeholder -- Fly app names are globally unique across every Fly
#    customer, so this will very likely need to change. --no-deploy so no
#    secrets-less deploy happens before step 2 below.
fly launch --name <your-unique-app-name> --region <your-region> --no-deploy \
  --copy-config --org <your-org>

# 2. Set the one root secret configFromEnv derives all six of its
#    bootstrap key materials from (see "Secrets to set first" below).
#    Generate a real value -- never reuse the example below, never commit
#    a real value anywhere in this repository.
fly secrets set \
  APP_ROOT_KEY="$(openssl rand -hex 32)"

# 3. First deploy. Creates the volume declared in fly.toml's [[mounts]] on
#    first deploy if it does not already exist (recent flyctl versions do
#    this automatically for an app with exactly one [[mounts]] entry and no
#    existing volume; if your flyctl version asks instead, answer yes, or
#    create it explicitly first with:
#      fly volumes create reference_app_data --region <your-region> --size 1
#    ONE volume only -- do not create a second one "for redundancy": this is
#    a single-machine demo deployment by design (see fly.toml's own comment
#    on why redundancy is deliberately out of scope here).
fly deploy

# 4. Make the single-machine intent explicit and durable across future
#    deploys (some flyctl versions default a fresh app to more than one
#    machine for HA where the region supports it).
fly scale count 1

# 5. Smoke-test.
fly status
curl -s "https://<your-app-name>.fly.dev/healthz"   # expect: ok
curl -s "https://<your-app-name>.fly.dev/api/config/public"
```

### Secrets to set first

**Set `APP_ROOT_KEY` — that is the whole recommended path.** `cmd/server/server.go`'s `configFromEnv` derives all six of the bootstrap key materials below from this one 32-byte hex secret via `dbkit.DeriveKey` (HKDF-SHA256, one distinct, versioned purpose string per key — `go/dbkit/AGENTS.md`'s "Key derivation" section has the full mechanism and its honest trade-off: a leaked root key compromises all six at once, and rotating it rotates all six together). Generate one value and set it, never valued by this repository — generate it yourself (`openssl rand -hex 32`) and set it only through `fly secrets set`, never in `fly.toml`'s `[env]` (which is plaintext and committed) or anywhere else in this repository:

```bash
fly secrets set APP_ROOT_KEY="$(openssl rand -hex 32)"
```

**Why this matters before real public traffic, not just as a convenience.** Before `APP_ROOT_KEY` existed, three of these six key materials — `APP_PKI_LOCAL_KEY_CIPHER_KEY`, `APP_AUTHN_BLIND_INDEX_KEY` and `APP_AUTHN_PII_CIPHER_KEY` — had **no environment-variable override path at all**: the only way to change them was editing the hardcoded, committed-to-source-control development defaults in `server.go` itself. A reference-app deployment that set only the three older keys (`APP_CONFIG_KEY`/`APP_ORG_INDEX_KEY`/`APP_NOTIFICATION_INDEX_KEY`) was, without anyone necessarily realizing it, still running its `pki` signing-key storage, its `authn` email/phone blind index and its `authn` PII encryption (email, phone, TOTP secrets) on keys committed to this very repository's source — a real, live gap this round closes. Setting `APP_ROOT_KEY` (or, at minimum, the three individual variables in the table below) before real public traffic reaches a deployment is what actually closes it.

| Secret | Why it is sensitive |
|---|---|
| `APP_ROOT_KEY` | **Recommended default.** The single root secret `configFromEnv` derives every other key below from. Set this and skip the rest of this table entirely for a normal deployment. |

Advanced: setting one or more of the six individual keys directly, instead of (or in addition to) `APP_ROOT_KEY`, for fine-grained per-key rotation. **An explicitly-set individual variable always wins over what `APP_ROOT_KEY` would derive for that same key** — the two compose freely, so a deployment can derive five keys from the root and rotate the sixth independently by setting only its own variable.

| Secret | Why it is sensitive |
|---|---|
| `APP_CONFIG_KEY` | The master key `config.WithCipher` seals every Sensitive dynamic-configuration value with. The key that encrypts the `configs` table cannot live in that table, so it must come from the environment — and a real deployment must never fall back to the committed, documented-as-non-secret `devConfigKey` development default. |
| `APP_ORG_INDEX_KEY` | The HMAC key org's blind indexer normalizes and indexes invitation email addresses with. A dbkit rule (an AES key must never double as an HMAC key) is why this is a separate secret from `APP_CONFIG_KEY`, never the same value. |
| `APP_NOTIFICATION_INDEX_KEY` | The HMAC key the notification module's blind indexers index encrypted contact email/phone addresses with. Same separate-secret rule as above, and separate again from `APP_ORG_INDEX_KEY`. |
| `APP_PKI_LOCAL_KEY_CIPHER_KEY` | The AES key that seals go/pki's `LocalSigner` private-key column — the key authn's access tokens are ultimately signed with. Had no override path before this round; a deployment that never set this was running signing-key storage on a key committed to this repository. |
| `APP_AUTHN_BLIND_INDEX_KEY` | The HMAC key authn indexes `users.email_index`/`phone_index` with, so a user can be found by email or phone without decrypting every row. Must stay identical across restarts (changing it — including by rotating `APP_ROOT_KEY` — makes every already-stored index unfindable until a rebuild) — had no override path before this round. |
| `APP_AUTHN_PII_CIPHER_KEY` | The AES key that seals authn's encrypted PII columns (email, phone, TOTP secrets). Had no override path before this round; deliberately a separate secret from `APP_CONFIG_KEY` and from `APP_PKI_LOCAL_KEY_CIPHER_KEY` — dbkit's key-separation rule applies across modules, not only within one. |

Optional, but recommended to set as a secret rather than leave in `[env]` **for a real deployment reachable over the public internet** — `APP_DEMO_USERS_PASSWORD` (gates the boot-time demo-user seed, `demo_users.go`). The app's own code and `.env.example` treat this as a non-secret local-demo passphrase, which is true on a laptop nobody else can reach; on a public Fly.io URL, though, whoever knows this value can sign in as `demo-owner@example.com`, a real account holding every permission any module declared. This deployment's `fly.toml` leaves it **unset entirely**, which skips the demo-account seed — the safer default for a fresh public deployment. Set it only if you deliberately want the demo accounts reachable, and set it via `fly secrets set APP_DEMO_USERS_PASSWORD=...`, never `[env]`.

The same holds, under its OWN variable, for the platform-staff demo account: `APP_DEMO_PLATFORM_STAFF_PASSWORD` gates the boot-time seed of `demo-platform-staff@example.com` (`demo_admin.go`'s `seedDemoPlatformStaff`), the rbac.SystemDomain platform administrator holding the built-in owner role there — every permission any module declared, `go/admin`'s `admin:*` permissions included. It is deliberately a separate variable from `APP_DEMO_USERS_PASSWORD`: whoever knows the demo users' passphrase must never also hold the platform administrator's. `fly.toml` leaves it **unset entirely** as well — the same skip-the-seed safer default. If you do set either, set it via `fly secrets set`, never `[env]`, and never set the two to the same value.

**A second, unrelated demo affordance this app ships that neither password variable controls**: the demo identity request headers — `X-Demo-User` (`demo_subject.go`'s `demoUserHeader`), read by every permission-gated route, and `X-Demo-User-Id` (`server.go`'s `demoOrgUserHeader`), read by the attribution seams serving notes' create handler, the cases surface, org's caller-scoped invitation endpoints and the notification surface — active regardless of either variable's setting. Any caller who can reach this deployment can set `X-Demo-User` to `demo-owner` (or any other seeded demo actor) and act with that actor's full rbac grants, or set `X-Demo-User-Id` to any user id and act as that user on the attribution surfaces — and, worse, the headers still outrank a REAL, verified access token's own identity when both are sent on the same request, so a real signed-in user's session does not even protect against them. Set `fly secrets set APP_DISABLE_DEMO_USER_HEADER=1` for any deployment a real, non-demo user might reach — this disables EVERY demo identity header at once: every gated route resolves its acting user from the verified access token alone, and so does every attribution seam, the demo headers no longer read at all. This deployment's `fly.toml` leaves it **unset entirely** by default, matching every other demo affordance's own opt-in-to-disable posture in this section; a genuinely public, non-demo deployment should set it.

Nothing else needs a secret for this deployment: standalone mode with no `APP_REDIS_ADDR`/`APP_S3_*`/`APP_SMTP_*`/`APP_SMS_GATEWAY_URL` set leaves every other seam on its zero-external-dependency in-process implementation (console mailer, local-directory object store, in-process event bus and KV store) — real cost and complexity this first deployment deliberately does not take on.

## The free-tier facts, as verified today

Verified directly against Fly's own current docs (`fly.io/docs/about/pricing/`, `fly.io/docs/about/free-trial/`, `fly.io/docs/about/billing/`) and `flyctl v0.4.99`'s own `platform vm-sizes` output, on the date this file was written. **Fly's terms have changed materially over its own history and will likely change again — re-verify before trusting this section blindly.**

- **There is no persistent, ongoing free tier anymore.** What Fly.io calls a "free trial" today is time- and usage-boxed: *"2 hours of machine runtime or 7 days of access, whichever comes first"*, with trial machines auto-stopping after 5 minutes of continuous runtime. It includes up to 10 machines (2 vCPUs / 4GB memory each) and 20GB of volume storage while it lasts. This is a materially different thing from the old always-on free allowance Fly.io used to offer (a few always-free `shared-cpu-1x-256mb` machines with no time limit) — that program no longer exists.
- **A payment method is required for real, ongoing use.** Fly's own billing docs: *"We require an active, valid credit card on file for most Fly.io accounts to do things like deploying multiple apps and deploying public images."* A prepaid-credit alternative exists (minimum purchase $25) for accounts that cannot provide a card. The free trial itself does not require a card up front, but **adding one ends the trial** and starts real, metered billing immediately.
- **This means a demo deployment meant to stay up (not just for a 2-hour/7-day trial window) will incur small real charges once the trial lapses or a card is added** — there is no way around that on Fly.io as it stands today. This config is sized to make that cost as close to the practical floor as Fly.io offers, not to reach an actual $0/month that no longer exists for an always-available deployment:
  - **Compute**: `shared-cpu-1x` (1 shared vCPU, 256MB memory) is Fly's smallest machine size, confirmed via `flyctl platform vm-sizes`. Billing is per-second while a machine is actually running (Amsterdam-region list price: **$0.0028/hour, ≈$2.02/month if run continuously**); `auto_stop_machines = "stop"` plus `min_machines_running = 0` in `fly.toml` mean an idle demo spends most of its time stopped and billed nothing for compute during that time.
  - **Volume storage**: standard Fly volumes cost **$0.15/GB of provisioned capacity per month, pro-rated to the hour, charged whether or not the machine using it is running**. There is no free allowance for this (the "first 10GB free" Fly's pricing page mentions applies only to volume *snapshots*, a different, optional feature this config does not enable). The 1GB volume this config declares costs roughly **$0.15/month** on its own, continuously, regardless of traffic.
  - **Realistic floor for this deployment, once past the trial**: on the order of **$0.15–$2.50/month** depending on how much real traffic keeps the machine running versus stopped — genuinely small, but not zero, and not avoidable through configuration alone.
- **Exactly one machine, no redundancy** — `fly scale count 1` in the command sequence above, and `fly.toml`'s own comment on why: this is a demo deployment, not a production HA setup, and a second machine would double the compute-cost side of the estimate above for no benefit this deployment needs.

## Local memory-footprint measurement (why 256MB is enough)

Rather than guess, the exact image `fly.toml` builds was built and run locally under a hard 256MB memory limit and measured with `docker stats`:

```bash
docker build -f Dockerfile -t reference-app-flyio-test ..   # repo root context
docker run -d --name reference-app-flyio-memtest -p 18080:8080 -m 256m reference-app-flyio-test
curl -s http://localhost:18080/healthz   # ok
docker stats reference-app-flyio-memtest --no-stream
```

Result: **~14.5MiB resident at idle, ~16.1MiB after a burst of `/healthz`, `/metrics` and `/api/config/public` requests** — under 6.3% of the 256MB ceiling in both cases. `shared-cpu-1x`'s default 256MB carries roughly 15x headroom for this app as it stands today; there is no basis in this measurement for choosing a larger, more expensive size.

## Validating this config

```bash
fly config validate -c fly.toml            # from the repository root
fly config validate -c fly.toml --strict   # also flags unrecognized keys
```

Both run with no authenticated app context required (only a harmless "Metrics token unavailable" warning appears, unrelated to config validity) and both passed cleanly against the committed `fly.toml` at authoring time.

## What this deployment deliberately does not do

- **No Redis, S3-compatible storage, SMTP relay, or SMS gateway.** `APP_DEPLOYMENT_MODE` stays `standalone` and every other seam stays on its in-process default — see `fly.toml`'s own comment on why, and this README's "Running it in Docker" section for what wiring those in (a *different*, more expensive composition) would look like, were a later deployment to need it.
- **No `fly launch`/`fly deploy`/`fly volumes create`/`fly secrets set` run against a real account by this change.** The command sequence above is documentation for a human (or an agent working directly with the account owner) to run; nothing in this repository executes it.
- **No real secret value anywhere in this repository.** Every key named above is a name only; generate real values with `openssl rand -hex 32` at deploy time.
