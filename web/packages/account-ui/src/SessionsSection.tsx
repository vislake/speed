/**
 * SessionsSection: the sessions-and-devices surface of the account page.
 *
 * Renders every session the authn module holds for the signed-in account,
 * the session the request's own token belongs to marked as the current
 * one, through the generated list hook. Rows show what the server
 * answers: the device string (the user_agent, or an unknown-device label
 * when the answer carries none), the raw IP and AMR values (AMR tokens
 * are opaque authentication-method references -- server vocabulary, not
 * text to translate -- so they render as-is in chips), created/last-seen
 * times, and a status badge telling an active session from a revoked
 * one.
 *
 * Actions: a session that is neither current nor revoked carries a
 * row-end sign-out button that revokes exactly that session, without a
 * second confirmation -- a signed-out session is low-loss (its owner can
 * simply sign in again) and the row itself is never the current one. The
 * section-top action, "sign out other devices", revokes every other
 * session at once and is the heavier gesture: it sits behind the
 * ui-kit danger ConfirmDialog with double confirm. The server itself
 * skips the current session; the answer's revoked_count is surfaced in a
 * success notice. After every successful revoke the list query is
 * invalidated, so the rows converge on the server's answer (revoked
 * sessions stay listed, greyed out).
 *
 * The section takes no props: whose sessions these are, and the right to
 * revoke them, come from the caller's bound client and its access token;
 * the section only renders the list and drives the generated mutations.
 * Empty and failure states hide the section header entirely and render
 * one ui-kit EmptyState (empty / error variant with a retry button), so
 * the heading order never skips a level.
 */

import { useMemo, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Chip from '@mui/material/Chip'
import CircularProgress from '@mui/material/CircularProgress'
import IconButton from '@mui/material/IconButton'
import Skeleton from '@mui/material/Skeleton'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import {
  getAuthnListSessionsQueryKey,
  useAuthnListSessions,
  useAuthnRevokeOtherSessions,
  useAuthnRevokeSession,
} from '@speed/api-sdk'
import { ConfirmDialog, EmptyState } from '@speed/ui-kit'
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { useAccountUiTranslation } from './internal/translation.js'

/** The transient outcome of a revoke action, rendered above the list. */
type Notice =
  | { readonly kind: 'others-done'; readonly count: number }
  | { readonly kind: 'revoke-failed'; readonly code: string }
  | null

/** The row-end sign-out glyph, hand-drawn on the packages' 24-grid
 * pattern: an arrow leaving a door, stroke currentColor, no fill. */
function SignOutIcon({ size = 20 }: { readonly size?: number }) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M9.5 21H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h2.5" />
      <path d="m15.5 17.5 4.5-4.5-4.5-4.5" />
      <path d="M20 13H9" />
    </svg>
  )
}

/** Parses a server timestamp; answers null for an absent or malformed
 * value so a bad row can never reach Intl and throw. */
function parseDate(iso: string | undefined): Date | null {
  if (iso === undefined) {
    return null
  }
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? null : date
}

/** The pending-state placeholder: rows shaped like the content rows, read
 * aloud as one loading announcement, never a fake heading. */
function SessionListSkeleton({ label }: { readonly label: string }) {
  const widths = [
    { primary: '46%', secondary: '62%', tertiary: '38%' },
    { primary: '58%', secondary: '44%', tertiary: '52%' },
    { primary: '38%', secondary: '68%', tertiary: '30%' },
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
            <Skeleton variant="circular" width={20} height={20} />
          </Box>
          <Skeleton variant="text" width={width.secondary} />
          <Skeleton variant="text" width={width.tertiary} />
        </Box>
      ))}
    </Box>
  )
}

export function SessionsSection() {
  const { t, i18n } = useAccountUiTranslation()
  const queryClient = useQueryClient()
  const { data, isLoading, isError, refetch } = useAuthnListSessions()
  const revokeSessionMutation = useAuthnRevokeSession()
  const revokeOthersMutation = useAuthnRevokeOtherSessions()

  const [othersConfirmOpen, setOthersConfirmOpen] = useState(false)
  const [notice, setNotice] = useState<Notice>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)

  const formatTime = useMemo(
    () =>
      new Intl.DateTimeFormat(i18n.language, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }),
    [i18n.language],
  )

  const sessions = data?.sessions
  const pending = isLoading && sessions === undefined
  const failed = !isLoading && sessions === undefined && isError
  const hasSessions =
    !isLoading && !failed && sessions !== undefined && sessions.length > 0
  const busy =
    revokeSessionMutation.isPending || revokeOthersMutation.isPending

  const hasRevocableOther =
    sessions !== undefined &&
    sessions.some(
      (session) =>
        session.id != null &&
        session.is_current !== true &&
        session.status !== 'revoked',
    )

  async function handleRevokeSession(sessionId: string): Promise<void> {
    if (busy || sessionId === revokingId) {
      return
    }
    setNotice(null)
    setRevokingId(sessionId)
    try {
      await revokeSessionMutation.mutateAsync({ sessionId })
      await queryClient.invalidateQueries({
        queryKey: getAuthnListSessionsQueryKey(),
      })
    } catch (error) {
      setNotice({ kind: 'revoke-failed', code: errorCodeOf(error) })
    } finally {
      setRevokingId(null)
    }
  }

  async function handleRevokeOthers(): Promise<void> {
    setNotice(null)
    try {
      const result = await revokeOthersMutation.mutateAsync()
      setOthersConfirmOpen(false)
      setNotice({
        kind: 'others-done',
        count: result?.revoked_count ?? 0,
      })
      await queryClient.invalidateQueries({
        queryKey: getAuthnListSessionsQueryKey(),
      })
    } catch (error) {
      setOthersConfirmOpen(false)
      setNotice({ kind: 'revoke-failed', code: errorCodeOf(error) })
    }
  }

  const showHeader = pending || hasSessions

  return (
    <Box>
      {showHeader && (
        <Box
          sx={{
            display: 'flex',
            alignItems: 'baseline',
            justifyContent: 'space-between',
            gap: 2,
            mb: 2,
          }}
        >
          <Typography variant="h5" component="h2">
            {t('sessions.title')}
          </Typography>
          {hasSessions && hasRevocableOther && (
            <Button
              variant="outlined"
              color="error"
              disabled={busy}
              onClick={() => setOthersConfirmOpen(true)}
            >
              {t('sessions.revokeOthers.label')}
            </Button>
          )}
        </Box>
      )}

      {pending ? (
        <SessionListSkeleton label={t('sessions.loading')} />
      ) : failed ? (
        <EmptyState
          variant="error"
          title={t('sessions.error.title')}
          description={t('sessions.error.description')}
          action={
            <Button onClick={() => void refetch()}>{t('sessions.retry')}</Button>
          }
        />
      ) : sessions === undefined || sessions.length === 0 ? (
        <EmptyState
          variant="empty"
          title={t('sessions.empty.title')}
          description={t('sessions.empty.description')}
        />
      ) : (
        <Box>
          {notice?.kind === 'revoke-failed' && (
            <InlineError code={notice.code} />
          )}
          {notice?.kind === 'others-done' && (
            <Alert
              severity="success"
              role="status"
              sx={{ width: '100%', mb: 1.5 }}
            >
              {t('sessions.revokeOthers.done', { count: notice.count })}
            </Alert>
          )}
          {sessions.map((session, index) => {
            const id = session.id ?? null
            const revoked = session.status === 'revoked'
            const current = session.is_current === true
            const revocable = id !== null && !current && !revoked
            const metaColor = revoked ? 'text.disabled' : 'text.secondary'
            // Line 1 carries the friendliest label the answer offers (its
            // device string when present); the raw user_agent repeats as a
            // muted detail line only when line 1 is not already it.
            const device =
              session.device != null && session.device !== ''
                ? session.device
                : null
            const agent =
              session.user_agent != null && session.user_agent !== ''
                ? session.user_agent
                : null
            const deviceLabel =
              device ?? agent ?? t('sessions.deviceUnknown')
            const agentLine =
              device !== null && agent !== null && agent !== device
                ? agent
                : null
            const created = parseDate(session.created_at)
            const lastSeen = parseDate(session.last_seen_at)
            const showLastSeen =
              lastSeen !== null && session.last_seen_at !== session.created_at
            return (
              <Box
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
                  <Typography
                    variant="body1"
                    noWrap
                    sx={{
                      minWidth: 0,
                      fontWeight: 500,
                      color: revoked ? 'text.disabled' : 'text.primary',
                    }}
                  >
                    {deviceLabel}
                  </Typography>
                  <Box
                    sx={{
                      marginLeft: 'auto',
                      flexShrink: 0,
                      display: 'flex',
                      alignItems: 'center',
                    }}
                  >
                    {current && (
                      <Chip
                        size="small"
                        color="primary"
                        variant="outlined"
                        label={t('sessions.current')}
                      />
                    )}
                    {revoked && (
                      <Chip
                        size="small"
                        variant="outlined"
                        label={t('sessions.status.revoked')}
                        sx={{ color: 'text.disabled' }}
                      />
                    )}
                    {revocable && (
                      <IconButton
                        aria-label={t('sessions.revokeAria')}
                        size="small"
                        disabled={busy || revokingId === id}
                        onClick={() => {
                          if (id !== null) {
                            void handleRevokeSession(id)
                          }
                        }}
                        sx={{ color: 'text.secondary' }}
                      >
                        {revokingId === id ? (
                          <CircularProgress
                            size={18}
                            thickness={5}
                            aria-hidden="true"
                          />
                        ) : (
                          <SignOutIcon />
                        )}
                      </IconButton>
                    )}
                  </Box>
                </Box>

                {agentLine !== null && (
                  <Typography
                    variant="body2"
                    noWrap
                    title={agentLine}
                    color={metaColor}
                    sx={{ minWidth: 0 }}
                  >
                    {agentLine}
                  </Typography>
                )}

                {session.amr != null && session.amr.length > 0 && (
                  <Box
                    sx={{
                      display: 'flex',
                      flexWrap: 'wrap',
                      gap: 0.75,
                      mt: 0.5,
                    }}
                  >
                    {session.amr.map((amr) => (
                      <Chip
                        key={amr}
                        size="small"
                        variant="outlined"
                        label={amr}
                        sx={revoked ? { color: 'text.disabled' } : undefined}
                      />
                    ))}
                  </Box>
                )}

                {(session.ip != null ||
                  created !== null ||
                  showLastSeen) && (
                  <Box
                    sx={{
                      display: 'flex',
                      flexWrap: 'wrap',
                      columnGap: 2.5,
                      rowGap: 0.5,
                      mt: 0.5,
                    }}
                  >
                    {session.ip != null && (
                      <Typography variant="body2" color={metaColor}>
                        {session.ip}
                      </Typography>
                    )}
                    {created !== null && (
                      <Typography variant="body2" color={metaColor}>
                        {t('sessions.signedIn', {
                          time: formatTime.format(created),
                        })}
                      </Typography>
                    )}
                    {showLastSeen && lastSeen !== null && (
                      <Typography variant="body2" color={metaColor}>
                        {t('sessions.lastSeen', {
                          time: formatTime.format(lastSeen),
                        })}
                      </Typography>
                    )}
                  </Box>
                )}
              </Box>
            )
          })}
        </Box>
      )}

      <ConfirmDialog
        open={othersConfirmOpen}
        title={t('sessions.revokeOthers.confirmTitle')}
        message={t('sessions.revokeOthers.confirmMessage')}
        variant="danger"
        doubleConfirm
        confirmLabel={t('sessions.revokeOthers.confirmLabel')}
        confirmLoading={revokeOthersMutation.isPending}
        onCancel={() => setOthersConfirmOpen(false)}
        onConfirm={() => void handleRevokeOthers()}
      />
    </Box>
  )
}
