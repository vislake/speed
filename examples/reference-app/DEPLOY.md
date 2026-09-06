# Deploying to Fly.io

This is a real, validated Fly.io deployment config for `examples/reference-app` — speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section) — sized to stay as close to Fly.io's current free allowance as that allowance actually goes (see "The free-tier facts, as verified today" below: there is less free headroom than there used to be). `fly.toml` lives at the **repository root**, not this directory, for the identical reason `Dockerfile`'s own header and this README's "Running it in Docker" section give: the build context must include `go.work`, every `go/*` module, and `examples/reference-app` all at once.

This document only *describes* the commands a human operator runs. Nothing in this repository runs `fly launch`/`fly deploy`/`fly secrets set` against a real account automatically — those are live, outward-facing actions with real billing consequences, deliberately left to a human (or an agent acting with the account owner directly in the loop) to run themselves.

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

# 2. Set the three bootstrap secrets configFromEnv treats as
#    security-sensitive (see "Secrets to set first" below). Generate real
#    values -- never reuse the example below, never commit real values
#    anywhere in this repository.
fly secrets set \
  SPEED_CONFIG_KEY="$(openssl rand -hex 32)" \
  SPEED_ORG_INDEX_KEY="$(openssl rand -hex 32)" \
  SPEED_NOTIFICATION_INDEX_KEY="$(openssl rand -hex 32)"

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

Every one of these is named, never valued, by this repository — generate real values yourself (`openssl rand -hex 32` for the three 32-byte hex keys) and set them only through `fly secrets set`, never in `fly.toml`'s `[env]` (which is plaintext and committed) or anywhere else in this repository. `cmd/server/server.go`'s `configFromEnv` is the authority for what each one does; `.env.example` has the equivalent local-development explanation for each.

| Secret | Why it is sensitive |
|---|---|
| `SPEED_CONFIG_KEY` | The master key `config.WithCipher` seals every Sensitive dynamic-configuration value with. The key that encrypts the `configs` table cannot live in that table, so it must come from the environment — and a real deployment must never fall back to the committed, documented-as-non-secret `devConfigKey` development default. |
| `SPEED_ORG_INDEX_KEY` | The HMAC key org's blind indexer normalizes and indexes invitation email addresses with. A dbkit rule (an AES key must never double as an HMAC key) is why this is a separate secret from `SPEED_CONFIG_KEY`, never the same value. |
| `SPEED_NOTIFICATION_INDEX_KEY` | The HMAC key the notification module's blind indexers index encrypted contact email/phone addresses with. Same separate-secret rule as above, and separate again from `SPEED_ORG_INDEX_KEY`. |

Optional, but recommended to set as a secret rather than leave in `[env]` **for a real deployment reachable over the public internet** — `SPEED_DEMO_USERS_PASSWORD` (gates the boot-time demo-user seed, `demo_users.go`). The app's own code and `.env.example` treat this as a non-secret local-demo passphrase, which is true on a laptop nobody else can reach; on a public Fly.io URL, though, whoever knows this value can sign in as `demo-owner@example.com`, a real account holding every permission any module declared. This deployment's `fly.toml` leaves it **unset entirely**, which skips the demo-account seed — the safer default for a fresh public deployment. Set it only if you deliberately want the demo accounts reachable, and set it via `fly secrets set SPEED_DEMO_USERS_PASSWORD=...`, never `[env]`.

Nothing else needs a secret for this deployment: standalone mode with no `SPEED_REDIS_ADDR`/`SPEED_S3_*`/`SPEED_SMTP_*`/`SPEED_SMS_GATEWAY_URL` set leaves every other seam on its zero-external-dependency in-process implementation (console mailer, local-directory object store, in-process event bus and KV store) — real cost and complexity this first deployment deliberately does not take on.

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

- **No Redis, S3-compatible storage, SMTP relay, or SMS gateway.** `SPEED_DEPLOYMENT_MODE` stays `standalone` and every other seam stays on its in-process default — see `fly.toml`'s own comment on why, and this README's "Running it in Docker" section for what wiring those in (a *different*, more expensive composition) would look like, were a later deployment to need it.
- **No `fly launch`/`fly deploy`/`fly volumes create`/`fly secrets set` run against a real account by this change.** The command sequence above is documentation for a human (or an agent working directly with the account owner) to run; nothing in this repository executes it.
- **No real secret value anywhere in this repository.** Every key named above is a name only; generate real values with `openssl rand -hex 32` at deploy time.
