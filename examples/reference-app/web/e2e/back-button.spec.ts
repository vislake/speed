/**
 * That the browser's own back button does not sign a person out.
 *
 * Someone signed in, moved between the surfaces, and pressed Back -- the
 * most reflexive action there is in a browser. The address returned to
 * the previous surface, the content came up blank, and they were
 * signed out: no nav, no sign-out control, the sign-in form again. If
 * they had been part-way through a patient's note, it was gone -- which
 * is worse than the offline case, where the page never reloaded and the
 * text survived.
 *
 * The cause is not the session design. This app keeps its access token in
 * memory and its refresh token in a closure, writing neither to storage,
 * which is the right call -- a token in localStorage is a token an XSS
 * walks away with. The problem is that Back triggered a full page load at
 * all: `performance.getEntriesByType('navigation')[0].type` reported
 * "navigate" rather than a same-document hash change, and a full load is
 * what discards an in-memory session. Hash routing is supposed to make
 * Back a same-document event.
 *
 * So the gate below asserts the property that keeps both halves intact --
 * the security decision and the reflex -- rather than picking one:
 * pressing Back moves the view and leaves the person signed in. If it
 * ever becomes technically impossible to avoid the reload, that is a
 * product decision with a security cost attached, and it belongs to a
 * person rather than to this file.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { APP_TEXT, SESSION_TEXT, openSurface, signInAs } from './test-utils/journeys.js'

test(
  'pressing Back moves the view and keeps the person signed in',
  { tag: ['@pending', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Two moves, so there is somewhere to go back to.
    await openSurface(page, APP_TEXT.navNotes)
    await expect(page.getByRole('heading', { name: APP_TEXT.notesHeading })).toBeVisible()
    await openSurface(page, APP_TEXT.navAccount)
    await expect(page.getByRole('heading', { name: APP_TEXT.accountHeading })).toBeVisible()

    await page.goBack()

    // Still signed in. This is the half that broke: the frame vanished
    // and the sign-in form came back.
    await expect(
      page.getByRole('button', { name: SESSION_TEXT.signOut }),
      'pressing Back signed the person out, losing anything they were part-way through',
    ).toBeVisible()

    // And actually back on the previous surface, with its content --
    // not a blank main landmark under a restored address.
    await expect(
      page.getByRole('heading', { name: APP_TEXT.notesHeading }),
      'Back restored the address but not the surface behind it',
    ).toBeVisible()
  },
)

test(
  'a same-document move does not reload the page',
  { tag: ['@pending', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Mark the live document. A full load throws the marker away, which
    // is a more direct way to catch a reload than reading navigation
    // timings -- and it is exactly what discards the in-memory session.
    await page.evaluate(() => {
      ;(window as unknown as { __e2eDocumentMarker?: string }).__e2eDocumentMarker = 'alive'
    })

    await openSurface(page, APP_TEXT.navNotes)
    await expect(page.getByRole('heading', { name: APP_TEXT.notesHeading })).toBeVisible()

    const survived = await page.evaluate(
      () => (window as unknown as { __e2eDocumentMarker?: string }).__e2eDocumentMarker,
    )
    expect(
      survived,
      'moving between surfaces reloaded the document, which is what throws the in-memory session away',
    ).toBe('alive')
  },
)
