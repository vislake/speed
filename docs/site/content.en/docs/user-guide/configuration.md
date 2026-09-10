---
title: "Configuration reference"
description: "Every bootstrap key and runtime configuration item a speed-based application resolves: the keys platform modules declare, the variables this repository's reference app reads, and the dynamic items an operator edits per tenant."
weight: 98
---
# Configuration reference

A speed-based application resolves configuration in two layers that never share a key. This page is generated from the modules' own declarations and the reference app's source -- never hand-written -- and a stale copy fails CI.

## Bootstrap configuration

The bootstrap layer is the process-start input a speed-based application resolves before anything else starts. Two halves are documented here: the **platform keys**, declared by the modules that consume them on the registry's bootstrap seat (`reg.Bootstrap.Add`), and the **reference app's own variables**, the environment surface the app reads today (with `os.Getenv`; the app does not drive the loader yet). Every variable below is optional in development, falling back to the documented default that keeps `go run ./cmd/server` booting a working standalone server with zero external dependencies.

The loader mechanism behind the platform keys is `go/pkgcore/config`. A host that drives it resolves each key from four sources, highest priority first: command-line flags (`--database.dsn=…`), environment variables, an optional YAML **or JSON** config file (`WithConfigFile`; an absent file is skipped silently, a malformed one is a hard error), then the defaults already set on the host's target struct. Keys are derived from the target struct's fields: `Database.DSN` is the key `database.dsn`, the flag `--database.dsn` and the variable `SPEED_DATABASE__DSN` -- the loader's default prefix `SPEED_`, the key uppercased, and a double underscore for each level of nesting. A single underscore is not a nesting marker (`SPEED_DATABASE_DSN` resolves to nothing), and variables that match no key are ignored rather than rejected. `WithEnvPrefix` replaces the prefix for a host whose variables already carry another one (`APP_`, say), and a field may pin its exact variable name with `config:"env=PORT"` for names no derivation reaches; a pinned field reads that name and no other. Two fields resolving to one variable is refused when the target is described, because a single variable cannot feed two fields. `config.Verify(target, declared)` then checks a declared key list against the target struct, which is how a host proves its target binds the keys its modules declared.

| Variable | Type | Unset fallback | Secret | What it configures |
|---|---|---|---|---|
| `APP_DB_PATH` | string | reference-app.db |  | SQLite database file path (relative to the working directory). |
| `APP_DEPLOYMENT_MODE` | string | standalone |  | Deployment topology: standalone (default) or distributed; constrains which seam implementations may compose. |
| `PORT` | string | 8080 |  | HTTP listen port. |
| `APP_AUTHN_BLIND_INDEX_KEY` | hexkey | documented non-secret development default | yes | HMAC key authn indexes users.email_index/phone_index with; must stay identical across restarts or stored indexes become unfindable. |
| `APP_AUTHN_PII_CIPHER_KEY` | hexkey | documented non-secret development default | yes | AES key sealing authn's encrypted PII columns (email, phone, TOTP secrets); separate from every other key. |
| `APP_CONFIG_KEY` | hexkey | documented non-secret development default | yes | Master key the config module seals every Sensitive dynamic-configuration value with; the key that encrypts the configs table can never live in that table. |
| `APP_NOTIFICATION_INDEX_KEY` | hexkey | documented non-secret development default | yes | HMAC key the notification module's blind indexers index encrypted contact addresses with; separate from every other key. |
| `APP_ORG_INDEX_KEY` | hexkey | documented non-secret development default | yes | HMAC key org's blind indexer indexes invitation email addresses with; separate from APP_CONFIG_KEY (an AES key must never double as an HMAC key). |
| `APP_PKI_LOCAL_KEY_CIPHER_KEY` | hexkey | documented non-secret development default | yes | AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with. |
| `APP_ROOT_KEY` | hexkey | individual development defaults | yes | Single root secret from which the other six keys of this group are derived (HKDF-SHA256, one purpose string per key); an explicitly-set individual key always wins over its derivation. |
| `APP_OBJECT_STORE_ROOT` | string | throwaway temp directory |  | Fixed local directory for the objectstore seam, the SurvivesRestart twin of the S3 composition; setting both is refused. |
| `APP_REDIS_ADDR` | string | in-process eventbus/kv (Preset default) |  | Redis address composing real, multi-replica-safe EventBus and KVStore implementations for both seams. |
| `APP_S3_ACCESS_KEY` | string | local-directory objectstore (Preset default) |  | S3 access key id for the objectstore seam (required with the S3 group). |
| `APP_S3_BUCKET` | string | local-directory objectstore (Preset default) |  | S3 bucket name for the objectstore seam (required with the S3 group). |
| `APP_S3_ENDPOINT` | string | local-directory objectstore (Preset default) |  | S3-compatible endpoint; required together with bucket/access-key/secret-key below. |
| `APP_S3_REGION` | string | unset (ignored by S3-compatible local servers) |  | Optional S3 region, used only when the S3 group above is set. |
| `APP_S3_SECRET_KEY` | string | local-directory objectstore (Preset default) | yes | S3 secret key for the objectstore seam (required with the S3 group). |
| `APP_S3_USE_SSL` | bool | false |  | Whether the S3 endpoint speaks TLS (optional; plain HTTP is the local-server default). |
| `APP_SMS_GATEWAY_URL` | string | console SMS sender (standalone) / refused boot (distributed) | yes | Endpoint the real HTTP SMS transport posts delivery requests to; the URL is where gateway credentials live. |
| `APP_SMTP_HOST` | string | console mailer (Preset default) |  | SMTP host composing a real Mailer for the mailer seam; required together with APP_SMTP_PORT. |
| `APP_SMTP_PASSWORD` | string | no SMTP AUTH (optional) | yes | Optional SMTP AUTH password. |
| `APP_SMTP_PORT` | int | console mailer (Preset default) |  | SMTP port for the mailer seam (required with APP_SMTP_HOST). |
| `APP_SMTP_USERNAME` | string | no SMTP AUTH (optional) |  | Optional SMTP AUTH username; AUTH activates only when a username is set. |
| `APP_OTLP_ENDPOINT` | string | local exporters (stdout traces/metrics) |  | OTLP/gRPC endpoint traces and metrics are pushed to; empty keeps go/observability's local exporters. |
| `APP_PUBLIC_ORIGIN` | string | http://localhost:<resolved PORT> |  | This deployment's own public origin, the base URL outbound mail links render when a tenant has no branded host; an unset value ships mail with unreachable localhost links. |
| `APP_READ_FLY_CLIENT_IP` | bool | false |  | Declares the proxy is Fly's, authorizing authn to read the single-hop Fly-Client-IP header; only takes effect alongside APP_TRUSTED_PROXIES ('true' with an empty proxy list refuses boot). |
| `APP_TRUSTED_PROXIES` | string | no trusted proxies (direct connection address recorded) |  | Comma-separated reverse-proxy addresses this deployment receives requests through; what lets session/login-history records carry the real client address. |
| `APP_WEB_DIST` | string | no frontend served (API only) |  | Directory the server serves the app's built frontend from; unset serves no frontend at all. |
| `APP_AI_GATEWAY_IMAGE_API_KEY` | string | no image credential row written |  | Key the images endpoint accepts; the demo value is not a secret (a local or CI-only fake provider accepts it), while a real deployment's key travels the same variable. |
| `APP_AI_GATEWAY_IMAGE_BASE_URL` | string | no image credential row written |  | Base URL of the OpenAI-compatible images endpoint the smile-simulation pipeline posts to; set together with the API key pair. |
| `APP_DEMO_PLATFORM_STAFF_PASSWORD` | string | no platform-staff seed |  | Gates the boot-time seed of demo-platform-staff@example.com, the rbac.SystemDomain platform administrator; deliberately separate from APP_DEMO_USERS_PASSWORD. |
| `APP_DEMO_USERS_PASSWORD` | string | no demo-account seed |  | Gates the boot-time seed of the three demo user accounts (demo-owner/demo-reader/demo-acme-only); a passphrase only a disposable demo server should carry. |
| `APP_DISABLE_DEMO_USER_HEADER` | string | demo identity headers read (any non-empty value disables them) |  | Kill switch closing the X-Demo-User/X-Demo-User-Id privilege-escalation hole: any non-empty value makes every route resolve its actor from the verified access token alone. |
| `APP_DISABLE_QUEUE_WORKER` | string | queue worker started |  | Test-only: any non-empty value skips standaloneQueue.Start, so this replica never claims or executes a job itself (the distributed-mode integration proof's other-replica driver). |
| `APP_FAIL_SELF_SERVICE_PROVISION` | int | 0 (disabled) |  | Test-and-e2e-only failure injection: a positive N fails the first N self-service provisioning attempts of each account; anything that is not a non-negative whole number refuses boot. |

### Bootstrap keys declared by platform modules

Each platform module declares the process-start keys it consumes on the registry's bootstrap seat, so the key's contract -- what it protects, why it is a separate secret, what an operator should expect when it is unset -- travels with the module instead of living in a host's own notes. The declarations below are rendered from `reg.Bootstrap`; the "environment variable" column names the spelling the reference app reads the same key from today, which is the transition's bridge until the app's loader-shaped struct pins it.

**authn**

| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |
|---|---|---|---|---|---|
| `authn.blind_index_key` | hexkey | true | documented non-secret development default | `APP_AUTHN_BLIND_INDEX_KEY` | HMAC key authn indexes its users.email_index and phone_index blind-index columns with; it must stay identical across restarts or every already-stored email and phone index becomes unfindable, and an HMAC key never doubles as a cipher key. |
| `authn.pii_cipher_key` | hexkey | true | documented non-secret development default | `APP_AUTHN_PII_CIPHER_KEY` | AES key sealing authn's encrypted PII columns (email, phone, TOTP secrets), deliberately separate from every other module's key material and from authn's own blind-index key below. |

**config**

| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |
|---|---|---|---|---|---|
| `config.master_key` | hexkey | true | documented non-secret development default | `APP_CONFIG_KEY` | Master key the config module seals every Sensitive dynamic-configuration value with (the configs table stores base64 ciphertext); the key that encrypts the table cannot live in the table, so it comes from the host's process-start input. |

**notification**

| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |
|---|---|---|---|---|---|
| `notification.contact_index_key` | hexkey | true | documented non-secret development default | `APP_NOTIFICATION_INDEX_KEY` | HMAC key the notification module's blind indexers index its encrypted contact addresses with; one key serves the email and phone indexers, whose canonical forms are disjoint, and it stays separate from every cipher key. |

**org**

| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |
|---|---|---|---|---|---|
| `org.invitation_email_index_key` | hexkey | true | documented non-secret development default | `APP_ORG_INDEX_KEY` | HMAC key org's blind indexer indexes invitation email addresses with; separate from every cipher key, because an AES key never doubles as an HMAC key. |

**pki**

| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |
|---|---|---|---|---|---|
| `pki.local_key_cipher_key` | hexkey | true | documented non-secret development default | `APP_PKI_LOCAL_KEY_CIPHER_KEY` | AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with; separate from every other key, since dbkit's key-separation rule spans modules, not only one. |

Every other module in this reference's composition declares no bootstrap keys: `compliance`, `metering`, `sharing`. That is an honest state rather than a gap -- a module that consumes no process-start input declares nothing, and nothing asks it for a placeholder.

A key belongs to exactly one configuration layer. A bootstrap key is process-start input, resolved once and fixed for the process's lifetime; a runtime item (the next section) is a per-tenant value an operator edits while the process runs. Declaring the same dotted key on both layers is refused at startup and in this generator, because one identifier cannot carry two meanings, two defaults and two edit surfaces.

## Dynamic configuration

The dynamic layer holds the configuration an operator edits at runtime, served by the `go/config` module: every module declares the items and feature flags it owns during `Register` (`reg.Config.Add` / `reg.Features.Add`), and `config.Module.Attach` freezes them into one schema the moment Bootstrap returns. Values live in the shared `configs` table (platform data, keyed by `(key, scope, tenant_id)`) under two scope tiers: a **system** row is platform-wide (writing one requires an audited system context), a **tenant** row overrides it for one tenant, and a read resolves tenant row -> system row -> the schema default. Scope is a property of the row, not of the key: every key below may hold a row at either tier. A key with no default and no row at any reachable scope has no value to serve (`config.item_unset`). Writes publish `config.item.changed` (with `[redacted]` markers in both value slots for a Sensitive item) and produce an audit record.

Sensitive items are encrypted at rest: the `configs` table stores `base64(ciphertext)` sealed by the host's `dbkit.Cipher` key, never plaintext, and are never served on the public endpoint, whose rows are exactly the items marked Public below (`/api/config/public`, `config.PathPublic`). A Sensitive item's default is redacted in this very table (the `[redacted]` marker), because this document is a committed artifact a secret's plaintext has no more business crossing than the event bus.

| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |
|---|---|---|---|---|---|---|---|---|
| `authn.access_token_ttl` | item | duration | 15m0s | 1m0s .. 24h0m0s | false | false | authn | How long an access token stays valid, and the worst-case delay before a sign-out takes effect under natural revocation. |
| `authn.oauth_state_ttl` | item | duration | 10m0s | 1m0s .. 1h0m0s | false | false | authn | How long a social or single sign-on authorization flow may take between leaving for the provider and returning. |
| `authn.password_login` | flag | bool | true | -- | false | false |  | Allows signing in with an email address or phone number and a password. |
| `authn.password_max_length` | item | int | 128 | 16 .. 1024 | false | false | authn | Maximum number of characters a new password may have. |
| `authn.password_min_length` | item | int | 12 | 8 .. 128 | false | false | authn | Minimum number of characters a new password must have. |
| `authn.refresh_token_ttl` | item | duration | 720h0m0s | 1h0m0s .. 8760h0m0s | false | false | authn | How long a refresh token stays usable before the user must sign in again. |
| `authn.session_ttl` | item | duration | 2160h0m0s | 1h0m0s .. 8760h0m0s | false | false | authn | Maximum lifetime of a session, however often it is refreshed. |
| `authn.sms_code_max_attempts` | item | int | 5 | 3 .. 10 | false | false | authn | How many wrong codes a single issued verification code tolerates before it locks and a fresh one must be requested. |
| `authn.sms_code_ttl` | item | duration | 5m0s | 1m0s .. 1h0m0s | false | false | authn | How long a phone-login verification code stays valid after it is sent. |
| `authn.sms_login` | flag | bool | true | -- | false | false |  | Allows signing in with a phone number and a one-time SMS code. |
| `authn.social.dingtalk` | flag | bool | false | -- | false | false |  | Offers DingTalk as a sign-in channel. Requires the DingTalk application key and secret to be configured. |
| `authn.social.dingtalk.client_id` | item | string |  | -- | false | false | authn | DingTalk OAuth application key. |
| `authn.social.dingtalk.client_secret` | item | string | [redacted] | -- | true | false | authn | DingTalk OAuth application secret. |
| `authn.social.feishu` | flag | bool | false | -- | false | false |  | Offers Feishu as a sign-in channel. Requires the Feishu app id and secret to be configured. |
| `authn.social.feishu.client_id` | item | string |  | -- | false | false | authn | Feishu OAuth app id. |
| `authn.social.feishu.client_secret` | item | string | [redacted] | -- | true | false | authn | Feishu OAuth app secret. |
| `authn.social.github` | flag | bool | false | -- | false | false |  | Offers GitHub as a sign-in channel. Requires the GitHub client id and secret to be configured. |
| `authn.social.github.client_id` | item | string |  | -- | false | false | authn | GitHub OAuth client id. |
| `authn.social.github.client_secret` | item | string | [redacted] | -- | true | false | authn | GitHub OAuth client secret. |
| `authn.social.google` | flag | bool | false | -- | false | false |  | Offers Google as a sign-in channel. Requires the Google client id and secret to be configured. |
| `authn.social.google.client_id` | item | string |  | -- | false | false | authn | Google OAuth client id. |
| `authn.social.google.client_secret` | item | string | [redacted] | -- | true | false | authn | Google OAuth client secret. |
| `authn.social.trusted_providers` | item | string |  | -- | false | false | authn | Whitespace-delimited social channels whose verified-email assertion may automatically link a new sign-in to an existing account. Empty disables automatic linking entirely. |
| `authn.social.wechat` | flag | bool | false | -- | false | false |  | Offers WeChat as a sign-in channel. Requires the WeChat Open Platform appid and secret to be configured. |
| `authn.social.wechat.client_id` | item | string |  | -- | false | false | authn | WeChat OAuth appid. |
| `authn.social.wechat.client_secret` | item | string | [redacted] | -- | true | false | authn | WeChat OAuth secret. |
| `authn.sso.oidc` | flag | bool | false | -- | false | false |  | Allows each tenant to configure an OpenID Connect identity provider its members sign in through. |
| `compliance.default_retention_window` | item | duration | 720h0m0s | 1h0m0s .. -- | false | false | compliance | How long a soft-deleted (mark-deleted) row survives before the periodic retention sweep hard-deletes it. |
| `compliance.export_delivery_expiry` | item | duration | 24h0m0s | 1h0m0s .. 72h0m0s | false | false | compliance | How long a data-export download link (minted through go/sharing) stays valid. Deliberately much shorter than a general-purpose share's own default expiry, since an export bundles a whole tenant's data. |
| `metering.default_overage_threshold` | item | int | 0 | 0 .. -- | false | false | metering | The default per-period usage quantity, applied to any feature with no threshold of its own, above which metering publishes an overage-threshold-crossed event. Zero means no default threshold. |
| `metering.period_bucket_size` | item | string | monthly | -- | false | false | metering | The calendar bucket real-time usage counters and usage-summary rows reset on: "daily" or "monthly". |
| `org.invitation_email` | flag | bool | true | -- | false | false |  | Let org deliver the invitation email itself, rather than leaving delivery to a notification module. (depends on `org.invitations`) |
| `org.invitations` | flag | bool | true | -- | false | false |  | Allow members of this tenant to invite people into its organization. |
| `pki.ca_default_validity` | item | duration | 87600h0m0s | -- | false | false | pki | Default validity period for a newly issued CA certificate (root or intermediate) when the caller does not specify one. |
| `pki.ca_max_validity` | item | duration | 131400h0m0s | -- | false | false | pki | Maximum validity period a CA certificate may be issued for, regardless of what the caller requests. |
| `pki.certificate_default_validity` | item | duration | 8760h0m0s | -- | false | false | pki | Default validity period for a newly issued end-entity certificate when the caller does not specify one. |
| `pki.certificate_max_validity` | item | duration | 17520h0m0s | -- | false | false | pki | Maximum validity period an end-entity certificate may be issued for, regardless of what the caller requests. |
| `pki.crl_distribution_point` | item | string |  | -- | false | false | pki | Default CRL distribution point URL for newly created authorities. Empty means no CRLDistributionPoints extension is written into certificates by default. |
| `pki.crl_validity` | item | duration | 168h0m0s | -- | false | false | pki | How long a generated CRL claims to be current (NextUpdate minus ThisUpdate) before it should be regenerated. |
| `pki.propagation_window` | item | duration | 2m30s | -- | false | false | pki | How long a newly staged pending signing key waits before the expiry scan promotes it to active. |
| `pki.renewal_lead_time` | item | duration | 720h0m0s | -- | false | false | pki | How far ahead of a signing key's expiry the expiry scan stages its replacement. |
| `sharing.default_expiry` | item | duration | 720h0m0s | -- | false | false | sharing | Default share-link expiry applied when a caller does not specify one. |

The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose "enabled" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries each row as structured fields (`layer`, `key`, `module`, `scope`, `sensitive`, `default`, ...) plus the module declarations. Concrete example rows of the `configs` table itself -- a platform row and a tenant override for authn and sharing items, the Sensitive handling included -- live in `docs/config-row-examples.json`.

<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: 35 bootstrap variable(s), 6 module-declared bootstrap key(s), 42 dynamic item(s)/flag(s) (10 flag(s), 5 sensitive) across 6 declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->
