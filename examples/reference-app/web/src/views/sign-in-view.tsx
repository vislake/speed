/**
 * sign-in-view.tsx -- the anonymous branch of the app: the brand
 * (server-served, like everywhere else on this page) over auth-ui's
 * SignInScreen, with the host's own register surface toggled from the
 * footer link.
 *
 * The screen offers the password channel alone. This deployment has no
 * way to deliver an SMS code: the server's SMS seam resolves to the
 * console sender (cmd/server's standalone wiring), so a code "sent to"
 * a phone never reaches one -- and this page is what a prospective
 * customer sees in a demo, not a developer's own machine where reading
 * the code in the server log is the point. Offering the SMS tab there
 * would invite a person down a path the deployment cannot finish and
 * then claim a delivery that never happened; the channel set is
 * declared here as channels={['password']}, and
 * e2e/offered-channels-work.spec.ts holds the choice.
 *
 * Registration is a destination, not a session operation: the created
 * account (auth-ui's RegisterForm hands it to the host through
 * onRegistered -- the form never signs in, by package contract) lands
 * in a success state with a way back to the sign-in surface, the
 * composition auth-ui documents for hosts that keep the two steps
 * apart.
 *
 * The view state is interaction-local: a completed registration does
 * not flip the auth-core snapshot (nothing here calls login), so
 * ProductShell stays on the sign-in branch and this view owns the
 * whole register -> back-to-sign-in turn.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import { Alert, Box, Button, Typography } from '@mui/material'
import { SignInScreen, RegisterForm } from '@speed/auth-ui'
import { useTranslation } from '@speed/i18n'
import { useAppServices, useBrandName } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

type SignInMode = 'sign-in' | 'register' | 'registered'

/** The anonymous-branch view ProductShell slots as its signIn node. */
export function SignInView(): ReactElement {
  const { session } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const brand = useBrandName()
  const [mode, setMode] = useState<SignInMode>('sign-in')

  const goBackToSignIn = (): void => {
    setMode('sign-in')
  }

  return (
    <Box
      sx={{
        width: 1,
        minHeight: '100vh',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        p: 3,
      }}
    >
      <Box sx={{ width: 1, maxWidth: 480 }}>
        <Typography
          component="h1"
          variant="h4"
          sx={{ fontWeight: 600, textAlign: 'center', mb: 3 }}
        >
          {brand}
        </Typography>
        {mode === 'sign-in' && (
          <>
            <SignInScreen session={session} channels={['password']} />
            <Box
              sx={{
                display: 'flex',
                justifyContent: 'center',
                alignItems: 'center',
                gap: 1,
                mt: 3,
              }}
            >
              <Typography variant="body2" sx={{ color: 'text.secondary' }}>
                {t('signIn.registerPrompt')}
              </Typography>
              <Button size="small" onClick={() => setMode('register')}>
                {t('signIn.registerAction')}
              </Button>
            </Box>
          </>
        )}
        {mode === 'register' && (
          <>
            <Typography variant="h6" sx={{ mb: 2 }}>
              {t('register.heading')}
            </Typography>
            <RegisterForm session={session} onRegistered={() => setMode('registered')} />
            <Box sx={{ display: 'flex', justifyContent: 'center', mt: 2 }}>
              <Button onClick={goBackToSignIn}>
                {t('register.backToSignIn')}
              </Button>
            </Box>
          </>
        )}
        {mode === 'registered' && (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
            <Alert severity="success" role="status" sx={{ width: 1 }}>
              {t('register.success')}
            </Alert>
            <Button onClick={goBackToSignIn} variant="contained">
              {t('register.backToSignIn')}
            </Button>
          </Box>
        )}
      </Box>
    </Box>
  )
}
