/**
 * LoginHistorySection: the sign-in history surface of the account page.
 *
 * Renders the account's login attempts, newest page of the server's
 * answer first, read through the generated list hook with the page size
 * frozen at 20 -- within the spec's 1..200 limit, sized to render a
 * page of history without scrolling machinery, and deliberately not
 * paginated: login history is a longest-tail surface and the section is
 * a read-only account page, not a search tool. Rows show the method
 * (password, SMS code, social account or enterprise SSO -- values
 * outside that set, a future channel, render an "other" label, never a
 * raw value), the outcome, and when the attempt happened plus the IP it
 * came from. Times render through Intl in the surface's current
 * language, never hand-formatted.
 *
 * Outcome text is guarded server vocabulary, the same discipline as the
 * error-code whitelist elsewhere in the package: a successful attempt
 * renders the success label; a failed one resolves its failure_reason
 * token (a bare token like bad_password, not an errors.<code> style
 * code -- the authn login surface records bare reason tokens) through
 * the history.reason bundle keys, but only tokens on the known list are
 * ever passed to t() -- a reason outside the list, or an absent one,
 * renders the generic sign-in-failed label. No raw token and no API
 * message field can ever reach the row.
 *
 * The section takes no props: whose history this is comes from the
 * caller's bound client and its access token. Empty and failure states
 * hide the section header entirely and render one ui-kit EmptyState
 * (empty / error variant with a retry button), so the heading order
 * never skips a level.
 */

import { useMemo } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Skeleton from '@mui/material/Skeleton'
import Typography from '@mui/material/Typography'
import {
  useAuthnListLoginHistory,
} from '@speed/api-sdk'
import { EmptyState } from '@speed/ui-kit'
import { useAccountUiTranslation } from './internal/translation.js'

/**
 * The frozen page size of this surface: one page of the server's login
 * history, within the spec's 1..200 limit, without pagination (a
 * read-only account page shows the recent attempts, not a search tool).
 */
const PAGE_SIZE = 20

/**
 * The failure-reason tokens the authn login surface records, one bundle
 * key each under history.reason. t() is only ever called with a token on
 * this list -- anything else (a future reason) renders the generic
 * history.reason.other label.
 */
const KNOWN_REASON_TOKENS = new Set<string>([
  'unknown_user',
  'bad_password',
  'no_password',
  'bad_code',
  'suspended',
  'no_membership',
  'requires_binding',
])

/**
 * The method values the authn login surface records, one bundle key each
 * under history.method. A value outside this list (a future channel)
 * renders the generic "other" label, never a raw value.
 */
const KNOWN_METHOD_TOKENS = new Set<string>([
  'password',
  'sms',
  'social',
  'oidc',
])

/** Parses a server timestamp; answers null for an absent or malformed
 * value so a bad row can never reach Intl and throw. */
function parseDate(iso: string | undefined): Date | null {
  if (iso === undefined) {
    return null
  }
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? null : date
}

/** The pending-state placeholder, read aloud as one loading
 * announcement. */
function HistoryListSkeleton({ label }: { readonly label: string }) {
  const widths = [
    { primary: '30%', secondary: '46%' },
    { primary: '42%', secondary: '38%' },
    { primary: '26%', secondary: '52%' },
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
            <Skeleton variant="text" width="14%" />
          </Box>
          <Skeleton variant="text" width={width.secondary} />
        </Box>
      ))}
    </Box>
  )
}

export function LoginHistorySection() {
  const { t, i18n } = useAccountUiTranslation()
  const { data, isLoading, isError, refetch } = useAuthnListLoginHistory({
    limit: PAGE_SIZE,
  })

  const formatTime = useMemo(
    () =>
      new Intl.DateTimeFormat(i18n.language, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }),
    [i18n.language],
  )

  const attempts = data?.attempts
  const pending = isLoading && attempts === undefined
  const failed = !isLoading && attempts === undefined && isError
  const hasRows =
    !isLoading && !failed && attempts !== undefined && attempts.length > 0

  return (
    <Box>
      {(pending || hasRows) && (
        <Box sx={{ mb: 2 }}>
          <Typography variant="h5" component="h2">
            {t('history.title')}
          </Typography>
        </Box>
      )}

      {pending ? (
        <HistoryListSkeleton label={t('history.loading')} />
      ) : failed ? (
        <EmptyState
          variant="error"
          title={t('history.error.title')}
          description={t('history.error.description')}
          action={
            <Button onClick={() => void refetch()}>{t('history.retry')}</Button>
          }
        />
      ) : attempts === undefined || attempts.length === 0 ? (
        <EmptyState
          variant="empty"
          title={t('history.empty.title')}
          description={t('history.empty.description')}
        />
      ) : (
        <Box>
          {attempts.map((attempt, index) => {
            const method =
              attempt.method != null &&
              KNOWN_METHOD_TOKENS.has(attempt.method)
                ? attempt.method
                : null
            const methodLabel =
              method !== null
                ? t(`history.method.${method}`)
                : t('history.method.other')
            const success = attempt.result === 'success'
            const statusColor = success ? 'success.main' : 'error.main'
            const reason =
              attempt.failure_reason != null &&
              KNOWN_REASON_TOKENS.has(attempt.failure_reason)
                ? attempt.failure_reason
                : null
            const statusText = success
              ? t('history.result.success')
              : reason !== null
                ? t(`history.reason.${reason}`)
                : t('history.reason.other')
            const created = parseDate(attempt.created_at)
            return (
              <Box
                key={attempt.id ?? String(index)}
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
                    alignItems: 'baseline',
                    gap: 1.5,
                  }}
                >
                  <Typography variant="body1" sx={{ fontWeight: 500 }}>
                    {methodLabel}
                  </Typography>
                  <Typography
                    variant="body2"
                    color={statusColor}
                    sx={{ marginLeft: 'auto', flexShrink: 0 }}
                  >
                    {statusText}
                  </Typography>
                </Box>
                {(created !== null || attempt.ip != null) && (
                  <Box
                    sx={{
                      display: 'flex',
                      flexWrap: 'wrap',
                      columnGap: 2.5,
                      rowGap: 0.5,
                      mt: 0.5,
                    }}
                  >
                    {created !== null && (
                      <Typography variant="body2" color="text.secondary">
                        {formatTime.format(created)}
                      </Typography>
                    )}
                    {attempt.ip != null && (
                      <Typography variant="body2" color="text.secondary">
                        {attempt.ip}
                      </Typography>
                    )}
                  </Box>
                )}
              </Box>
            )
          })}
        </Box>
      )}
    </Box>
  )
}
