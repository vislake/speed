/**
 * app-journey.test.tsx -- the app's user-story suites: the complete
 * owner day in one continuous session, driven end to end through the
 * composed AppView over the real-client rig and pinned request by
 * request, plus the member days of the accounts whose grants differ
 * (the reader's day and the read-denied day).
 *
 * Named for the behaviour it chronicles rather than for one source
 * file, because it crosses every layer the shell composes -- the
 * precedent the packages' session-journey suites set for journeys
 * that span a whole composed family. Where app.test.tsx pins the
 * AppView's routing contract (the fragment parser's degradation
 * rules, per-journey composition checks over the same rig), this
 * suite's first journey keeps one session alive across every surface
 * and pins the ordered network trace that session produces: every
 * method and path, every bearer (the access-1..access-5 numbering of
 * the rig's own token-issuing server), and every body. The binding
 * exchange itself is not re-driven here -- app.test.tsx's travel
 * journey already pins a composed held-open exchange against the
 * same account surface -- so this suite's account leg drives the
 * other account actions: a session revoked from the list, MFA set up
 * over the discover-by-acting step-up gate, and the replacement
 * recovery codes shown once.
 *
 * The journey's arc, in order: a fresh visitor registers over the
 * sign-in surface's register turn, and the created account's own
 * sign-in answers the membership refusal of a registered-but-unseeded
 * account -- the honest display of the server's 403 code text, not a
 * fabricated state (demo_users_test.go:297 pins the refusal for the
 * browser's no-tenant sign-in shape; :162/:233 are its named-tenant
 * siblings);
 * the owner signs in (access-1) and the day runs: notes read and
 * written under the token's tenant, the tenant switch away and back
 * proving per-tenant lists and the eviction discipline, the account
 * day (a session revoked, the first MFA setup answering its recovery
 * codes, a second setup discovering the active factor through the
 * 403 step-up gate -- a wrong step-up code answered with its field
 * text -- and the replacement codes shown once), sign-out into the
 * session-ended screen, and the re-login that lands back on the
 * account fragment. A bilingual leg closes the day, and the whole
 * trace pins with configGets === 1: one Public-config fetch served
 * the entire session.
 *
 * The second and third journeys script the member days of the demo's
 * other account shapes: the reader day (the rig's reader option --
 * its list served like any member's, its create refused with the
 * rbac write gate's 403 and the draft kept, the answer the Go suite
 * pins at demo_users_test.go:135-153 and server_test.go:335) and the
 * read-denied day (a list read answering the read gate's 403 -- the
 * gate fails closed to the no-permission empty state, no form and no
 * list surface below the heading).
 *
 * Built-in strings are asserted through the bundles they render from
 * -- the app's own fixtures and the packages' locale fixtures,
 * relative imports like app.test.tsx's -- never inline (the CJK scan
 * treats test files as English text, and inline copy would drift from
 * the resources). The scripted facts (the registered email, the note
 * text, the demo MFA constants) are ASCII by the same rule; the demo
 * server's exports are the journeys' source for the facts that have
 * one (identifiers, codes), so a rename in the fixture breaks the
 * suite instead of drifting silently.
 */

import { act, configure, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import accountUiZhCN from '../../../../web/packages/account-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiZhCN from '../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import {
  APP_PASSWORD,
  configGets,
  makeAppRig,
  navigateTo,
  rendered,
  signInWithPasswordUi,
} from './test-utils/app-harness.js'
import {
  DEMO_MFA_CONFIRM_CODE,
  DEMO_MFA_PROVISIONING_URI,
  DEMO_MFA_RECOVERY_CODES,
  DEMO_MFA_REPLACEMENT_RECOVERY_CODES,
  DEMO_MFA_SECRET,
  DEMO_OWNER_IDENTIFIER,
  DEMO_READER_IDENTIFIER,
} from './test-utils/demo-server.js'
import type { RealCall, RealClientRig } from './test-utils/real-client.js'

/** The identifier the register turn creates -- an account the demo
 * seed granted no membership, whose own sign-in the day then answers
 * honestly. */
const REGISTER_EMAIL = 'journey-visitor@example.test'

/** The note the owner day writes in tenant-acme; its text is served
 * data like everything inline in a suite (ASCII by the CJK rule). */
const NOTE_TEXT = 'A journey note written in tenant-acme'

/** The draft a reader's refused create leaves behind. */
const READER_NOTE_TEXT = 'A note the reader cannot create'

/** A refused sign-in's identifier input holds its failed value; the
 * owner's sign-in follows it, so the journeys clear the field first. */
function loginFields(view: ReturnType<typeof rendered>) {
  return {
    identifier: view.getByLabelText(
      authUiZhCN.passwordSignIn.identifierLabel,
    ) as HTMLInputElement,
    password: view.getByLabelText(
      authUiZhCN.passwordSignIn.passwordLabel,
    ) as HTMLInputElement,
  }
}

function bodyOf(call: RealCall): Record<string, string> {
  return JSON.parse(call.body) as Record<string, string>
}

/** The journey's own indexed access -- the pin sections read calls by
 * position after the length wait, so a missing call is a suite bug,
 * not a possibly-undefined to paper over. */
function callOf(rig: RealClientRig, index: number): RealCall {
  const call = rig.calls[index]
  if (call === undefined) {
    throw new Error(`expected a recorded call at index ${index}`)
  }
  return call
}

/** The revoke control of a session row, named from the account-ui
 * aria template over the row's device. */
function revokeOf(device: string): string {
  return accountUiZhCN.sessions.revokeAriaWithDevice.replace(
    '{{device}}',
    device,
  )
}

describe('the app journey', () => {
  // These journeys drive dozens of real userEvent interactions and
  // network round-trips end to end -- on a warm dev machine the whole
  // file finishes in a few seconds, but a real GitHub Actions run
  // (2026-09-05, pr-check run 2) measured a cold import alone at ~14s
  // against ~0.8s locally, and the owner-day journey's own test then
  // tripped vitest's 5000ms default per-test timeout.
  //
  // That first timeout does not stay contained to its own test. Vitest
  // has no way to cancel a running async test body -- it only races the
  // test's promise against a timer and reports whichever settles first
  // -- so the owner-day test's own userEvent/mutateAsync continuation
  // keeps executing, unobserved, after vitest has already moved on to
  // the next test. Reproduced directly (a throwaway forced-timeout
  // build of this file): the abandoned continuation's console output
  // lands under the *next* test's name in the reporter, and its
  // continued DOM queries and network calls measurably starve the next
  // test's own event-loop turns badly enough to blow its own timeout
  // too -- because jsdom's `document` is one shared instance per test
  // *file*, not per test, and Testing Library's render queries are
  // bound to `document.body` by default rather than to the render's own
  // container, an abandoned test's stale query handle keeps resolving
  // against whatever the next test currently has on screen. That is
  // the honest account of the read-only-member journey's own failure in
  // the same CI run: its DOM at the moment `findByRole('alert')` gave
  // up already carried a *real*, served note row (the reader's own
  // typed text under the server's real creation timestamp) with no
  // Alert in sight -- not a slow assertion, but exactly the signature
  // this cross-test interference produces. `notes-view.tsx` and the
  // demo server's write gate were re-audited over this: both are plain,
  // deterministic, synchronous code with no path for a reader's create
  // to succeed, and every scripted rbac refusal here is a real 403 the
  // real notes handler's rbac gate produces (server_test.go:335) --
  // there is no application-level bug for this failure to be hiding.
  //
  // The fix stays the one applied here (raising both budgets, file-
  // scoped so neither call reaches another suite) because it removes
  // the one precondition this whole cascade depends on: a test in this
  // file actually exceeding its timeout. At ~14x the measured CI
  // slowdown, 30s leaves the owner-day journey (a couple of seconds
  // locally) generous headroom, so no test here should come close to
  // needing it -- if one ever does, that is worth treating as its own
  // incident rather than raising the budget again, since a second test
  // in this file paying for the first one's overrun is exactly the
  // failure mode above.
  beforeAll(() => {
    vi.setConfig({ testTimeout: 30_000 })
    configure({ asyncUtilTimeout: 5_000 })
  })

  beforeEach(() => {
    window.location.hash = ''
  })

  it('runs the whole owner day in one session, every request pinned', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()

    // A fresh visitor: the sign-in surface over the one config fetch.
    await view.findByRole('heading', { level: 1 })
    expect(configGets(rig)).toBe(1)
    expect(view.queryByRole('link', { name: zhCN.nav.home })).not.toBeInTheDocument()

    // The register turn: a created account is a destination, never a
    // session flip -- the register panel reports the created account
    // and sends the visitor back to the sign-in surface.
    await user.click(
      view.getByRole('button', { name: zhCN.signIn.registerAction }),
    )
    await user.type(
      view.getByLabelText(authUiZhCN.register.identifierLabel),
      REGISTER_EMAIL,
    )
    await user.type(
      view.getByLabelText(authUiZhCN.register.passwordLabel),
      APP_PASSWORD,
    )
    await user.click(
      view.getByRole('button', { name: authUiZhCN.register.submit }),
    )
    expect(await view.findByText(zhCN.register.success)).toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: zhCN.register.backToSignIn }),
    )
    await view.findByRole('button', { name: zhCN.signIn.registerAction })

    // The created account's own sign-in answers the membership
    // refusal of a registered-but-unseeded account, and the surface
    // renders that code text honestly -- no session, no frame.
    const fields = loginFields(view)
    await user.type(fields.identifier, REGISTER_EMAIL)
    await user.type(fields.password, APP_PASSWORD)
    await user.click(
      view.getByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
    )
    expect(
      await view.findByText(authUiZhCN.errors.authn.tenant_membership_required),
    ).toBeInTheDocument()
    expect(view.queryByRole('link', { name: zhCN.nav.home })).not.toBeInTheDocument()

    // The owner signs in and the day begins (access-1).
    await user.clear(fields.identifier)
    await user.clear(fields.password)
    await signInWithPasswordUi(view, user)
    expect(configGets(rig)).toBe(1)

    // Notes: the empty demo list first, then a created note read back
    // through the invalidated list query.
    navigateTo('#/notes')
    expect(await view.findByText(zhCN.notes.list.emptyTitle)).toBeInTheDocument()
    await user.type(view.getByLabelText(zhCN.notes.create.textLabel), NOTE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )
    expect(await view.findByText(NOTE_TEXT)).toBeInTheDocument()
    expect(view.getByText(zhCN.notes.createdColumn)).toBeInTheDocument()

    // Away to tenant-globex: its own empty list renders, the acme
    // rows nowhere in sight. Back to tenant-acme: the note is there
    // again, fetched fresh under the new token.
    await user.click(
      view.getByRole('button', { name: zhCN.tenants.acme }),
    )
    await user.click(
      await view.findByRole('menuitem', { name: zhCN.tenants.globex }),
    )
    expect(await view.findByText(zhCN.notes.list.emptyTitle)).toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: zhCN.tenants.globex }),
    )
    await user.click(
      await view.findByRole('menuitem', { name: zhCN.tenants.acme }),
    )
    expect(await view.findByText(NOTE_TEXT)).toBeInTheDocument()

    // The account day: the session list, the login history and the
    // bound identities all served, then one session revoked from the
    // list -- its row flips to revoked, the current session carries
    // no revoke control at all.
    navigateTo('#/account')
    await view.findByRole('heading', { name: zhCN.account.heading })
    for (const device of ['Windows desktop', 'iPad Safari']) {
      expect(await view.findByRole('button', { name: revokeOf(device) }))
        .toBeInTheDocument()
    }
    expect(
      view.queryByRole('button', { name: revokeOf('Demo laptop') }),
    ).not.toBeInTheDocument()
    expect(view.queryByText(accountUiZhCN.sessions.status.revoked))
      .not.toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: revokeOf('iPad Safari') }),
    )
    expect(
      await view.findByText(accountUiZhCN.sessions.status.revoked),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: revokeOf('iPad Safari') }),
    ).not.toBeInTheDocument()
    expect(
      view.getByRole('button', { name: revokeOf('Windows desktop') }),
    ).toBeInTheDocument()

    // The first MFA setup answers its secret and its show-once
    // recovery codes.
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.authenticator.enrollButton,
      }),
    )
    expect(await view.findByText(DEMO_MFA_SECRET)).toBeInTheDocument()
    expect(view.getByText(DEMO_MFA_PROVISIONING_URI)).toBeInTheDocument()
    await user.type(
      view.getByLabelText(accountUiZhCN.mfa.authenticator.codeLabel),
      DEMO_MFA_CONFIRM_CODE,
    )
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await view.findByText(accountUiZhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeInTheDocument()
    for (const code of DEMO_MFA_RECOVERY_CODES) {
      expect(view.getByText(code)).toBeInTheDocument()
    }
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.recoveryCodes.savedLabel,
      }),
    )
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.authenticator.enrollButton,
      }),
    ).toBeInTheDocument()

    // A second setup over the now-active factor discovers it through
    // the step-up gate's 403: the gate opens, a wrong code answers
    // with its field text, the right one elevates the token and the
    // retried setup confirms over the active factor, replacing it and
    // answering the fresh recovery set.
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.authenticator.enrollButton,
      }),
    )
    expect(
      await view.findByText(accountUiZhCN.mfa.stepUp.title),
    ).toBeInTheDocument()
    const codeField = view.getByLabelText(accountUiZhCN.mfa.stepUp.codeLabel)
    await user.type(codeField, '000000')
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.stepUp.confirmLabel,
      }),
    )
    expect(
      await view.findByText(accountUiZhCN.errors.authn.mfa_invalid_code),
    ).toBeInTheDocument()
    await user.clear(codeField)
    await user.type(codeField, DEMO_MFA_CONFIRM_CODE)
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.stepUp.confirmLabel,
      }),
    )
    expect(
      await view.findByText(accountUiZhCN.mfa.authenticator.replacingNotice),
    ).toBeInTheDocument()
    await user.type(
      view.getByLabelText(accountUiZhCN.mfa.authenticator.codeLabel),
      DEMO_MFA_CONFIRM_CODE,
    )
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await view.findByText(accountUiZhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeInTheDocument()
    for (const code of DEMO_MFA_REPLACEMENT_RECOVERY_CODES) {
      expect(view.getByText(code)).toBeInTheDocument()
    }
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.recoveryCodes.savedLabel,
      }),
    )
    await view.findByRole('button', {
      name: accountUiZhCN.mfa.authenticator.enrollButton,
    })

    // Sign-out converges to the session-ended screen and back to the
    // sign-in surface; the fragment stays where the day left it.
    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    expect(
      await view.findByText(authUiZhCN.sessionEnded.title),
    ).toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )
    await view.findByRole('button', { name: zhCN.signIn.registerAction })

    // The re-login lands back on the account fragment (access-5),
    // whose surface is served fresh again.
    await signInWithPasswordUi(view, user)
    await view.findByRole('heading', { name: zhCN.account.heading })
    expect(configGets(rig)).toBe(1)

    // The bilingual leg: the frame speaks the switched language, and
    // the notes list of the day still answers under the new token.
    await act(async () => {
      await switchLanguage(view.i18n, 'en-US')
    })
    await view.findByRole('link', { name: enUS.nav.home })
    expect(
      view.getByRole('heading', { name: enUS.account.heading }),
    ).toBeInTheDocument()
    navigateTo('#/notes')
    expect(
      await view.findByRole('button', { name: enUS.notes.create.submit }),
    ).toBeInTheDocument()
    expect(await view.findByText(NOTE_TEXT)).toBeInTheDocument()

    // The whole session, pinned: one config fetch served the day, and
    // every request landed in order with the bearer of the token that
    // was current when it left -- the session's own token timeline
    // (access-1 after the first sign-in, access-2/3 across the two
    // switches, access-4 after the step-up, access-5 after the
    // re-login), credentials never riding the register or the
    // sign-ins, and every body exactly as the surface sent it.
    await waitFor(() => expect(rig.calls).toHaveLength(29))
    expect(configGets(rig)).toBe(1)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}${call.query}`)
    expect(trace).toEqual([
      'GET /api/config/public',
      'POST /api/v1/authn/register',
      'POST /api/v1/authn/login/password',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/notes',
      'POST /api/v1/notes',
      'GET /api/v1/notes',
      'POST /api/v1/authn/tenant/switch',
      'GET /api/v1/notes',
      'POST /api/v1/authn/tenant/switch',
      'GET /api/v1/notes',
      'GET /api/v1/authn/sessions',
      'GET /api/v1/authn/login-history?limit=20',
      'GET /api/v1/authn/identities',
      'DELETE /api/v1/authn/sessions/session-3',
      'GET /api/v1/authn/sessions',
      'POST /api/v1/authn/mfa/totp/enroll',
      'POST /api/v1/authn/mfa/totp/confirm',
      'POST /api/v1/authn/mfa/totp/enroll',
      'POST /api/v1/authn/mfa/step-up',
      'POST /api/v1/authn/mfa/step-up',
      'POST /api/v1/authn/mfa/totp/enroll',
      'POST /api/v1/authn/mfa/totp/confirm',
      'POST /api/v1/authn/logout',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/authn/sessions',
      'GET /api/v1/authn/login-history?limit=20',
      'GET /api/v1/authn/identities',
      'GET /api/v1/notes',
    ])

    const authOf = (index: number): string | null =>
      callOf(rig, index).authorization
    expect(authOf(0)).toBeNull() // the pre-auth config fetch
    expect(authOf(1)).toBeNull() // register: a public route
    expect(authOf(2)).toBeNull() // the refused sign-in: still public
    expect(authOf(3)).toBeNull() // the owner sign-in: still public
    for (let index = 4; index <= 7; index += 1) {
      expect(authOf(index)).toBe('Bearer access-1')
    }
    for (let index = 8; index <= 9; index += 1) {
      expect(authOf(index)).toBe('Bearer access-2')
    }
    for (let index = 10; index <= 20; index += 1) {
      expect(authOf(index)).toBe('Bearer access-3')
    }
    for (let index = 21; index <= 23; index += 1) {
      expect(authOf(index)).toBe('Bearer access-4')
    }
    expect(authOf(24)).toBeNull() // the re-login: public again
    for (let index = 25; index <= 28; index += 1) {
      expect(authOf(index)).toBe('Bearer access-5')
    }

    expect(bodyOf(callOf(rig, 1))).toEqual({
      email: REGISTER_EMAIL,
      password: APP_PASSWORD,
      locale: 'zh-CN',
    })
    expect(bodyOf(callOf(rig, 2))).toEqual({
      identifier: REGISTER_EMAIL,
      password: APP_PASSWORD,
    })
    expect(bodyOf(callOf(rig, 3))).toEqual({
      identifier: DEMO_OWNER_IDENTIFIER,
      password: APP_PASSWORD,
    })
    expect(bodyOf(callOf(rig, 5))).toEqual({ text: NOTE_TEXT })
    expect(bodyOf(callOf(rig, 7))).toEqual({ tenant_id: 'tenant-globex' })
    expect(bodyOf(callOf(rig, 9))).toEqual({ tenant_id: 'tenant-acme' })
    expect(bodyOf(callOf(rig, 17))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 19))).toEqual({ code: '000000' })
    expect(bodyOf(callOf(rig, 20))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 22))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 24))).toEqual({
      identifier: DEMO_OWNER_IDENTIFIER,
      password: APP_PASSWORD,
    })
  })

  it('runs the read-only member day: a served list, a refused create with its code text', async () => {
    const rig = makeAppRig({ reader: true })
    const view = rendered(rig)
    const user = userEvent.setup()

    await signInWithPasswordUi(view, user, DEMO_READER_IDENTIFIER)
    expect(configGets(rig)).toBe(1)

    // A reader's list is served like any member's -- the gate opens
    // and the empty demo list stands in for the page.
    navigateTo('#/notes')
    expect(await view.findByText(zhCN.notes.list.emptyTitle)).toBeInTheDocument()
    const draft = view.getByLabelText(
      zhCN.notes.create.textLabel,
    ) as HTMLInputElement

    // A reader's create answers the write gate's 403: the code text
    // renders on the surface, the draft stays on the form, and
    // nothing joins the list.
    await user.type(draft, READER_NOTE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )
    expect(await view.findByRole('alert')).toHaveTextContent(
      zhCN.notes.errors.permissionDenied,
    )
    expect(view.getByLabelText(zhCN.notes.create.textLabel)).toHaveValue(
      READER_NOTE_TEXT,
    )
    // The empty list state still stands -- nothing joined the list
    // (the refused create never touched the list data).
    expect(view.getByText(zhCN.notes.list.emptyTitle)).toBeInTheDocument()

    await waitFor(() => expect(rig.calls).toHaveLength(4))
    expect(configGets(rig)).toBe(1)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}`)
    expect(trace).toEqual([
      'GET /api/config/public',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/notes',
      'POST /api/v1/notes',
    ])
    expect(callOf(rig, 1).authorization).toBeNull()
    expect(callOf(rig, 2).authorization).toBe('Bearer access-1')
    expect(callOf(rig, 3).authorization).toBe('Bearer access-1')
    expect(bodyOf(callOf(rig, 3))).toEqual({ text: READER_NOTE_TEXT })
  })

  it('answers a notes read refusal with the no-permission gate, no surface below the heading', async () => {
    const rig = makeAppRig({ denyNotesRead: true })
    const view = rendered(rig)
    const user = userEvent.setup()

    await signInWithPasswordUi(view, user)
    expect(configGets(rig)).toBe(1)

    navigateTo('#/notes')
    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    // Failed closed: no create form and no list surface below the
    // heading.
    expect(
      view.queryByLabelText(zhCN.notes.create.textLabel),
    ).not.toBeInTheDocument()
    expect(view.queryByText(zhCN.notes.list.emptyTitle)).not.toBeInTheDocument()

    await waitFor(() => expect(rig.calls).toHaveLength(3))
    expect(configGets(rig)).toBe(1)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}`)
    expect(trace).toEqual([
      'GET /api/config/public',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/notes',
    ])
    expect(callOf(rig, 2).authorization).toBe('Bearer access-1')
  })
})
