# Configuration reference

This reference lists every configuration value a speed-based application built from this repository's modules resolves: the **bootstrap** layer, the process-start input the modules declare and a host resolves before anything else starts, and the **dynamic** layer, the configuration items and feature flags an operator edits at runtime. It is generated from the live configuration schema and the modules' own declarations, never hand-written -- root CLAUDE.md's documentation discipline that "the configuration reference is generated from the config schema" applies to exactly this document; a stale reference is a CI failure.

How it is built: the dynamic layer is enumerated through `config.Service.Describe` from the frozen schema of a composed host that registers every platform module whose declarations fold into the schema (authn, metering, compliance, sharing, pki and org), and the bootstrap layer renders those same modules' `reg.Bootstrap` declarations -- the two sources are the modules' own registration-time declarations, never a hand-kept list. Every committed output -- this document, its JSON twin `docs/config-reference.json`, the derived `config.example.json` and the documentation site's copy of this page -- is byte-identical across runs; regeneration is `go run .` from `tools/configrefgen/`.

## Bootstrap configuration

The bootstrap layer is the process-start input a speed-based application resolves once, before anything else is wired; it has no tenant dimension and no runtime edit surface, so a change takes effect at the next start. Its keys are declared by the modules that consume them: a module states each key's contract on the registry's bootstrap seat (`reg.Bootstrap.Add`) while it registers, and never resolves the value itself -- the host does, and injects it. A module that consumes no process-start input declares nothing, which is an honest state rather than a gap.

The mechanism a host drives to resolve the values is `go/pkgcore/config`. A host that drives the loader declares a target struct whose fields are the keys: the field `Database.DSN` is the key `database.dsn`, the flag `--database.dsn`, and -- under the loader's default prefix -- the environment variable `SPEED_DATABASE__DSN`, where a double underscore marks each level of nesting; a single underscore is never a nesting marker. `WithEnvPrefix` replaces the prefix for a host whose variables already carry another one, and a field whose variable name does not derive from its key can pin that exact name (`config:"env=PORT"`, the customary unprefixed name for the port a platform tells the process to listen on), after which the field reads that name and no other. Every key resolves from four sources, highest priority first: command-line flags, environment variables, an optional YAML or JSON config file (`WithConfigFile`; an absent file is skipped silently, a malformed one is a hard error), then the defaults already set on the target struct. A value supplied by a text source is judged as text: a field that can hold an empty value takes it, and a field with no representation for one -- a number, a bool -- refuses the load rather than quietly becoming that field's zero value. `config.Verify(target, declared)` then checks a declared key list against a target struct, every declared key mapping onto a field, which is how a host proves its target binds the keys its modules declared.

A paired example of this file source is committed at the repository root: `config.example.yaml` and its derived `config.example.json`, covering the platform keys a host must feed and a demonstration of a host's own keys, in both accepted formats.

Secret materials (Sensitive above) must come from a secret store in a real deployment, never from a committed file. The Unset fallback column states what an unset key resolves to; for key materials that is a documented, recognizable, NON-SECRET development default a real deployment must override, and each declaration's own text says what its key protects and how it is isolated from every other key.

A deployment that would rather manage one secret than one per key can derive a declared key's material from a single 32-byte root key: the key path's purpose string is a platform convention fixed at the bootstrap seat (`pkgcore.BootstrapKeyPurpose` -- the literal `"speed." + keyPath + ".v1"`, one per declared key path), and `dbkit.DeriveKey(rootKey, purpose)` (HKDF-SHA256) turns the root key plus that purpose into the key's 32-byte material -- `speed.config.cipher_key.v1` for `config.cipher_key`, `speed.authn.blind_index_key.v1` and `speed.authn.pii_cipher_key.v1` for authn's two, and `speed.notification.contact_index_key.v1`, `speed.org.invitation_email_index_key.v1` and `speed.pki.local_key_cipher_key.v1` for the remaining three. Which variable carries the root key, and which carries a single key's own override, is the host's choice: the names belong to the host, and the platform only receives the bytes. Precedence is the host's to apply, and the supported shape is "explicit beats derived" -- an individually configured key always wins over the value derived for it. Because a purpose embeds the declared key path, renaming a declared key path is a rotation of that key's material, and must ship as one.

### Bootstrap keys declared by platform modules

Each platform module declares the process-start keys it consumes on the registry's bootstrap seat, so the key's contract -- what it protects, why it is a separate secret, what an operator should expect when it is unset -- travels with the module instead of living in a host's own notes. The tables below are rendered from `reg.Bootstrap` itself. The Env variable column is the name the loader derives from the key path under its default prefix, which is what an unpinned host field reads; a host that pins a different name for its own field is exercising its own naming choice.

**authn**

| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |
|---|---|---|---|---|---|
| `authn.blind_index_key` | `SPEED_AUTHN__BLIND_INDEX_KEY` | hexkey | true | documented non-secret development default | HMAC key authn indexes its users.email_index and phone_index blind-index columns with; it must stay identical across restarts or every already-stored email and phone index becomes unfindable, and an HMAC key never doubles as a cipher key. |
| `authn.pii_cipher_key` | `SPEED_AUTHN__PII_CIPHER_KEY` | hexkey | true | documented non-secret development default | AES key sealing authn's encrypted PII columns (email, phone, TOTP secrets), deliberately separate from every other module's key material and from authn's own blind-index key below. |

**config**

| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |
|---|---|---|---|---|---|
| `config.cipher_key` | `SPEED_CONFIG__CIPHER_KEY` | hexkey | true | documented non-secret development default | The AES cipher key the config module seals every Sensitive dynamic-configuration value with (the configs table stores base64 ciphertext); the key that encrypts the table cannot live in the table, so it comes from the host's process-start input. |

**notification**

| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |
|---|---|---|---|---|---|
| `notification.contact_index_key` | `SPEED_NOTIFICATION__CONTACT_INDEX_KEY` | hexkey | true | documented non-secret development default | HMAC key the notification module's blind indexers index its encrypted contact addresses with; one key serves the email and phone indexers, whose canonical forms are disjoint, and it stays separate from every cipher key. |

**org**

| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |
|---|---|---|---|---|---|
| `org.invitation_email_index_key` | `SPEED_ORG__INVITATION_EMAIL_INDEX_KEY` | hexkey | true | documented non-secret development default | HMAC key org's blind indexer indexes invitation email addresses with; separate from every cipher key, because an AES key never doubles as an HMAC key. |

**pki**

| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |
|---|---|---|---|---|---|
| `pki.local_key_cipher_key` | `SPEED_PKI__LOCAL_KEY_CIPHER_KEY` | hexkey | true | documented non-secret development default | AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with; separate from every other key, since dbkit's key-separation rule spans modules, not only one. |

A key belongs to exactly one configuration layer. A bootstrap key is process-start input, resolved once and fixed for the process's lifetime; a runtime item (the next section) is a per-tenant value an operator edits while the process runs. Declaring the same dotted key on both layers is refused at startup and in this generator, because one identifier cannot carry two meanings, two defaults and two edit surfaces.

## Dynamic configuration

The dynamic layer holds the configuration an operator edits at runtime, served by the `go/config` module: every module declares the items and feature flags it owns during `Register` (`reg.Config.Add` / `reg.Features.Add`), and `config.Module.Attach` freezes them into one schema the moment Bootstrap returns. Values live in the shared `configs` table (platform data, keyed by `(key, scope, tenant_id)`) under two scope tiers: a **system** row is platform-wide (writing one requires an audited system context), a **tenant** row overrides it for one tenant, and a read resolves tenant row -> system row -> the schema default. Scope is a property of the row, not of the key: every key below may hold a row at either tier. A key with no default and no row at any reachable scope has no value to serve (`config.item_unset`). Writes publish `config.item.changed` (with `[redacted]` markers in both value slots for a Sensitive item) and produce an audit record.

Sensitive items are encrypted at rest: the `configs` table stores `base64(ciphertext)` sealed by the host's `dbkit.Cipher` key, never plaintext, and are never served on the public endpoint, whose rows are exactly the items marked Public below (`/api/v1/config/public`, `config.PathPublic`). A Sensitive item's default is redacted in this very table (the `[redacted]` marker), because this document is a committed artifact a secret's plaintext has no more business crossing than the event bus.

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

The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose "enabled" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries both layers as flat row lists: a bootstrap entry holds `key`, `module`, `env`, `type`, `unset_fallback`, `sensitive` and `description`; a dynamic item's structured fields follow (`key`, `module`, `scope`, `sensitive`, `default`, ...).
<!-- Generated by tools/configrefgen (go run . from tools/configrefgen/). Do not hand-edit. Reference stats: 6 module-declared bootstrap key(s), 42 dynamic item(s)/flag(s) (10 flag(s), 5 sensitive) across 6 declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->
