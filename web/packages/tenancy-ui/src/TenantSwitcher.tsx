/**
 * TenantSwitcher: the tenant-switch affordance over a session.
 *
 * A trigger button shows the current tenant (the host-supplied name of
 * the tenant whose id the host passes as currentTenantId, or the
 * noCurrentTenant text while the host has none -- data lag on first
 * render, or a pre-auth mount). Clicking it opens the host-supplied
 * tenant list; picking a row that is not the current tenant drives
 * session.switchTenant(id) and closes the menu immediately. While the
 * switch is in flight the trigger is disabled and a role="status" notice
 * renders the switching text -- a live-region announcement, never a
 * blocking overlay, and the trigger label itself stays put so the current
 * tenant never visually flickers mid-switch. A successful switch is
 * deliberately quiet: the session committed (the store holds the fresh
 * access token, the snapshot flipped), the host observes the principal
 * change through its own auth-core hooks, and onSwitched fires exactly
 * once, after the commit -- refetching, navigation, permission-list
 * re-attachment and previous-tenant query-cache cleanup are the host's
 * to run then. A throwing host callback is contained: it is not a switch
 * failure (the onSwitched contract) and never surfaces as one -- no
 * error banner, and no unhandled rejection from the fire-and-forget row
 * handler. A failed switch leaves the state exactly as it was (the
 * auth-core contract: a raw ApiError rejection with zero state change)
 * and renders the answer's code text in one InlineError under the
 * control -- the whitelist of reachable codes (authn.tenant_membership_required,
 * the session-lifecycle codes, the transport-level client.* codes) or
 * the unknown fallback, never a raw key -- with the trigger re-enabled
 * and the same row retryable on the next open. The flight is also guarded
 * at the entry of the switch handler itself: one switch at a time, with
 * the guard checked synchronously, because the disabled trigger renders
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
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { useTenancyUiTranslation } from './internal/translation.js'

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

  const menuOpen = anchorEl !== null
  const currentTenant =
    tenants.find((tenant) => tenant.id === currentTenantId) ?? null
  const triggerDisabled = pending || currentTenant === null

  // The in-flight flag the entry guard checks. It is a ref, not state,
  // because the guard must be synchronous: pending renders only on the
  // next frame, and a repeat activation of a still-mounted closing-list
  // row can reach switchTo in the same window.
  const switching = useRef(false)

  const switchTo = async (tenantId: string): Promise<void> => {
    // One switch at a time: a second attempt while a flight is pending
    // is refused before any await, never queued. The first flight's
    // commit wins and fires onSwitched exactly once.
    if (switching.current) {
      return
    }
    switching.current = true
    setErrorCode(null)
    setPending(true)
    try {
      await session.switchTenant(tenantId)
      // Success is the host's to observe: the snapshot flipped to the new
      // tenant and onSwitched fires exactly once below, after the
      // in-flight state cleared, so the commit never surfaces as an
      // error through this component.
    } catch (error) {
      setErrorCode(errorCodeOf(error))
      setPending(false)
      switching.current = false
      return
    }
    setPending(false)
    switching.current = false
    try {
      onSwitched?.(tenantId)
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
          disabled={triggerDisabled}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          aria-controls={menuOpen ? menuId : undefined}
          onClick={(event) => {
            setErrorCode(null)
            setAnchorEl(event.currentTarget)
          }}
        >
          {currentTenant === null
            ? t('tenantSwitcher.noCurrentTenant')
            : currentTenant.name}
        </Button>
        {pending ? (
          <Typography component="span" role="status">
            {t('tenantSwitcher.switching')}
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
              void switchTo(tenant.id)
            }}
          >
            {tenant.name}
          </MenuItem>
        ))}
      </Menu>
    </Box>
  )
}
