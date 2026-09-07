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
 *
 * CLOSED, AND NOT BY THE ROUND THAT WAS AIMED AT IT
 *
 * Both assertions pass against the deployment on all three engines
 * (chromium, webkit and the iPad project), verified in the acceptance
 * session after the web-host UI round landed -- while the round written
 * for hash navigation and session survival had not landed at all. The
 * likeliest cause is that round's config render guard: the AppBar's
 * brand name read a Public-config answer without null guards and threw a
 * TypeError mid-render, which white-screens the page and takes the
 * in-memory session down with the unmount. "The content came up blank,
 * and they were signed out" is that failure, described from the outside,
 * and nothing about it needed the router to be at fault.
 *
 * That is worth stating rather than quietly reclassifying: a symptom
 * attributed to one layer was produced by another, so the fix that
 * closed it is not the fix anyone planned, and the gate is what settled
 * it. The tag is @budget rather than @pending now -- verified, and out
 * of the default run only because its two owner sign-ins do not fit the
 * budget (see e2e/README.md).
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  SESSION_TEXT,
  expectOnSurface,
  openSurface,
  signInAs,
} from './test-utils/journeys.js'

test(
  'pressing Back moves the view and keeps the person signed in',
  { tag: ['@budget', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Two moves, so there is somewhere to go back to.
    await openSurface(page, APP_TEXT.navNotes)
    await expectOnSurface(page, APP_TEXT.notesHeading)
    await openSurface(page, APP_TEXT.navAccount)
    await expectOnSurface(page, APP_TEXT.accountHeading)

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
      page.getByRole('heading', { name: APP_TEXT.notesHeading, level: 1 }),
      'Back restored the address but not the surface behind it',
    ).toBeVisible()
  },
)

test(
  'a same-document move does not reload the page',
  { tag: ['@budget', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Mark the live document. A full load throws the marker away, which
    // is a more direct way to catch a reload than reading navigation
    // timings -- and it is exactly what discards the in-memory session.
    await page.evaluate(() => {
      ;(window as unknown as { __e2eDocumentMarker?: string }).__e2eDocumentMarker = 'alive'
    })

    await openSurface(page, APP_TEXT.navNotes)
    await expectOnSurface(page, APP_TEXT.notesHeading)

    const survived = await page.evaluate(
      () => (window as unknown as { __e2eDocumentMarker?: string }).__e2eDocumentMarker,
    )
    expect(
      survived,
      'moving between surfaces reloaded the document, which is what throws the in-memory session away',
    ).toBe('alive')
  },
)
