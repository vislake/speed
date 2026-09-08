/**
 * useHashRoute -- the app's micro hash-navigation hook.
 *
 * The shell's navigation state is the URL fragment: the sign-in surface,
 * the app frame and the surfaces behind it each own a route of the form
 * "#/route/params", and the app ships no router dependency -- a formal
 * routing library was never chosen, so navigation is the current
 * fragment plus a subscription to its changes, which this hook provides
 * against window alone. Hash moves are same-document by construction,
 * which is what keeps the memory-only session alive across them.
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
