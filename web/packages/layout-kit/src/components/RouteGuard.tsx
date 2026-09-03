/**
 * RouteGuard: gates its children on an injected authorization decision.
 *
 * `status` is a plain value the host computes and re-renders with --
 * never a callback RouteGuard invokes -- matching every other controlled
 * component at this architectural tier (`ConfirmDialog.open`,
 * `EmptyState.variant`). The three states (`'allowed' | 'denied' |
 * 'pending'`) are mutually exclusive by construction: there is no
 * separate boolean "loading" flag that could disagree with "allowed".
 *
 * RouteGuard takes no dependency on any concrete authentication or
 * routing package -- it has no idea what "authorization" means, only
 * that the host has already decided. A host wires this to whatever
 * mechanism it uses (a hook backed by a real auth package, a stubbed
 * `useState` before one exists, a static value in a test) by computing
 * `status` and passing it in; nothing here changes when that mechanism
 * changes.
 *
 * `deniedFallback` defaults to `@speed/ui-kit`'s own
 * `EmptyState variant="noPermission"` -- the one concrete coupling this
 * package takes, already sanctioned for chrome/primitives, never
 * anything auth-shaped. `pendingFallback` defaults to a centered MUI
 * `CircularProgress` labelled from this package's own namespace.
 *
 * `onDenied` is a side-effect escape hatch (a router redirect,
 * telemetry) fired from an effect keyed on `status`, so it runs exactly
 * once per transition INTO `denied` -- never on a re-render that leaves
 * `status` at `'denied'`, and never during render itself.
 */

import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import CircularProgress from '@mui/material/CircularProgress'
import { EmptyState } from '@speed/ui-kit'
import { useLayoutKitTranslation } from '../internal/translation.js'

export type RouteGuardStatus = 'allowed' | 'denied' | 'pending'

export interface RouteGuardProps {
  /** The host-computed authorization decision for this render. */
  readonly status: RouteGuardStatus
  /** Rendered only when `status` is `'allowed'`. */
  readonly children?: ReactNode
  /** Overrides the default `'pending'` placeholder (a labelled spinner). */
  readonly pendingFallback?: ReactNode
  /** Overrides the default `'denied'` placeholder (ui-kit's `noPermission` EmptyState). */
  readonly deniedFallback?: ReactNode
  /** Fires once per transition into `'denied'` -- never on every re-render. */
  readonly onDenied?: () => void
}

/**
 * Gate `children` on `status`: renders them only when `'allowed'`,
 * otherwise the matching fallback. See the module doc comment for the
 * full contract.
 */
export function RouteGuard({
  status,
  children,
  pendingFallback,
  deniedFallback,
  onDenied,
}: RouteGuardProps) {
  const { t } = useLayoutKitTranslation()

  // Fires onDenied exactly once per transition INTO 'denied': the guard
  // ref starts false, flips true the first render `status === 'denied'`
  // is seen, and resets the moment status leaves 'denied' -- so a later
  // denied spell (allowed -> denied again) fires onDenied again, but two
  // renders that both see 'denied' in a row do not.
  const firedForThisDenial = useRef(false)
  useEffect(() => {
    if (status === 'denied') {
      if (!firedForThisDenial.current) {
        firedForThisDenial.current = true
        onDenied?.()
      }
    } else {
      firedForThisDenial.current = false
    }
  }, [status, onDenied])

  if (status === 'allowed') {
    return <>{children}</>
  }

  if (status === 'pending') {
    return (
      pendingFallback ?? (
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'center',
            alignItems: 'center',
            paddingY: 6,
          }}
        >
          <CircularProgress aria-label={t('routeGuard.pending')} />
        </Box>
      )
    )
  }

  return <>{deniedFallback ?? <EmptyState variant="noPermission" />}</>
}
