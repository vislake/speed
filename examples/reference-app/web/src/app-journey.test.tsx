/**
 * app-journey.test.tsx -- the app's user-story suites: the complete
 * owner day in one continuous session, driven end to end through the
 * composed AppView over the real-client rig and pinned request by
 * request, plus the member days of the accounts whose grants differ
 * (the reader's day and the read-denied day).
 *
 * Named for the behaviour it covers rather than for one source file,
 * because it crosses every layer the shell composes. Where
 * app.test.tsx pins the AppView's routing contract (the fragment
 * parser's degradation rules, per-journey composition checks over the
 * same rig), this suite's first journey keeps one session alive
 * across every surface
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
 * sign-in surface's register turn, and the owner signs in (access-1)
 * and the day runs: notes read and
 * written under the token's tenant, the tenant switch away and back
 * proving per-tenant lists and the eviction discipline, the account
 * day (a session revoked, the first MFA setup answering its recovery
 * codes, a second setup discovering the active factor through the
 * 403 step-up gate -- a wrong step-up code answered with its field
 * text -- and the replacement codes shown once), sign-out into the
 * session-ended screen, and the re-login that lands back on the
 * account fragment. A bilingual leg closes the day, and the closing
 * self-service leg brings the register-turn visitor back: its sign-in
 * lands inside the clinic its registration provisioned (the server's
 * self-service signup, internal/app/self_service.go) -- the empty notes
 * list of its own tenant, never the demo rows of the day and never a
 * membership-refusal. The whole trace pins with configGets === 3: the
 * initial Public-config fetch plus one revalidation per tenant switch
 * (a switch re-asks the host-resolved config the frame's brand and
 * the home cards render from).
 *
 * The second and third journeys script the member days of the demo's
 * other account shapes: the reader day (the rig's reader option --
 * its list served like any member's, its create refused with the
 * rbac write gate's 403 and the draft kept, the answer the Go suite
 * pins at demo_users_test.go:212-224
 * (TestDemoUsers_SeededAccountsReachTheGateThroughTheirPrincipal) and
 * flowtests/server_test.go:438-440) and the read-denied day (a list
 * read answering the read gate's 403 -- the
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
import { beforeAll, beforeEach, describe, expect, it } from 'vitest'
import type { NotesNote } from './app-api/index.js'
import { switchLanguage } from '@speed/i18n'
import accountUiZhCN from '../../../../web/packages/account-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiZhCN from '../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import layoutKitZhCN from '../../../../web/packages/layout-kit/src/locales/zh-CN.json' with { type: 'json' }
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
  FIRST_REGISTERED_CLINIC_TENANT_ID,
  REGISTERED_CLINIC_DEFAULT_NAME,
  demoServer,
} from './test-utils/demo-server.js'
import type { RealCall, RealClientRig } from './test-utils/real-client.js'
import { errorResponse, makeRealClientRig } from './test-utils/real-client.js'
import { watchSessionEnd } from '@speed/product-shell/bootstrap'

/** The identifier the register turn creates -- the account whose own
 * sign-in the day's closing self-service leg lands in its clinic (the
 * fixture provisions the registered account's own tenant, the web
 * mirror of the composed stack's self-service signup). */
const REGISTER_EMAIL = 'journey-visitor@example.test'

/** The note the owner day writes in tenant-acme; its text is served
 * data like everything inline in a suite (ASCII by the CJK rule). */
const NOTE_TEXT = 'A journey note written in tenant-acme'

/** The draft a reader's refused create leaves behind. */
const READER_NOTE_TEXT = 'A note the reader cannot create'

/** The note text a first account's read caches, whose leak into a
 * second, read-denied account's session the cross-account regression
 * below proves closed. */
const CACHED_NOTE_TEXT = 'A note only the first account should see'
const CACHED_NOTE: NotesNote = {
  id: 'note-1',
  text: CACHED_NOTE_TEXT,
  created_at: '2026-09-04T00:00:00Z',
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
  // network round-trips end to end, which can exceed vitest's 5000ms
  // default per-test timeout against CI's cold-start slowdown -- and a
  // timed-out test must not be allowed to happen at all: vitest cannot
  // cancel a running async test body, it only races the test's promise
  // against a timer, so an abandoned continuation keeps executing,
  // unobserved, while the next test runs. Because jsdom's `document`
  // is one shared instance per test *file*, not per test, and Testing
  // Library's render queries are bound to `document.body` by default,
  // the abandoned test's stale query handles keep resolving against
  // the next test's screen and its continued work starves the next
  // test's own event-loop turns -- a cascade where one overrun can
  // take down its neighbours.
  //
  // The 30s budget therefore has to apply at COLLECTION time, which is
  // when vitest freezes each collected test's effective timeout: a
  // runtime `vi.setConfig` inside a `beforeAll` lands too late to
  // change a timeout already frozen onto the task. The budget is
  // passed as a plain-number third argument to `describe` itself --
  // the supported form of vitest's `SuiteOptions.timeout` (the same
  // shorthand `it('name', fn, 30_000)` uses per test; the
  // object-options third-argument form is not part of the pinned
  // Vitest's describe signature) --
  // read while the suite is being built, before any test body or hook
  // runs, and applied to every test collected under it. 30s leaves the
  // owner-day journey generous headroom; a test that ever comes close
  // to it is its own incident rather than a reason to raise the budget
  // again, since a second test in this file paying for the first one's
  // overrun is exactly the cascade above.
  //
  // `asyncUtilTimeout` stays a `beforeAll`-set `configure()` call:
  // unlike vitest's own per-task timeout, Testing Library's config is
  // a plain runtime object that `findBy*`/`waitFor` read at the moment
  // they are called, which is always after `beforeAll` has run -- so
  // it never had the collection-time problem to begin with.
  beforeAll(() => {
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
    // and sends the visitor back to the sign-in surface. The visitor's
    // own sign-in is the day's closing leg (below): the account the
    // register turn created lands in the clinic its registration
    // provisioned.
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

    // The owner signs in and the day begins (access-1).
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
    // whose surface is served fresh again. The config count stands at
    // three: the first paint's fetch plus one revalidation per tenant
    // switch (the two switches earlier in the day).
    await signInWithPasswordUi(view, user)
    await view.findByRole('heading', { name: zhCN.account.heading })
    expect(configGets(rig)).toBe(3)

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

    // The closing self-service leg: back in the day's zh-CN language,
    // the owner signs out, and the register-turn visitor signs in.
    // Under the product's self-service shape, the visitor lands INSIDE
    // the clinic its registration provisioned -- the notes surface
    // under it serves the clinic's own empty list, the day's tenant-
    // acme rows are nowhere, and no membership-refusal code text
    // answers the sign-in.
    await act(async () => {
      await switchLanguage(view.i18n, 'zh-CN')
    })
    await view.findByRole('link', { name: zhCN.nav.home })
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

    // The visitor's own sign-in (the browser shape, no tenant named)
    // commits: the frame names the clinic tenant the registration
    // provisioned by the clinic's NAME (the fixture's
    // /api/reference-app/clinic-name mirror of internal/app/clinic_name.go
    // answers the registration's recorded name; this register turn typed
    // no display name, so the answer is the fixture's
    // REGISTERED_CLINIC_DEFAULT_NAME -- never the raw derived tenant
    // id, which a person must never be shown as their clinic's
    // identity), the notes surface under it answers the clinic's empty
    // list, and no membership-refusal text renders anywhere.
    await signInWithPasswordUi(view, user, REGISTER_EMAIL)
    expect(
      view.getByRole('button', { name: REGISTERED_CLINIC_DEFAULT_NAME }),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: FIRST_REGISTERED_CLINIC_TENANT_ID }),
    ).not.toBeInTheDocument()
    expect(await view.findByText(zhCN.notes.list.emptyTitle)).toBeInTheDocument()
    expect(view.queryByText(NOTE_TEXT)).not.toBeInTheDocument()
    expect(
      view.queryByText(authUiZhCN.errors.authn.tenant_membership_required),
    ).not.toBeInTheDocument()

    // The whole session, pinned: every request landed in order with
    // the bearer of the token that was current when it left -- the
    // session's own token timeline (access-1 after the owner's first
    // sign-in, access-2/3 across the two switches, access-4 after the
    // step-up, access-5 after the owner's re-login, access-6 after the
    // visitor's closing sign-in), credentials never riding the
    // register or the sign-ins, and every body exactly as the surface
    // sent it. The Public-config fetch appears three times: the
    // first-paint fetch (anonymous, hence bearer-less) plus one
    // revalidation after each completed tenant switch -- the switch
    // handler re-asks the host-resolved config. A revalidation rides
    // the access token current at the time (the client attaches the
    // store's token to every request; the pre-auth endpoint ignores
    // it). Each revalidation lands right behind the new tenant's first
    // notes read after its switch: the notes view's re-keyed query
    // refetches as the tenant change commits, the switch handler's
    // config refresh follows in the same turn.
    await waitFor(() => expect(rig.calls).toHaveLength(37))
    expect(configGets(rig)).toBe(3)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}${call.query}`)
    expect(trace).toEqual([
      'GET /api/v1/config/public',
      'POST /api/v1/authn/register',
      'POST /api/v1/authn/login/password',
      // The signed-in frame's one profile-preference read (the
      // language/timezone chain's profile tier, session-stable thanks
      // to the app's staleness window) lands right behind the sign-in.
      'GET /api/v1/authn/me/preferences',
      'GET /api/v1/notes',
      'POST /api/v1/notes',
      'GET /api/v1/notes',
      'POST /api/v1/authn/tenant/switch',
      'GET /api/v1/notes',
      'GET /api/v1/config/public',
      'POST /api/v1/authn/tenant/switch',
      'GET /api/v1/notes',
      'GET /api/v1/config/public',
      // The account page's first visit: the sections' preferences read
      // (the earlier one was evicted with the departing tenant's
      // ['tenant', tenantId] rows by the switch), then the list reads.
      'GET /api/v1/authn/me/preferences',
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
      'GET /api/v1/authn/me/preferences',
      'GET /api/v1/authn/sessions',
      'GET /api/v1/authn/login-history?limit=20',
      'GET /api/v1/authn/identities',
      'GET /api/v1/notes',
      'POST /api/v1/authn/logout',
      'POST /api/v1/authn/login/password',
      'GET /api/reference-app/clinic-name',
      'GET /api/v1/notes',
    ])

    const authOf = (index: number): string | null =>
      callOf(rig, index).authorization
    expect(authOf(0)).toBeNull() // the pre-auth config fetch
    expect(authOf(1)).toBeNull() // register: a public route
    expect(authOf(2)).toBeNull() // the owner sign-in: still public
    for (let index = 3; index <= 7; index += 1) {
      expect(authOf(index)).toBe('Bearer access-1')
    }
    for (let index = 8; index <= 10; index += 1) {
      expect(authOf(index)).toBe('Bearer access-2')
    }
    expect(authOf(9)).toBe('Bearer access-2') // the switch's revalidation
    for (let index = 11; index <= 23; index += 1) {
      expect(authOf(index)).toBe('Bearer access-3')
    }
    for (let index = 24; index <= 26; index += 1) {
      expect(authOf(index)).toBe('Bearer access-4')
    }
    expect(authOf(27)).toBeNull() // the re-login: public again
    for (let index = 28; index <= 33; index += 1) {
      expect(authOf(index)).toBe('Bearer access-5')
    }
    expect(authOf(34)).toBeNull() // the visitor's sign-in: public
    expect(authOf(35)).toBe('Bearer access-6') // the frame's clinic-name
    // fetch: the clinic the visitor's sign-in landed in is not on the
    // demo roster, so the menu asks the app's tenant-identity answer
    expect(authOf(36)).toBe('Bearer access-6') // the visitor's clinic
    // notes read

    expect(bodyOf(callOf(rig, 1))).toEqual({
      email: REGISTER_EMAIL,
      password: APP_PASSWORD,
      locale: 'zh-CN',
      // The registration form reports the browser's timezone beside
      // the language (the registration chain's first tier); the zone
      // itself is the test environment's own.
      timezone: expect.any(String),
    })
    expect(bodyOf(callOf(rig, 2))).toEqual({
      identifier: DEMO_OWNER_IDENTIFIER,
      password: APP_PASSWORD,
    })
    expect(bodyOf(callOf(rig, 5))).toEqual({ text: NOTE_TEXT })
    expect(bodyOf(callOf(rig, 7))).toEqual({ tenant_id: 'tenant-globex' })
    expect(bodyOf(callOf(rig, 10))).toEqual({ tenant_id: 'tenant-acme' })
    expect(bodyOf(callOf(rig, 20))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 22))).toEqual({ code: '000000' })
    expect(bodyOf(callOf(rig, 23))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 25))).toEqual({ code: DEMO_MFA_CONFIRM_CODE })
    expect(bodyOf(callOf(rig, 27))).toEqual({
      identifier: DEMO_OWNER_IDENTIFIER,
      password: APP_PASSWORD,
    })
    expect(bodyOf(callOf(rig, 34))).toEqual({
      identifier: REGISTER_EMAIL,
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

    await waitFor(() => expect(rig.calls).toHaveLength(5))
    expect(configGets(rig)).toBe(1)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}`)
    expect(trace).toEqual([
      'GET /api/v1/config/public',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/authn/me/preferences',
      'GET /api/v1/notes',
      'POST /api/v1/notes',
    ])
    expect(callOf(rig, 1).authorization).toBeNull()
    expect(callOf(rig, 2).authorization).toBe('Bearer access-1')
    expect(callOf(rig, 3).authorization).toBe('Bearer access-1')
    expect(bodyOf(callOf(rig, 4))).toEqual({ text: READER_NOTE_TEXT })
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

    await waitFor(() => expect(rig.calls).toHaveLength(4))
    expect(configGets(rig)).toBe(1)
    const trace = rig.calls.map((call) => `${call.method} ${call.path}`)
    expect(trace).toEqual([
      'GET /api/v1/config/public',
      'POST /api/v1/authn/login/password',
      'GET /api/v1/authn/me/preferences',
      'GET /api/v1/notes',
    ])
    expect(callOf(rig, 2).authorization).toBe('Bearer access-1')
  })

  it('never shows a second, read-denied account the notes an earlier account cached in the same tenant (reference-app-web.md P1-1)', async () => {
    // The demo server's own denyNotesRead switch is global (it denies
    // every principal, the shape the previous test drives); this
    // regression needs exactly the opposite -- one principal's read
    // served, a later one's refused -- which the shared demo server
    // has no per-account knob for. The rig issues bearer tokens
    // deterministically (access-1 the first sign-in, access-2 the
    // second -- the same numbering the owner-day journey above pins
    // request by request), so the second signed-in account's read is
    // refused right here, by its own bearer, rather than by extending
    // the shared demo server for one test's shape.
    //
    // The second account's read is held open on a gate this test
    // releases by hand -- proving the cache-eviction half, not only
    // the gate-order half: without the session-end eviction, the
    // query's cache still holds the first account's row the instant
    // the second account's view remounts, and that row would render
    // for the whole time this gate stays held (data defined, isError
    // still false) -- a real, observable leak this test can catch
    // mid-flight, before the deferred 403 ever settles isError and
    // lets the gate-order alone paper over it.
    let releaseSecondRead: (() => void) | undefined
    const secondReadGate = new Promise<void>((resolve) => {
      releaseSecondRead = resolve
    })
    const server = demoServer({ initialNotes: [CACHED_NOTE] })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/notes') {
        if (call.authorization === 'Bearer access-2') {
          await secondReadGate
          return errorResponse(403, 'rbac.permission_denied')
        }
      }
      return server(call)
    })
    const view = rendered(rig)
    // The harness's rendered() -- like every suite here -- composes the
    // tree by hand rather than calling bootstrapReferenceApp itself (no
    // suite mounts a real DOM root; see main.tsx's own doc comment), so
    // the shipped session-end strategy is wired here explicitly,
    // exactly as the real assembly wires it, over the same rig session
    // and the same QueryClient this render uses.
    watchSessionEnd(rig.session, view.queryClient)
    const user = userEvent.setup()

    // The first account signs in and reads the tenant's notes -- the
    // read that populates the shared QueryClient's cache under this
    // tenant's namespaced key.
    await signInWithPasswordUi(view, user)
    navigateTo('#/notes')
    expect(await view.findByText(CACHED_NOTE_TEXT)).toBeInTheDocument()

    // It signs out: the session-ended screen, then back to sign-in --
    // the same authenticated -> anonymous transition a session death
    // produces (the session-end strategy fires on either, emptying the
    // whole cache).
    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    await view.findByText(authUiZhCN.sessionEnded.title)
    await user.click(
      view.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )

    // A second account signs into the SAME tenant, lacking notes:read.
    // The hash is still '#/notes' from before (sign-out never changes
    // it), so the frame renders the notes surface again the moment
    // this sign-in commits, with no extra navigation -- and its read
    // is held open on the gate above.
    await signInWithPasswordUi(view, user, 'demo-no-notes-read@example.test')

    // While the second account's read is still in flight, the cached
    // row must already be gone and the guard's pending spinner must
    // stand in its place -- proof the tenant's cache was evicted at
    // sign-out rather than surviving to be raced against the refusal.
    expect(
      await view.findByRole('progressbar', {
        name: layoutKitZhCN.routeGuard.pending,
      }),
    ).toBeInTheDocument()
    expect(view.queryByText(CACHED_NOTE_TEXT)).not.toBeInTheDocument()

    // Releasing the gate lets the refusal land: the guard converges to
    // denied, never back to the first account's row.
    await act(async () => {
      releaseSecondRead?.()
    })
    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(view.queryByText(CACHED_NOTE_TEXT)).not.toBeInTheDocument()
  })

  it('never shows a second account the identity-domain rows an earlier account cached in the same tenant (reference-app-web.md P1-apisdk-1)', async () => {
    // The first account visits the account surface -- the reads that
    // populate the identity-domain queries: the sessions list, the
    // login history and the bound identities, all under bare spec-path
    // keys that no ['tenant', tenantId] eviction can reach (nothing in
    // @speed/api-sdk is tenant-namespaced). It signs out; the session-
    // end eviction must empty those keys too, so the second account's
    // account surface answers only what its own token can fetch. The
    // second account's reads are refused, per its own bearer, which
    // makes the leak deterministic in the final state: an un-evicted
    // cache would keep rendering the first account's rows on top of
    // the refusals (react-query keeps `data` through a failed
    // refetch), while the eviction lets the refusals surface as each
    // section's own error state -- the same shape the notes cross-
    // account regression above pins for the tenant domain.
    const server = demoServer()
    const rig = makeRealClientRig((call) => {
      if (
        call.authorization === 'Bearer access-2' &&
        (call.path === '/api/v1/authn/sessions' ||
          call.path === '/api/v1/authn/login-history' ||
          call.path === '/api/v1/authn/identities')
      ) {
        return errorResponse(403, 'authn.session_not_found')
      }
      return server(call)
    })
    const view = rendered(rig)
    // The shipped session-end strategy, wired over this render's
    // session and QueryClient exactly as the real assembly wires it
    // (rendered() composes the tree by hand; see the notes
    // cross-account regression above).
    watchSessionEnd(rig.session, view.queryClient)
    const user = userEvent.setup()

    // The first account signs in and reads the account surface: the
    // session rows of the day render from the demo's served state.
    await signInWithPasswordUi(view, user)
    navigateTo('#/account')
    await view.findByRole('heading', { name: zhCN.account.heading })
    expect(await view.findByText('Demo laptop')).toBeInTheDocument()
    expect(view.getByText('iPad Safari')).toBeInTheDocument()

    // It signs out: the session-ended screen, then back to sign-in --
    // the session-end transition whose eviction must clear the
    // identity-domain rows this account's reads just cached.
    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    await view.findByText(authUiZhCN.sessionEnded.title)
    await user.click(
      view.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )

    // A second account signs into the SAME tenant. The hash is still
    // '#/account' from before (sign-out never changes it), so the
    // frame renders the account surface again the moment this sign-in
    // commits -- and every one of its reads is refused, per its own
    // bearer, by the gate above.
    await signInWithPasswordUi(view, user, 'second-account@example.test')

    // The refusals surface as each section's own error state -- the
    // sessions list, the login history and the bound identities --
    // never as the first account's cached rows and never as the
    // cached empty binding list the earlier session left behind.
    expect(
      await view.findByText(accountUiZhCN.sessions.error.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(accountUiZhCN.history.error.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(accountUiZhCN.bindings.error.title),
    ).toBeInTheDocument()
    for (const device of ['Demo laptop', 'Windows desktop', 'iPad Safari']) {
      expect(view.queryByText(device)).not.toBeInTheDocument()
    }
  })
}, 30_000)
