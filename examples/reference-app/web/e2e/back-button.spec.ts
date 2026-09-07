/**
 * That the browser's own navigation never signs a person out or throws
 * away work in progress.
 *
 * A person signed in, moved between the surfaces, and pressed Back --
 * the most reflexive action there is in a browser. Three properties
 * must hold, and this file gates each of them at the browser level:
 *
 *  1. Back moves the view (the fragment returns to the previous
 *     surface) and the person stays signed in: the frame and its
 *     sign-out control persist and the previous surface renders.
 *  2. A same-document move never reloads the document -- neither the
 *     app's own navigation (a click on a surface link) nor a bare JS
 *     hash assignment, the navigation a scripted or bookmark-driven
 *     visit makes. A reload is what discards this product's
 *     deliberately memory-only session (the access token lives in
 *     memory and the refresh token in a closure; neither is ever
 *     written to storage, because a token in localStorage is a token
 *     an XSS walks away with). The gates assert the properties that
 *     keep both halves intact -- the security decision and the
 *     reflex -- rather than picking one: gate two marks the live
 *     document before any move, so a reload of either kind is caught
 *     directly, and pins the sign-in through every move.
 *  3. Work in progress on a surface -- a half-typed patient note --
 *     survives a hash navigation to another surface and back. The
 *     page never reloaded, so the text must still be there when the
 *     clinician returns; losing a half-typed patient record is worse
 *     than anything a failed save can do (the offline case's
 *     guarantee, kept here for the navigation case too).
 *
 * HOW THE FINDING CLOSED -- AND NOT BY THE ROUND THAT WAS AIMED AT IT
 *
 * The acceptance finding behind gates one and two reported a full page
 * load on Back -- "the content came up blank, and they were signed
 * out". Gates one and two were verified passing against the deployment
 * on all three engines (chromium, webkit and the iPad project) while
 * the round written for hash navigation and session survival had not
 * landed at all, which made the closing an attribution lesson rather
 * than a quiet one: the likeliest cause was the web-host UI round's
 * config render guard. The AppBar's brand name read a Public-config
 * answer without null guards and threw a TypeError mid-render, which
 * white-screens the page and takes the in-memory session down with the
 * unmount -- a symptom attributed to one layer was produced by
 * another, so the fix that closed it was not the fix anyone planned,
 * and nothing about it needed the router to be at fault.
 *
 * The hash-navigation round then landed, and its subject is gates two
 * and three here. The round's own acceptance drive could not
 * reproduce a full page load on any shape it ran -- dev server,
 * Go-served dist or live deployment alike: hash routing makes every
 * one of these moves a same-document event, zero document requests --
 * but a reload remains the one thing that would discard the in-memory
 * session, which is exactly why gate two asserts its absence directly
 * rather than relying on the diagnosis, and why it now covers the
 * bare JS hash assignment alongside the app's own navigation. Gate
 * three is the round's own property: a half-typed note lives in a
 * view-level store (src/views/notes-draft.ts) that survives the
 * unmount a hash navigation causes, so a trip away and back comes
 * home to the text the clinician left.
 *
 * Every gate signs in once, and the file is tagged @budget as well as
 * @deployment. @budget means VERIFIED PASSING but out of the default
 * local run, whose sign-ins already sit at the edge of the server's
 * per-account login budget (five per account per minute, go/authn's
 * ratelimit.go -- every attempt counts, successful or refused), and
 * a file's worth more would tip a neighbouring gate into 429 noise;
 * a separate invocation boots its own server with the counters at
 * zero, which is what pnpm test:e2e:budget is (see e2e/README.md).
 * @deployment is the one-sign-in-per-test shape the deployment run
 * asks for -- in both places the property under test, a served,
 * hash-routed page whose own navigation never signs a person out or
 * loses their half-typed work, is exactly the property that only
 * exists where the real page runs.
 */
import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'
import { DEMO_OWNER, DEMO_READER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  SESSION_TEXT,
  expectOnSurface,
  openSurface,
  signInAs,
} from './test-utils/journeys.js'

/** The frame's sign-out control, pinned to the exact name: on the
 * account surface the substring 'Sign out' also matches the section's
 * 'Sign out other devices' button and each session row's
 * 'Sign out this session:' icon button. */
function signOutButton(page: Page): ReturnType<Page['getByRole']> {
  return page.getByRole('button', { name: SESSION_TEXT.signOut, exact: true })
}

/** Marks the live document and returns whether the marker survived --
 * the direct way to catch a reload, which is exactly what discards
 * the in-memory session. */
async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    ;(window as unknown as Record<string, unknown>).__navDocMarker = 'alive'
  })
}

async function documentMarked(page: Page): Promise<string | undefined> {
  return page.evaluate(
    () =>
      (window as unknown as Record<string, unknown>).__navDocMarker as
        | string
        | undefined,
  )
}

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
      signOutButton(page),
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
  'a same-document move never reloads the page: neither the app\'s own navigation nor a bare JS hash change',
  { tag: ['@budget', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Mark the live document before any move. A full load -- the one
    // thing that can discard the memory-only session -- throws the
    // marker away, so the app's own link below and the bare hash
    // assignments after it must all keep it alive.
    await markDocument(page)

    // The app's own navigation first: a click on a surface link, the
    // move every real visit makes (openSurface walks the drawer below
    // the md breakpoint, so this gate holds on the iPad project too).
    await openSurface(page, APP_TEXT.navNotes)
    await expectOnSurface(page, APP_TEXT.notesHeading)
    await expect(signOutButton(page)).toBeVisible()
    expect(
      await documentMarked(page),
      'the app\'s own navigation reloaded the document, which is what throws the in-memory session away',
    ).toBe('alive')

    // The control-experiment move: a plain JS hash assignment, no
    // history involved.
    await page.evaluate(() => {
      window.location.hash = '/account'
    })
    await expectOnSurface(page, APP_TEXT.accountHeading)
    await expect(
      signOutButton(page),
      'a plain hash change signed the person out',
    ).toBeVisible()
    expect(
      await documentMarked(page),
      'the hash change reloaded the document, which is what throws the in-memory session away',
    ).toBe('alive')

    // And back again by the same means: the notes surface, still
    // signed in, still the same document.
    await page.evaluate(() => {
      window.location.hash = '/notes'
    })
    await expectOnSurface(page, APP_TEXT.notesHeading)
    await expect(signOutButton(page)).toBeVisible()
    expect(
      await documentMarked(page),
      'the return hash change reloaded the document',
    ).toBe('alive')
  },
)

test(
  'work in progress survives a hash navigation to another surface and back',
  { tag: ['@budget', '@deployment'] },
  async ({ page }) => {
    await signInAs(page, DEMO_READER)

    const noteText = `Patient note half-typed before navigating ${Date.now()}`
    await openSurface(page, APP_TEXT.navNotes)
    await expectOnSurface(page, APP_TEXT.notesHeading)
    const input = page.getByRole('textbox', { name: APP_TEXT.notesTextLabel })
    await input.fill(noteText)

    // Leave the surface mid-note and come back -- the journey the
    // reported loss happened on. The page never reloaded, so the
    // half-typed text must still be in the field.
    await openSurface(page, APP_TEXT.navAccount)
    await expectOnSurface(page, APP_TEXT.accountHeading)
    await page.goBack()
    await expectOnSurface(page, APP_TEXT.notesHeading)
    await expect(
      input,
      'the half-typed note was lost on the way back from another surface',
    ).toHaveValue(noteText)
  },
)
