/**
 * useHashRoute -- the app's micro hash-navigation hook.
 *
 * The shell's navigation state is the URL fragment: the sign-in surface,
 * the app frame and the surfaces behind it each own a route of the form
 * "#/route/params". B1 deliberately ships no router dependency -- the
 * frame is still a placeholder, so the only thing navigation needs today
 * is the current fragment and a subscription to its changes, which this
 * hook provides against window alone. The formal router choice for the
 * full shell (which library, if any, once the frame and its surfaces
 * land) is a DEFERRED decision of this round, recorded in the round's
 * follow-up docs.
 *
 * The returned value is the fragment without its leading '#' ('' for the
 * bare page), re-read from window.location.hash on every hashchange
 * event, never cached. The hook is browser-only by contract: it reads
 * window at render time and subscribes in an effect.
 */

import { useEffect, useState } from 'react'

export function useHashRoute(): string {
  const [route, setRoute] = useState<string>(() =>
    window.location.hash.slice(1),
  )

  useEffect(() => {
    const onHashChange = (): void => {
      setRoute(window.location.hash.slice(1))
    }
    window.addEventListener('hashchange', onHashChange)
    return () => {
      window.removeEventListener('hashchange', onHashChange)
    }
  }, [])

  return route
}
