/**
 * SocialBindingsSection: the social-account bindings surface of the
 * account page.
 *
 * Lists every external identity the authn module holds bound to the
 * signed-in account, through the generated list hook. Each row names the
 * provider (mapped through bindings.provider.<provider> for the five
 * providers the spec hosts -- a value outside that set, a future
 * channel, renders the generic "other" label, never a raw value) and the
 * provider account's email when the answer carries one. A row whose
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
 * Empty and failure states render one ui-kit EmptyState (empty / error
 * variant with a retry button) with the section header hidden, so the
 * heading order never skips a level. The one exception is a first-run
 * account with an unbound configured provider: there the empty list is
 * exactly the add area's cue, so the header stays and the add area is
 * the whole of the content.
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
 * a value on this list -- anything else (a future channel) renders the
 * generic bindings.provider.other label.
 */
const KNOWN_PROVIDERS = new Set<string>([
  'google',
  'github',
  'wechat',
  'dingtalk',
  'feishu',
])

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
  const { data, isLoading, isError, refetch } = useAuthnListIdentities()
  const unbindMutation = useAuthnUnbindIdentity()

  const [unbindTarget, setUnbindTarget] = useState<string | null>(null)
  const [busyProvider, setBusyProvider] = useState<SocialProvider | null>(null)
  const [notice, setNotice] = useState<Notice>(null)
  const [authorizeError, setAuthorizeError] = useState<string | null>(null)

  const identities = data?.identities
  const pending = isLoading && identities === undefined
  const failed = !isLoading && identities === undefined && isError
  const rows =
    !isLoading && !failed && identities !== undefined ? identities : undefined

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
  // in for the section header, so the heading order never skips a level.
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
      ) : failed ? (
        <EmptyState
          variant="error"
          title={t('bindings.error.title')}
          description={t('bindings.error.description')}
          action={
            <Button onClick={() => void refetch()}>{t('bindings.retry')}</Button>
          }
        />
      ) : showEmptyState ? (
        <EmptyState
          variant="empty"
          title={t('bindings.empty.title')}
          description={t('bindings.empty.description')}
        />
      ) : rows === undefined ? null : (
        <Box>
          {notice?.kind === 'unbind-failed' && <InlineError code={notice.code} />}
          {/* The identity rows are one real list, as the sessions rows
              are: a screen-reader user hears each binding as one item
              of a numbered set. The notice above and the add area
              below are page-level content and stay outside the list. */}
          <Box component="ul" sx={{ m: 0, p: 0, listStyle: 'none' }}>
            {rows.map((identity, index) => {
              const id = identity.id ?? null
              const provider = identity.provider ?? null
              const providerLabel =
                provider !== null && KNOWN_PROVIDERS.has(provider)
                  ? t(`bindings.provider.${provider}`)
                  : t('bindings.provider.other')
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
