/**
 * SessionsSection: the sessions-and-devices surface of the account page.
 *
 * Renders every session the authn module holds for the signed-in account,
 * the session the request's own token belongs to marked as the current
 * one, through the generated list hook. Rows show what the server
 * answers, shaped for the eye that has to tell them apart: a row names
 * the client-supplied device string when the sign-in carried one, else
 * the readable browser/OS summary its user agent parses to ("Chrome ·
 * macOS" -- never the raw UA string, which would make two sign-ins from
 * one browser read as two identical, truncated walls of text), else the
 * unknown-device label; the IP and AMR values render raw (AMR tokens are
 * opaque authentication-method references -- server vocabulary, not text
 * to translate -- so they render as-is in chips), created/last-seen
 * times, and a status badge telling an active session from a revoked or
 * an expired one. Expiry is checked at use time and never written back
 * to the row, so the server answers only active/revoked -- a row whose
 * stored expires_at lies in the past is a dead session, rendered
 * expired (greyed, with no revoke affordance) rather than a live device
 * that stays individually revocable and arms "sign out other devices"
 * forever.
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
 * An unresolved load -- the first load in flight, or parked by
 * react-query's default networkMode 'online' while the device is offline
 * -- keeps the loading branch, header included. Only a settled query
 * leaves it: an answer listing zero sessions hides the header and
 * renders the ui-kit EmptyState empty variant, while a load that failed
 * -- and a successful answer that omits the optional sessions key, as
 * unreadable as an error and never grounds for a "no sessions" claim --
 * render the error variant with a retry button. In every settled state
 * the EmptyState title stands in for the hidden h2 header at its own
 * level, so the heading order never skips a level and the no-sessions
 * text is only ever asserted for an answer that genuinely listed none.
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
import type { SxProps, Theme } from '@mui/material'
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
import { summarizeUserAgent } from './internal/ua-summary.js'

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

/** Whether the row's stored expiry has passed. Server-side expiry is
 * checked at use time and never written back to the row, so an active
 * session whose expires_at lies in the past is a dead session, never a
 * live device. A row without expires_at (an answer that does not carry
 * the field) is never declared expired. */
function isExpired(expiresAtIso: string | undefined, now = Date.now()): boolean {
  const expiresAt = parseDate(expiresAtIso)
  return expiresAt !== null && expiresAt.getTime() <= now
}

/** The visually-hidden recipe for the success live region while there is
 * nothing to announce (the clip technique, the family's shape -- see
 * product-shell's sr-only region and ui-kit's ConfirmDialog arming
 * region): the region must stay in the accessibility tree to announce,
 * so it is clipped, never display:none -- a hidden region would not be
 * live. */
const srOnlyRegionSx: SxProps<Theme> = {
  clip: 'rect(0 0 0 0)',
  clipPath: 'inset(50%)',
  height: 1,
  margin: 0,
  overflow: 'hidden',
  position: 'absolute',
  whiteSpace: 'nowrap',
  width: 1,
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
  const { data, isPending, refetch } = useAuthnListSessions()
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
  // The answer is unresolved whenever there is no data yet: the first
  // load in flight, or parked by react-query's default networkMode
  // 'online' while the device is offline -- such a fetch sits at
  // fetchStatus 'paused', where isFetching (and isLoading, its
  // isPending-and-isFetching conjunction) is false and isError stays
  // false too, so a pending test derived from isLoading would miss
  // every branch and read an unresolved load as an empty answer.
  // isPending alone tracks "no answer yet" across the in-flight and the
  // parked states.
  const pending = isPending && sessions === undefined
  const hasSessions = sessions !== undefined && sessions.length > 0
  const busy =
    revokeSessionMutation.isPending || revokeOthersMutation.isPending

  // An expired session is as dead as a revoked one: it never arms the
  // section-top bulk action, which exists to sign out live devices.
  const hasRevocableOther =
    sessions !== undefined &&
    sessions.some(
      (session) =>
        session.id != null &&
        session.is_current !== true &&
        session.status !== 'revoked' &&
        !isExpired(session.expires_at),
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
      ) : sessions === undefined ? (
        // This guard tests the absent list field with the loading
        // branch already excluded above: pending is false here, so no
        // sessions means the query settled without delivering a list.
        // Two shapes settle that way: a load that failed with no data
        // (isError), and a successful answer whose body omits the
        // optional sessions key -- AuthnListSessionsResponse marks
        // `.sessions` optional, so a type-legal 200 `{}` carries data
        // yet no list, and nothing would re-arm the loading branch for
        // it (isPending is false and stays false). Both land on the
        // error state, never the loading skeleton, which nothing could
        // resolve: the error copy claims no account content, where
        // reading the field-less answer as "no sessions" would fabricate
        // a statement the answer never made, and its Retry is the exit.
        <EmptyState
          variant="error"
          title={t('sessions.error.title')}
          description={t('sessions.error.description')}
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
            <Button onClick={() => void refetch()}>{t('sessions.retry')}</Button>
          }
          // showHeader is false here (see its definition above): this
          // EmptyState's title stands in for the hidden h2 section
          // header, so it must render at that same level or the page's
          // heading order skips straight from h1 to h6.
          headingLevel="h2"
        />
      ) : sessions.length === 0 ? (
        <EmptyState
          variant="empty"
          title={t('sessions.empty.title')}
          description={t('sessions.empty.description')}
          headingLevel="h2"
        />
      ) : (
        <Box>
          {notice?.kind === 'revoke-failed' && (
            <InlineError code={notice.code} />
          )}
          {/* The revoke-others success live region. It must never mount
              together with its text: a role="status" region announces
              only content changes that follow its own insertion, so a
              success notice born in the same commit as the region would
              be silent. The region therefore stands here for the whole
              list phase -- mounted empty and visually silent (clipped,
              never display:none) while there is nothing to announce --
              and the notice commit fills the text into a region the
              screen reader already knows, which is what makes the
              announcement fire. The clip and the notice never coexist:
              the same node shows the full success banner exactly while
              it carries the text. */}
          <Alert
            severity="success"
            role="status"
            sx={
              notice?.kind === 'others-done'
                ? { width: '100%', mb: 1.5 }
                : srOnlyRegionSx
            }
          >
            {notice?.kind === 'others-done'
              ? t('sessions.revokeOthers.done', { count: notice.count })
              : ''}
          </Alert>
          {/* The rows are one real list: a screen-reader user hears each
              session as one item of a numbered set with a boundary
              between rows, never a flat div stack. The notices above are
              page-level messages and stay outside the list. role="list"
              keeps the list semantics under WebKit, which strips them
              from a list-style-none ul. */}
          <Box component="ul" role="list" sx={{ m: 0, p: 0, listStyle: 'none' }}>
            {sessions.map((session, index) => {
              const id = session.id ?? null
              const revoked = session.status === 'revoked'
              // The server answers only active/revoked (expiry is
              // checked at use time, never written back), so a row
              // whose stored expires_at lies in the past is a dead
              // session: render it expired, never as a live device.
              const expired = !revoked && isExpired(session.expires_at)
              const current = session.is_current === true
              const revocable = id !== null && !current && !revoked && !expired
              const dead = revoked || expired
              const metaColor = dead ? 'text.disabled' : 'text.secondary'
              // Line 1 carries the friendliest label the answer offers:
              // the client-named device string when the sign-in carried
              // one, else the readable summary the user agent parses to
              // ("Chrome · macOS", never the raw UA -- the raw string is
              // the defect this surface exists to spare its reader). The
              // summary repeats as a muted detail line only under a row
              // line 1 already names with a device string.
              const device =
                session.device != null && session.device !== ''
                  ? session.device
                  : null
              const agent =
                session.user_agent != null && session.user_agent !== ''
                  ? session.user_agent
                  : null
              const summary =
                agent !== null ? summarizeUserAgent(agent) : null
              const deviceLabel =
                device ?? summary ?? t('sessions.deviceUnknown')
              const agentLine =
                device !== null && summary !== null ? summary : null
              const created = parseDate(session.created_at)
              const lastSeen = parseDate(session.last_seen_at)
              const showLastSeen =
                lastSeen !== null &&
                session.last_seen_at !== session.created_at
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
                    <Typography
                      variant="body1"
                      noWrap
                      sx={{
                        minWidth: 0,
                        fontWeight: 500,
                        color: dead ? 'text.disabled' : 'text.primary',
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
                      {expired && (
                        <Chip
                          size="small"
                          variant="outlined"
                          label={t('sessions.status.expired')}
                          sx={{ color: 'text.disabled' }}
                        />
                      )}
                      {revocable && (
                        <IconButton
                          aria-label={t('sessions.revokeAriaWithDevice', {
                            device: deviceLabel,
                            // The label is an aria-label, not HTML: the
                            // device string must reach the accessibility
                            // tree verbatim (i18next's default value
                            // escaping would embed a literal `&#x2F;`
                            // for a free-text device label's slashes).
                            interpolation: { escapeValue: false },
                          })}
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
                          sx={dead ? { color: 'text.disabled' } : undefined}
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
