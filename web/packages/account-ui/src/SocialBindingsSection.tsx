/**
 * SocialBindingsSection: the social-account bindings surface of the
 * account page.
 *
 * Lists every external identity the authn module holds bound to the
 * signed-in account, through the generated list hook. Each row names the
 * provider (mapped through bindings.provider.<provider> for the five
 * social providers the spec hosts and through the bindings.provider.oidc
 * family label for the enterprise-SSO channel, whose live provider
 * values are the dynamic per-tenant names "oidc:<tenant>" -- the raw
 * value renders as the row's reference line so one tenant's binding
 * never reads as another's; a value outside both sets, a future
 * channel, renders the generic "other" label, never a raw value) and
 * the provider account's email when the answer carries one. A row whose
 * identity carries an id has a row-end "unlink" action that sits behind
 * the ui-kit danger ConfirmDialog: unbinding is irreversible (the
 * binding is re-established only by walking the provider's OAuth flow
 * again), so the row asks once before the DELETE goes out. After a
 * successful unbind the list query is invalidated, so the rows converge
 * on the server's answer; a refused unbind renders its code text above
 * the list -- authn.last_login_method when the account would be left
 * with no sign-in method (the server decides by its own login-method
 * count, never computable client-side), authn.identity_not_found when
 * the row was already unbound elsewhere (a race; the list refetches so
 * the stale row disappears), anything else through the whitelist.
 *
 * The add area renders one button per configured provider that is not
 * already bound; clicking asks the session for that channel's
 * authorization URL -- a pure request reported upward through
 * onAuthorizeUrl, never a navigation the package performs -- and the
 * host completes the flow at its callback route with
 * BindingCallbackHandler. One bind flow at a time: while a channel's
 * URL is being built every provider button is disabled, so a second
 * authorize request -- any channel's -- cannot start before the first
 * answers. When every configured provider is already bound the add
 * area does not render.
 *
 * The provider vocabulary is deliberately not imported from @speed/auth-ui
 * (same-layer packages never import each other): SocialProvider and
 * SocialProviderConfig are copied here, shaped identically to auth-ui's
 * own definitions, and must be kept in sync with them -- the authn spec
 * is the shared source of truth for the provider set, and a social
 * channel added to the spec lands in both packages' copies in the same
 * round.
 *
 * An unresolved load -- the first load in flight, or parked by
 * react-query's default networkMode 'online' while the device is
 * offline -- keeps the section header and the loading skeleton; the
 * block must never silently vanish while the answer has not arrived.
 * Settled states render one ui-kit EmptyState with the section header
 * hidden, so the heading order never skips a level: an answer listing
 * zero identities renders the empty variant, while a load that failed
 * -- and a successful answer that omits the optional identities key, as
 * unreadable as an error and never grounds for a "no linked accounts"
 * claim or an add area -- render the error variant with a retry button.
 * The one exception is a first-run account with an unbound configured
 * provider: there a genuinely empty list is exactly the add area's cue,
 * so the header stays and the add area is the whole of the content.
 */

import { useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Skeleton from '@mui/material/Skeleton'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import {
  getAuthnListIdentitiesQueryKey,
  useAuthnListIdentities,
  useAuthnUnbindIdentity,
} from '@speed/api-sdk'
import type { AuthSession } from '@speed/auth-core'
import { ConfirmDialog, EmptyState } from '@speed/ui-kit'
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { useAccountUiTranslation } from './internal/translation.js'

/**
 * The social sign-in channels the authn spec hosts -- the account-ui
 * copy of @speed/auth-ui's SocialProvider, kept in sync with it (the
 * same-layer rule forbids importing the sign-in package).
 */
export type SocialProvider =
  | 'google'
  | 'github'
  | 'wechat'
  | 'dingtalk'
  | 'feishu'

/**
 * One configured channel: which provider, and the redirect URI the
 * host's callback route listens on. The account-ui copy of auth-ui's
 * SocialProviderConfig, kept in sync with it.
 */
export interface SocialProviderConfig {
  readonly provider: SocialProvider
  readonly redirectUri: string
}

export interface SocialBindingsSectionProps {
  /** The session that builds authorization URLs for the add area. */
  readonly session: AuthSession
  /** The channels the host offers for binding, in the order wanted. */
  readonly providers: readonly SocialProviderConfig[]
  /**
   * Receives the authorization URL for one channel once it is built;
   * the host navigates. Omit to run the section without a follow-up
   * (a caller that only exercises the request path).
   */
  readonly onAuthorizeUrl?: (url: string) => void
}

/** The transient outcome of an unbind action, rendered above the list. */
type Notice = { readonly kind: 'unbind-failed'; readonly code: string } | null

/**
 * The provider values the authn identity surface answers with, one
 * bundle key each under bindings.provider. t() is only ever called with
 * a value on this list -- anything else either carries the enterprise
 * OIDC_PROVIDER_PREFIX below (the live oidc: family, dynamic per
 * tenant, mapped by its prefix and never enumerable) or renders the
 * generic bindings.provider.other label.
 */
const KNOWN_PROVIDERS = new Set<string>([
  'google',
  'github',
  'wechat',
  'dingtalk',
  'feishu',
])

/**
 * The enterprise-SSO provider family prefix: go/authn stores each
 * tenant's enterprise connection binding under the per-tenant provider
 * name "oidc:<tenant>", so the value set is dynamic -- enumeration can
 * never suffice. A provider value carrying this prefix renders the
 * bindings.provider.oidc family label with the raw value as the row's
 * reference line (the label maps the family, the raw value tells one
 * tenant's binding from another's).
 */
const OIDC_PROVIDER_PREFIX = 'oidc:'

/** The pending-state placeholder: rows shaped like the content rows,
 * read aloud as one loading announcement, never a fake heading. */
function BindingListSkeleton({ label }: { readonly label: string }) {
  const widths = [
    { primary: '38%', secondary: '52%' },
    { primary: '28%', secondary: '46%' },
    { primary: '34%', secondary: '58%' },
  ]
  return (
    <Box role="status" aria-label={label} aria-busy="true">
      {widths.map((width, index) => (
        <Box
          key={String(index)}
          sx={{
            py: 1.5,
            ...(index > 0
              ? { borderTop: '1px solid', borderColor: 'divider' }
              : {}),
          }}
        >
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
            }}
          >
            <Skeleton variant="text" width={width.primary} />
            <Skeleton variant="text" width="22%" />
          </Box>
          <Skeleton variant="text" width={width.secondary} />
        </Box>
      ))}
    </Box>
  )
}

export function SocialBindingsSection({
  session,
  providers,
  onAuthorizeUrl,
}: SocialBindingsSectionProps) {
  const { t } = useAccountUiTranslation()
  const queryClient = useQueryClient()
  const { data, isPending, refetch } = useAuthnListIdentities()
  const unbindMutation = useAuthnUnbindIdentity()

  const [unbindTarget, setUnbindTarget] = useState<string | null>(null)
  const [busyProvider, setBusyProvider] = useState<SocialProvider | null>(null)
  const [notice, setNotice] = useState<Notice>(null)
  const [authorizeError, setAuthorizeError] = useState<string | null>(null)

  const identities = data?.identities
  // The answer is unresolved whenever there is no data yet: the first
  // load in flight, or parked by react-query's default networkMode
  // 'online' while the device is offline -- such a fetch sits at
  // fetchStatus 'paused', where isFetching (and isLoading, its
  // isPending-and-isFetching conjunction) is false and isError stays
  // false too, so a pending test derived from isLoading would miss
  // every branch and the block would silently vanish. isPending alone
  // tracks "no answer yet" across the in-flight and the parked states.
  const pending = isPending && identities === undefined
  const rows =
    !pending && identities !== undefined ? identities : undefined

  const boundProviders =
    rows === undefined
      ? new Set<string>()
      : new Set(
          rows
            .map((identity) => identity.provider)
            .filter((provider): provider is string => provider != null),
        )
  const available = providers.filter(
    (config) => !boundProviders.has(config.provider),
  )
  const hasAddArea = rows !== undefined && available.length > 0
  // Empty list with nothing to offer: the EmptyState's own title stands
  // in for the section header (headingLevel="h2" below keeps it at the
  // hidden header's own level, rather than skipping to EmptyState's h6
  // default), so the heading order never skips a level.
  const showEmptyState = rows !== undefined && rows.length === 0 && !hasAddArea
  const showHeader =
    pending || (rows !== undefined && (rows.length > 0 || hasAddArea))

  async function handleUnbind(identityId: string): Promise<void> {
    setNotice(null)
    try {
      await unbindMutation.mutateAsync({ identityId })
      setUnbindTarget(null)
      await queryClient.invalidateQueries({
        queryKey: getAuthnListIdentitiesQueryKey(),
      })
    } catch (error) {
      const code = errorCodeOf(error)
      setUnbindTarget(null)
      setNotice({ kind: 'unbind-failed', code })
      // The row was already unbound elsewhere: refetch so the stale
      // row disappears and the list converges on the server's answer,
      // then clear the banner -- the race it announced is over.
      if (code === 'authn.identity_not_found') {
        await queryClient.invalidateQueries({
          queryKey: getAuthnListIdentitiesQueryKey(),
        })
        setNotice(null)
      }
    }
  }

  async function handleAuthorize(config: SocialProviderConfig): Promise<void> {
    // One bind flow at a time, across every provider: the busy slot is
    // provider-wide, so a second authorize click while a URL is being
    // built -- this channel's or any other's -- is refused outright
    // instead of racing the in-flight request.
    if (busyProvider !== null) {
      return
    }
    setAuthorizeError(null)
    setBusyProvider(config.provider)
    try {
      const authorizeUrl = await session.socialAuthorizeUrl(config.provider, {
        redirect_uri: config.redirectUri,
      })
      onAuthorizeUrl?.(authorizeUrl)
    } catch (error) {
      setAuthorizeError(errorCodeOf(error))
    } finally {
      setBusyProvider(null)
    }
  }

  const unbindBusy =
    unbindTarget !== null && unbindMutation.isPending

  return (
    <Box>
      {showHeader && (
        <Box sx={{ mb: 2 }}>
          <Typography variant="h5" component="h2">
            {t('bindings.title')}
          </Typography>
        </Box>
      )}

      {pending ? (
        <BindingListSkeleton label={t('bindings.loading')} />
      ) : rows === undefined ? (
        // This guard tests the absent list field with the loading
        // branch already excluded above: pending is false here, so no
        // rows means the query settled without delivering a list. Two
        // shapes settle that way: a load that failed with no data
        // (isError), and a successful answer whose body omits the
        // optional identities key -- AuthnListIdentitiesResponse marks
        // `.identities` optional, so a type-legal 200 `{}` carries data
        // yet no list, and nothing would re-arm the loading branch for
        // it (isPending is false and stays false). Both land on the
        // error state, never the loading skeleton, which nothing could
        // resolve: the error copy claims no account content, where
        // reading the field-less answer as "no linked accounts" would
        // fabricate a statement the answer never made -- and would arm
        // the add area's binding offers on that fabrication -- and its
        // Retry is the exit.
        <EmptyState
          variant="error"
          title={t('bindings.error.title')}
          description={t('bindings.error.description')}
          // The retry is the exit for both shapes this state covers. A
          // refetch of a data-less failed load moves the query back to
          // the pending state, so the section re-enters the loading
          // branch above -- that loading announcement is the retry's
          // progress feedback; a refetch of a settled field-less answer
          // keeps the query's own data, so this state holds until the
          // refetched answer changes it. Either way react-query dedupes
          // the per-query fetches, so a click can never overlap a
          // request already in flight.
          action={
            <Button onClick={() => void refetch()}>{t('bindings.retry')}</Button>
          }
          // The section header is hidden whenever this renders (see the
          // `showHeader` derivation above), so this EmptyState's title
          // takes over the section's own heading level.
          headingLevel="h2"
        />
      ) : showEmptyState ? (
        <EmptyState
          variant="empty"
          title={t('bindings.empty.title')}
          description={t('bindings.empty.description')}
          headingLevel="h2"
        />
      ) : (
        <Box>
          {notice?.kind === 'unbind-failed' && <InlineError code={notice.code} />}
          {/* The identity rows are one real list, as the sessions rows
              are: a screen-reader user hears each binding as one item
              of a numbered set. The notice above and the add area
              below are page-level content and stay outside the list.
              role="list" keeps the list semantics under WebKit, which
              strips them from a list-style-none ul. */}
          <Box component="ul" role="list" sx={{ m: 0, p: 0, listStyle: 'none' }}>
            {rows.map((identity, index) => {
              const id = identity.id ?? null
              const provider = identity.provider ?? null
              // Enterprise-SSO bindings arrive under the dynamic
              // per-tenant provider name "oidc:<tenant>": the family
              // label renders for the prefix, and the raw value stays
              // in the row as its reference line, so two SSO bindings
              // in two tenants never read as one identical row.
              const oidcProvider =
                provider !== null &&
                provider.startsWith(OIDC_PROVIDER_PREFIX)
                  ? provider
                  : null
              const providerLabel =
                provider !== null && KNOWN_PROVIDERS.has(provider)
                  ? t(`bindings.provider.${provider}`)
                  : oidcProvider !== null
                    ? t('bindings.provider.oidc')
                    : t('bindings.provider.other')
              // The row identity the unbind action is named after (the
              // sessions twin names its revoke action after the row's
              // device label): the raw provider value when the label
              // is a shared family label, the label itself otherwise.
              const unbindIdentity = oidcProvider ?? providerLabel
              const email =
                identity.email != null && identity.email !== ''
                  ? identity.email
                  : null
              return (
                <Box
                  component="li"
                  key={id ?? String(index)}
                  sx={{
                    py: 1.5,
                    minWidth: 0,
                    ...(index > 0
                      ? { borderTop: '1px solid', borderColor: 'divider' }
                      : {}),
                  }}
                >
                  <Box
                    sx={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 1.5,
                    }}
                  >
                    <Box sx={{ minWidth: 0 }}>
                      <Typography variant="body1" sx={{ fontWeight: 500 }}>
                        {providerLabel}
                      </Typography>
                      {oidcProvider !== null && (
                        <Typography
                          variant="body2"
                          color="text.secondary"
                          noWrap
                          title={oidcProvider}
                          sx={{ minWidth: 0 }}
                        >
                          {oidcProvider}
                        </Typography>
                      )}
                      {email !== null && (
                        <Typography
                          variant="body2"
                          color="text.secondary"
                          noWrap
                        >
                          {email}
                        </Typography>
                      )}
                    </Box>
                    {id !== null && (
                      <Box sx={{ marginLeft: 'auto', flexShrink: 0 }}>
                        <Button
                          variant="outlined"
                          color="error"
                          size="small"
                          disabled={unbindBusy}
                          aria-label={t('bindings.unbindAriaWithProvider', {
                            provider: unbindIdentity,
                            // The label is an aria-label, not HTML: the
                            // row identity (an oidc:<tenant> raw value
                            // included) must reach the accessibility
                            // tree verbatim (i18next's default value
                            // escaping would embed a literal `&#x2F;`).
                            interpolation: { escapeValue: false },
                          })}
                          onClick={() => setUnbindTarget(id)}
                        >
                          {t('bindings.unbind')}
                        </Button>
                      </Box>
                    )}
                  </Box>
                </Box>
              )
            })}
          </Box>

          {hasAddArea && (
            <AddArea
              available={available}
              busy={busyProvider !== null}
              authorizeError={authorizeError}
              sectionLabel={t('bindings.addSectionTitle')}
              providerLabel={(provider) => t(`bindings.provider.${provider}`)}
              onAuthorize={handleAuthorize}
            />
          )}
        </Box>
      )}

      <ConfirmDialog
        open={unbindTarget !== null}
        title={t('bindings.confirmTitle')}
        message={t('bindings.confirmMessage')}
        variant="danger"
        confirmLabel={t('bindings.confirmLabel')}
        confirmLoading={unbindMutation.isPending}
        onCancel={() => setUnbindTarget(null)}
        onConfirm={() => {
          if (unbindTarget !== null) {
            void handleUnbind(unbindTarget)
          }
        }}
      />
    </Box>
  )
}

/** The add area: one button per unbound configured provider, plus the
 * authorize-path failure banner. Rendered only when the list has
 * loaded and at least one configured provider is unbound. While a bind
 * flow is building every button is disabled together -- the busy flag
 * is provider-wide, never one channel's alone. */
function AddArea({
  available,
  busy,
  authorizeError,
  sectionLabel,
  providerLabel,
  onAuthorize,
}: {
  readonly available: readonly SocialProviderConfig[]
  /** True while any channel's authorization URL is being built. */
  readonly busy: boolean
  readonly authorizeError: string | null
  readonly sectionLabel: string
  readonly providerLabel: (provider: SocialProvider) => string
  readonly onAuthorize: (config: SocialProviderConfig) => void
}) {
  return (
    <Box sx={{ mt: 2, borderTop: '1px solid', borderColor: 'divider', pt: 2 }}>
      <Typography
        variant="body2"
        color="text.secondary"
        sx={{ mb: 1.5 }}
      >
        {sectionLabel}
      </Typography>
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'flex-start',
          gap: 1,
        }}
      >
        {available.map((config) => (
          <Button
            key={config.provider}
            variant="outlined"
            disabled={busy}
            onClick={() => onAuthorize(config)}
          >
            {providerLabel(config.provider)}
          </Button>
        ))}
      </Box>
      <InlineError code={authorizeError} />
    </Box>
  )
}
