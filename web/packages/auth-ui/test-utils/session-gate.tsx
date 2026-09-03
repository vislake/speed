/**
 * session-gate.tsx -- the host-role session gate the journey tests
 * compose (usage-example.test.tsx, session-journey.test.tsx).
 *
 * The host's router in miniature, reading the snapshot of the session
 * attached with attachSession: while the snapshot is authenticated the
 * host's app content renders; an anonymous snapshot at a view that held
 * an authenticated one is the "session ended" state, and the
 * SessionEndedScreen placeholder takes the app's place until the viewer
 * asks to sign in again, which hands the host back to its sign-in
 * surface. Authentication itself is observed, not signalled: whichever
 * channel commits, the same snapshot transition flips this gate -- the
 * host does not thread onSignedIn through its own router.
 *
 * Lives in test-utils/ because it is a fixture of the composed host,
 * not package code; the journey tests mount their varying app content
 * and sign-in surfaces into it.
 */

import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useAuthState } from '@speed/auth-core'
import { SessionEndedScreen } from '../src/SessionEndedScreen.js'

export interface SessionGateProps {
  /** The host's authenticated view (its app content). */
  readonly app: ReactNode
  /** The host's sign-in surface, mounted while anonymous before the
   * first authentication and again after the viewer signs in again from
   * the session-ended screen. */
  readonly signIn: ReactNode
}

export function SessionGate({ app, signIn }: SessionGateProps) {
  const snapshot = useAuthState()
  // Remembering the app was reached tells an anonymous snapshot apart:
  // before the first authentication it is a plain sign-in view, after
  // one it is a session that ended.
  const [reachedApp, setReachedApp] = useState(false)
  useEffect(() => {
    if (snapshot.state === 'authenticated') {
      setReachedApp(true)
    }
  }, [snapshot.state])

  if (snapshot.state === 'authenticated') {
    return <>{app}</>
  }
  if (reachedApp) {
    return (
      <SessionEndedScreen onSignIn={() => setReachedApp(false)} />
    )
  }
  return <>{signIn}</>
}
