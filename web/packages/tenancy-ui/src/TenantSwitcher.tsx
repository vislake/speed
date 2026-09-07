/**
 * TenantSwitcher: the tenant-switch affordance over a session.
 *
 * A trigger button shows the current tenant (the host-supplied name of
 * the tenant whose id the host passes as currentTenantId, or the
 * noCurrentTenant text while the host has none -- data lag on first
 * render, or a pre-auth mount). Clicking it opens the host-supplied
 * tenant list; picking a row that is not the current tenant drives
 * session.switchTenant(id) and closes the menu immediately. The trigger
 * renders with `color="inherit"` by default, so its text follows the
 * ambient text color instead of defaulting to the primary palette
 * color: mounted inside a colored header (the common host placement,
 * next to the sign-out action in an AppBar whose background is the
 * primary color), an inherited color resolves to the surface's own
 * contrastText and stays legible where a primary-on-primary default
 * would vanish (reference-app acceptance: the trigger once measured
 * 1:1 against its own AppBar). On a plain surface the same inheritance
 * resolves to the surrounding text color, which is equally safe -- the
 * one thing the default never does is paint itself in the palette
 * color of the very surface it usually sits on.
 *
 * While the switch is in flight the trigger is inert and one
 * role="status" notice renders -- a live-region announcement, never a
 * blocking overlay, and the trigger label itself stays put so the
 * current tenant never visually flickers mid-switch. The notice names
 * where the switch is going (the switchingTo text) and, once the
 * switch commits, becomes the switchedTo confirmation naming the
 * tenant the session now runs under -- a context change that silently
 * alters which rows are on screen must announce itself, and it must do
 * so in a live region, because a person using a screen reader cannot
 * glance at the chrome to check (the acceptance gate for "switching
 * clinic says so"). The confirmation stays on the page until the next
 * switch begins (or the next attempt fails, when the failure's code
 * text replaces it) -- deliberately no auto-dismiss timer, so the
 * announcement's lifetime never races a reader's processing of it.
 * "Inert" is deliberately not
 * the native disabled attribute here: the menu closes onto the trigger
 * at the moment the flight starts, and MUI's focus trap restores focus
 * to the trigger on close -- a native-disabled control cannot take
 * focus in a real browser, so the restore would no-op and the round
 * trip would strand focus on document.body, leaving a failed switch
 * unreachable from where the keyboard user is. The trigger therefore
 * stays focusable while the flight is pending, inert in the accessible
 * way: aria-disabled, its open handler refusing while pending, and the
 * disabled look rendered from the theme tokens (see the Button below).
 * A successful switch fires
 * onSwitched exactly once per committed switch, after the commit
 * -- refetching, navigation, permission-list re-attachment and
 * previous-tenant query-cache cleanup are the host's to run then. A
 * throwing host callback is contained: it is not a switch failure (the
 * onSwitched contract) and never surfaces as one -- no error banner,
 * and no unhandled rejection from the fire-and-forget row handler.
 *
 * A switch that loses a commit race reconciles instead of vanishing.
 * When this instance's own switch request answers successfully but a
 * concurrent sibling operation on the same session -- another
 * TenantSwitcher instance's switch, most plausibly, since this
 * component's own entry guard already rules out a second concurrent
 * call through itself -- committed first, auth-core's switchTenant
 * rejects with OperationSupersededError rather than resolving: this
 * request did go through server-side, but the session it would have
 * described is not the one now current, so treating the answer as a
 * commit would fire onSwitched for a tenant the session is not actually
 * running under. The session row the server keeps, however, is a shared
 * fact both racing requests wrote: the server stores the current tenant
 * per session, and the authn refresh mints the next access token for
 * whatever request wrote it last -- normally this superseded one, since
 * a browser's requests arrive in order. A quiet swallow would therefore
 * leave the session able to DRIFT into this request's tenant on the
 * next silent refresh: the principal flips with no commit, no
 * onSwitched, no cache invalidation, and the tenant-domain permission
 * list silently dropped -- the host's caches and lists would describe
 * the tenant it was told about, not the one the session runs. The
 * superseded path therefore reconciles: it re-issues the same switch
 * request while the drift condition holds -- the winning snapshot still
 * shows the same user (the component's proxy for "the held token family
 * still reaches this request's server-side write": a logout or another
 * user's login replaced or cleared the family, so the write is
 * unreachable and the superseded call contributes nothing; a same-user
 * login is the one case the proxy cannot tell apart, and the re-issue
 * then simply lands the requested switch on the fresh session,
 * announced, if the membership still holds) and the session runs under
 * a tenant other than the one requested -- so the tenant the server
 * actually committed lands as a real, announced commit and no silent
 * drift can follow. The reconciliation is bounded: at most
 * MAX_SWITCH_ATTEMPTS
 * requests per user intent (see below), and a superseded call gives up
 * quietly when the bound is spent -- the residual drift window then
 * needs three or more concurrent commits to the same session inside one
 * flight, vanishing enough to record rather than engineer for. The
 * reconciliation's commit fires onSwitched for the requested tenant
 * exactly once, so in a two-instance race the host observes the
 * winner's commit and then the reconciling one, each truthful at the
 * moment it fired; the exactly-once contract holds per commit, not per
 * race. A failed switch leaves the state exactly as it was (the
 * auth-core contract: a raw ApiError rejection with zero state change)
 * and renders the answer's code text in one InlineError under the
 * control -- the whitelist of reachable codes (the membership and
 * account-status answers, the token-verification answers, the
 * session-lifecycle codes, the transport-level client.* codes) or
 * the unknown fallback, never a raw key -- with the trigger re-enabled
 * and the same row retryable on the next open. The flight is also guarded
 * at the entry of the switch handler itself: one switch at a time, with
 * the guard checked synchronously, because the inert trigger renders
 * only on the next frame and cannot stop a repeat activation of a row of
 * the still-closing list in the same window -- such an attempt is
 * refused, never queued, and never reaches the session.
 *
 * The component is controlled and fails closed: the session arrives as a
 * prop, tenants and the current tenant id are host data, and nothing
 * here consumes the auth-core hooks, reads storage, navigates or touches
 * the network directly. With no current tenant the trigger is disabled,
 * because there is nothing to switch from. The current-tenant row in the
 * list is disabled -- never switchable, never re-triggering -- and the
 * guard in the row handler keeps that invariant even if a synthetic or
 * assistive click reaches a disabled row.
 *
 * Hosts render this where the current tenant belongs (typically app
 * chrome next to the user menu) with the tenant list their membership
 * source provides, and treat onSwitched as the moment their own
 * tenant-namespaced caches must move to the new tenant: auth-core's
 * permission survival rules already dropped the tenant-domain list on
 * the switch commit, so hosts re-attach /me-derived lists afterwards
 * (see @speed/auth-core's session header).
 */

import { useId, useRef, useState } from 'react'
import Button from '@mui/material/Button'
import Box from '@mui/material/Box'
import Menu from '@mui/material/Menu'
import MenuItem from '@mui/material/MenuItem'
import Typography from '@mui/material/Typography'
import type { AuthSession } from '@speed/auth-core'
import { isOperationSuperseded } from '@speed/auth-core'
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { useTenancyUiTranslation } from './internal/translation.js'

/**
 * The most switch requests one user intent may issue: the original plus
 * the corrective re-issues a superseded outcome triggers (see the file
 * header). Each re-issue is a fresh session operation, so the bound is
 * what keeps a pathological pile-up of racing commits from looping this
 * component's requests forever; when the bound is spent the superseded
 * call gives up quietly.
 */
const MAX_SWITCH_ATTEMPTS = 3

/** One switchable tenant: the id the switch is called with, and the name
 * the trigger and the list show. */
export interface TenantOption {
  /** Stable tenant id, the value session.switchTenant is called with. */
  readonly id: string
  /** Display name for the trigger and the list rows. */
  readonly name: string
}

export interface TenantSwitcherProps {
  /** The session the switch operates on: every request this component
   * makes is a session operation through the seam the host bound. */
  readonly session: AuthSession
  /** The tenants the current principal may switch between, host data in
   * host order. The row whose id equals currentTenantId is the current
   * one and renders disabled. */
  readonly tenants: readonly TenantOption[]
  /** The id of the tenant the session currently operates in, or null
   * when the host has none yet (no current tenant: the trigger is
   * disabled and shows the noCurrentTenant text). */
  readonly currentTenantId: string | null
  /** Fired exactly once after a switch commits, with the id of the
   * tenant the session now operates in. The host refetches and cleans
   * its own state here; throwing from this callback is not a switch
   * failure and never renders an error, and the component contains it,
   * so it cannot escape as an unhandled rejection either. */
  readonly onSwitched?: (tenantId: string) => void
}

/** The one live-region notice a switch drives: its text changes from
 * the in-flight announcement to the committed one, so a screen reader
 * hears the destination and then the completion from a single region. */
interface SwitchNotice {
  /** The display name of the tenant the switch is going to / landed in. */
  readonly tenantName: string
  /** False while the flight is pending, true once the switch committed. */
  readonly committed: boolean
}

export function TenantSwitcher({
  session,
  tenants,
  currentTenantId,
  onSwitched,
}: TenantSwitcherProps) {
  const { t } = useTenancyUiTranslation()
  const menuId = useId()
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null)
  const [pending, setPending] = useState(false)
  const [errorCode, setErrorCode] = useState<string | null>(null)
  const [notice, setNotice] = useState<SwitchNotice | null>(null)

  const menuOpen = anchorEl !== null
  const currentTenant =
    tenants.find((tenant) => tenant.id === currentTenantId) ?? null
  // With no current tenant the trigger is native-disabled: there is
  // nothing to switch from, and a control that can never open a list
  // does not belong in the tab order. While a switch is in flight the
  // trigger is INERT instead -- aria-disabled, its open handler
  // refusing, the disabled look applied from the theme tokens below --
  // but never native-disabled, so it stays focusable: the menu closes
  // onto it at the moment the flight starts, MUI's focus trap restores
  // focus to the trigger on close, and a native-disabled control cannot
  // take focus in a real browser (the restore no-ops and the round trip
  // strands focus on document.body; see the file header).
  const triggerInert = pending
  const triggerNativeDisabled = currentTenant === null

  // The in-flight flag the entry guard checks. It is a ref, not state,
  // because the guard must be synchronous: pending renders only on the
  // next frame, and a repeat activation of a still-mounted closing-list
  // row can reach switchTo in the same window.
  const switching = useRef(false)

  const switchTo = async (tenant: TenantOption): Promise<void> => {
    // One switch at a time: a second attempt while a flight is pending
    // is refused before any await, never queued. The first flight's
    // commit wins and fires onSwitched for its tenant exactly once --
    // unless that commit is superseded and reconciled below.
    if (switching.current) {
      return
    }
    switching.current = true
    setErrorCode(null)
    setPending(true)
    // The notice announces the destination while the flight is pending
    // and the landed tenant once it commits (the file header explains
    // why a committed context change must say so out loud).
    setNotice({ tenantName: tenant.name, committed: false })
    // The principal this switch speaks for, captured before the first
    // attempt: a superseded outcome can only still reach the session
    // through a later refresh while the SAME user owns it. The user_id
    // is the proxy for "the held token family still reaches this
    // request's server-side write" (a logout or another user's login
    // replaced or cleared the family; see the file header for the one
    // case the proxy cannot tell apart).
    const issuedBy = session.getSnapshot().principal
    let committed = false
    try {
      // One request per attempt, staying pending across them. When the
      // attempt is superseded -- a sibling operation committed to the
      // session first -- this request's own server write may be the one
      // the session row now holds, so the outcome reconciles by
      // re-issuing the switch: the tenant the server actually committed
      // must land as a real, announced commit, or a later silent
      // refresh drifts the session into it behind the host's back (no
      // onSwitched, no cache invalidation, the tenant permission list
      // silently dropped).
      for (let attempt = 0; ; attempt += 1) {
        try {
          await session.switchTenant(tenant.id)
          committed = true
          break
        } catch (error) {
          if (!isOperationSuperseded(error)) {
            throw error
          }
          if (attempt + 1 >= MAX_SWITCH_ATTEMPTS) {
            // The reconciliation budget is spent: racing operations keep
            // owning the session. Contribute nothing -- no alert, no
            // onSwitched (the residual drift window needs three or more
            // concurrent commits; see the file header).
            break
          }
          const winner = error.snapshot.principal
          const canStillDrift =
            winner !== null &&
            issuedBy !== null &&
            winner.user_id === issuedBy.user_id &&
            winner.tenant_id !== tenant.id
          if (!canStillDrift) {
            // The session already runs under the requested tenant (the
            // race was redundant), or a different principal took it
            // over (the token family was replaced -- no drift can
            // follow). Contribute nothing.
            break
          }
          // The drift condition holds: re-issue the same request. The
          // flight stays pending; the loop breaks only on a commit.
        }
      }
    } catch (error) {
      // A genuine failure of the switch (the membership, account-status,
      // token-verification, session-lifecycle or transport answers): the
      // state is exactly as it was (the auth-core contract) and the
      // answer's code text renders below. The notice is cleared so the
      // failure's own text is the only message standing.
      setErrorCode(errorCodeOf(error))
    } finally {
      setPending(false)
      switching.current = false
    }
    if (!committed) {
      // The switch did not land (a genuine failure, a spent
      // reconciliation budget, or nothing left to reconcile): no
      // confirmation to announce, and on a failure the code text below
      // already replaced the notice.
      setNotice(null)
      return
    }
    // The commit: the confirmation announces the tenant the session now
    // runs under (see the file header for why the announcement must
    // survive the flight), and onSwitched fires exactly once, after the
    // in-flight state cleared, so the commit never surfaces as an error
    // through this component.
    setNotice({ tenantName: tenant.name, committed: true })
    try {
      onSwitched?.(tenant.id)
    } catch {
      // A throwing host callback is not a switch failure (onSwitched's
      // contract): the commit already happened and nothing here renders
      // the host's error. The containment also keeps that throw out of
      // this promise, which the row handler fires and forgets -- an
      // unhandled rejection would be the alternative.
    }
  }

  return (
    <Box>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <Button
          type="button"
          variant="outlined"
          // Inherit the ambient text color instead of defaulting to the
          // primary palette color: hosts mount this control inside a
          // colored AppBar (whose own text is its contrastText), where
          // a primary-colored default reads primary-on-primary (the
          // acceptance measurement that named the trigger at 1:1). On a
          // plain surface the inherited color is the surrounding text
          // color, equally legible -- the file header has the full
          // rationale.
          color="inherit"
          disabled={triggerNativeDisabled}
          aria-disabled={triggerInert || undefined}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          aria-controls={menuOpen ? menuId : undefined}
          onClick={(event) => {
            if (triggerInert) {
              // Inert while a switch is in flight: the list must not
              // open under the flight (the entry guard would refuse the
              // row anyway, but an inert control opens nothing).
              return
            }
            setErrorCode(null)
            setAnchorEl(event.currentTarget)
          }}
          sx={
            triggerInert
              ? {
                  // The disabled look of a control that must stay
                  // focusable: MUI's own outlined-disabled recipe
                  // (color + border from the theme tokens) -- the native
                  // disabled attribute would drop the trigger out of the
                  // tab order and break the menu-close focus restore.
                  color: 'action.disabled',
                  borderColor: 'action.disabledBackground',
                  cursor: 'default',
                }
              : undefined
          }
        >
          {currentTenant === null
            ? t('tenantSwitcher.noCurrentTenant')
            : currentTenant.name}
        </Button>
        {notice !== null ? (
          <Typography
            component="span"
            role="status"
            sx={{ color: 'inherit' }}
          >
            {notice.committed
              ? t('tenantSwitcher.switchedTo', { tenant: notice.tenantName })
              : t('tenantSwitcher.switchingTo', {
                  tenant: notice.tenantName,
                })}
          </Typography>
        ) : null}
      </Box>
      <InlineError code={errorCode} />
      <Menu
        id={menuId}
        anchorEl={anchorEl}
        open={menuOpen}
        onClose={() => setAnchorEl(null)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        transformOrigin={{ vertical: 'top', horizontal: 'left' }}
      >
        {tenants.map((tenant) => (
          <MenuItem
            key={tenant.id}
            disabled={tenant.id === currentTenantId}
            onClick={() => {
              // The current-tenant row is disabled above; this guard keeps
              // the never-re-trigger invariant even when a synthetic or
              // assistive click reaches the row.
              if (tenant.id === currentTenantId) {
                return
              }
              setAnchorEl(null)
              void switchTo(tenant)
            }}
          >
            {tenant.name}
          </MenuItem>
        ))}
      </Menu>
    </Box>
  )
}
