/**
 * AppView contract: the product-shell composition of this app -- the
 * hash-routed fragment machine over the three-branch view machine.
 *
 * The pure half of this suite pins parseHashFragment's degradation
 * rules: the three routes parse to their kinds, the binding subroute
 * parses to a binding target only for a demo provider carrying both
 * halves of the (code, state) pair (anything else on that prefix is
 * account content, never an exchange driven with garbage), and anything
 * the app does not know degrades to unknown -- home content with no nav
 * item selected. The journeys render AppView over a real client bound
 * into the runtime seam and drive it through what a user actually does:
 * a fresh visitor sees the sign-in surface over one config fetch (the
 * page's whole first paint is one GET /api/v1/config/public), a completed
 * sign-in flips the machine into the frame (header brand, nav carrying
 * host-computed aria-current, home over the served brand), navigation
 * travels home/notes/account through the location hash -- notes
 * answering with its served list (the empty demo list renders the
 * list's empty state), the account fragment rendering the account
 * surface over its served state, and the binding subroute completing
 * its exchange inside that surface: the host's answer to the handler's
 * onBound cue is navigation back to the account fragment, unmounting
 * the completion UI (the journey gates the exchange's answer so it can
 * rest on the pending notice before the cue). Unknown fragments
 * degrade to home with nothing selected, and a sign-out after the
 * frame converges to the session-ended screen and back to the sign-in
 * surface -- the session still anonymous, the config cache still one
 * fetch, and the shell's own polite announcement of that flip (its
 * product-shell namespace, registered at the host's bootstrap like
 * every sibling's) carrying the human session-ended text in the
 * status region, in the active language, never the raw key. A
 * bilingual leg proves the frame and the auth surface speak the
 * active language while the served brand stays verbatim.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures imported relatively, the auth-ui,
 * account-ui and product-shell copy through the packages' locale
 * fixtures (relative imports, the product-shell precedent) -- never
 * inline: the CJK scan treats test files as English text like
 * everything else, and inline copy would both violate that rule and
 * drift from the resources.
 */

import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import accountUiZhCN from '../../../../web/packages/account-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiEnUS from '../../../../web/packages/auth-ui/src/locales/en-US.json' with { type: 'json' }
import authUiZhCN from '../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import productShellEnUS from '../../../../web/packages/product-shell/src/locales/en-US.json' with { type: 'json' }
import productShellZhCN from '../../../../web/packages/product-shell/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import { parseHashFragment } from './app.js'
import {
  BRAND,
  configGets,
  makeAppRig,
  navigateTo,
  rendered,
  signInWithPasswordUi,
} from './test-utils/app-harness.js'
import { demoServer } from './test-utils/demo-server.js'
import {
  jsonResponse,
  makeRealClientRig,
} from './test-utils/real-client.js'
import uiKitZhCN from '../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import { SYSTEM_PSEUDO_TENANT_ID } from './demo-tenants.js'
import { FEATURE_SMILE_PREVIEW } from './views/home-view.js'
import { clearNotesDraft } from './views/notes-draft.js'

describe('parseHashFragment', () => {
  it('parses the app\'s routes, with or without a leading slash', () => {
    expect(parseHashFragment('')).toEqual({ kind: 'home' })
    expect(parseHashFragment('/')).toEqual({ kind: 'home' })
    expect(parseHashFragment('/cases')).toEqual({ kind: 'cases' })
    expect(parseHashFragment('/notes')).toEqual({ kind: 'notes' })
    expect(parseHashFragment('/team')).toEqual({ kind: 'team' })
    expect(parseHashFragment('/credits')).toEqual({ kind: 'credits' })
    expect(parseHashFragment('/account')).toEqual({ kind: 'account' })
    expect(parseHashFragment('/admin')).toEqual({ kind: 'admin' })
    expect(parseHashFragment('/admin/usage')).toEqual({
      kind: 'adminUsage',
    })
  })

  it("parses the cases surface's subroutes: the create page and one case's detail", () => {
    expect(parseHashFragment('/cases/new')).toEqual({ kind: 'casesCreate' })
    expect(parseHashFragment('/cases/case-9')).toEqual({
      kind: 'caseDetail',
      caseId: 'case-9',
    })
    // A case id never carries a slash: deeper paths are unknown
    // fragments, like every other unrecognized route.
    expect(parseHashFragment('/cases/a/b')).toEqual({ kind: 'unknown' })
    expect(parseHashFragment('/cases/')).toEqual({ kind: 'unknown' })
  })

  it('drops the query string when parsing a route', () => {
    expect(parseHashFragment('/notes?x=1')).toEqual({ kind: 'notes' })
  })

  it('parses the binding subroute into a target only for a demo provider carrying the full pair', () => {
    expect(
      parseHashFragment('/auth/binding/github?code=c&state=s'),
    ).toEqual({ kind: 'binding', target: { provider: 'github', code: 'c', state: 's' } })
  })

  it('degrades binding-shaped fragments without a valid target to account content', () => {
    // Unknown provider: never an exchange.
    expect(
      parseHashFragment('/auth/binding/twitter?code=c&state=s'),
    ).toEqual({ kind: 'account' })
    // A provider with only half (or none) of the pair: nothing to complete.
    expect(
      parseHashFragment('/auth/binding/github?code=c'),
    ).toEqual({ kind: 'account' })
    expect(parseHashFragment('/auth/binding/github')).toEqual({
      kind: 'account',
    })
  })

  it('parses the patient share fragment into the before/after pair of tokens', () => {
    // The share-link shape: /share/<before>/<after>, exactly two
    // further segments (a share token is base64url and never carries a
    // slash), constraining the pair the same way the case-detail route
    // constrains its own id.
    expect(parseHashFragment('/share/aB3-z_9-xyz/Tk-42_xz')).toEqual({
      kind: 'share',
      beforeToken: 'aB3-z_9-xyz',
      afterToken: 'Tk-42_xz',
    })
    expect(parseHashFragment('share/token-1/token-2')).toEqual({
      kind: 'share',
      beforeToken: 'token-1',
      afterToken: 'token-2',
    })
    expect(parseHashFragment('/share/token-1/token-2?lang=en-US')).toEqual({
      kind: 'share',
      beforeToken: 'token-1',
      afterToken: 'token-2',
    })
    // Share-shaped fragments without the full pair degrade to unknown
    // like every other unrecognized path -- a lone token cannot name a
    // comparison, and neither can a link with a third segment.
    expect(parseHashFragment('/share/')).toEqual({ kind: 'unknown' })
    expect(parseHashFragment('/share/token-1')).toEqual({ kind: 'unknown' })
    expect(parseHashFragment('/share/a/b/c')).toEqual({ kind: 'unknown' })
  })

  it('degrades anything else to unknown', () => {
    expect(parseHashFragment('/nope')).toEqual({ kind: 'unknown' })
    expect(parseHashFragment('nope')).toEqual({ kind: 'unknown' })
  })
})

describe('AppView', () => {
  // Each journey starts as a fresh visitor on the bare page. jsdom
  // keeps location.hash between tests, so a journey that navigated
  // would leak its fragment into the next one -- reset per test and
  // every journey is order-independent. The notes draft store is
  // module-scoped the same way, so a journey that typed into the notes
  // create form would leak its half-typed text into the next one.
  beforeEach(() => {
    window.location.hash = ''
    clearNotesDraft()
  })

  it('shows an anonymous visitor the sign-in surface over one config fetch', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    // The sign-in surface's own heading is the served brand.
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(view.getByText(zhCN.signIn.registerPrompt)).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: zhCN.signIn.registerAction }),
    ).toBeInTheDocument()
    expect(configGets(rig)).toBe(1)
    expect(rig.calls).toHaveLength(1)
  })

  it('renders the patient share page for a share fragment: no sign-in gate, no frame, no config fetch', async () => {
    // The share-link visitor journey: an anonymous visitor opens the
    // share link and meets the before/after pair -- the sign-in
    // surface that owns every other anonymous fragment never appears,
    // and neither does the clinic frame (no nav, no brand fetch: the
    // page's whole network activity is the two image loads, which
    // jsdom never performs).
    const rig = makeAppRig()
    const view = rendered(rig)
    navigateTo(`/share/patient-before-token/patient-after-token`)

    expect(await view.findByText(zhCN.shareView.heading)).toBeInTheDocument()
    const before = view.getByRole('img', {
      name: zhCN.shareView.beforeImageAlt,
    })
    expect(before.getAttribute('src')).toBe(
      `${window.location.origin}/api/v1/sharing/access?token=patient-before-token`,
    )
    const after = view.getByRole('img', {
      name: zhCN.shareView.afterImageAlt,
    })
    expect(after.getAttribute('src')).toBe(
      `${window.location.origin}/api/v1/sharing/access?token=patient-after-token`,
    )
    expect(before.getAttribute('src')).not.toBe(after.getAttribute('src'))
    expect(
      view.queryByRole('link', { name: zhCN.nav.home }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: zhCN.signIn.registerAction }),
    ).not.toBeInTheDocument()
    // The page added nothing to the one config fetch the anonymous
    // first paint made before the fragment was entered: the patient
    // page itself performs no API traffic.
    expect(configGets(rig)).toBe(1)
    expect(rig.calls).toHaveLength(1)
  })

  it('signs in into the frame: header brand, nav with host-computed selection, home content', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)

    // The frame is up: the nav is frame chrome.
    await view.findByRole('link', { name: zhCN.nav.notes })
    // Brand in the AppBar and on the home heading: two rendered slots,
    // one served value, one fetch. The rig's answer enables no
    // features, so the home panel under the heading is its honest
    // empty state -- the intro renders only beside the card list its
    // words describe, which a no-features answer does not show
    // (home-view.test.tsx pins that rule).
    expect(view.getAllByText(BRAND)).toHaveLength(2)
    expect(view.getByText(zhCN.home.emptyTitle)).toBeInTheDocument()

    const homeLink = view.getByRole('link', { name: zhCN.nav.home })
    expect(homeLink).toHaveAttribute('href', '#/')
    expect(homeLink).toHaveAttribute('aria-current', 'page')
    const notesLink = view.getByRole('link', { name: zhCN.nav.notes })
    expect(notesLink).toHaveAttribute('href', '#/notes')
    expect(notesLink).not.toHaveAttribute('aria-current')
    const accountLink = view.getByRole('link', { name: zhCN.nav.account })
    expect(accountLink).toHaveAttribute('href', '#/account')
    expect(accountLink).not.toHaveAttribute('aria-current')

    // One config fetch served the whole first paint and the frame.
    expect(configGets(rig)).toBe(1)
    expect(
      rig.calls.some(
        (call) =>
          call.method === 'POST' && call.path === '/api/v1/authn/login/password',
      ),
    ).toBe(true)
  })

  it("a clinic owner is never offered either administration entry, and a direct visit meets the gates' refusals", async () => {
    // The platform-staff gate's other half, at the unit tier: the
    // account with every clinic-level power (the rig's owner shape,
    // signed into a demo tenant) must not be offered an entrance to
    // the platform's tenant ledger or its usage dashboard -- both
    // entries exist on the platform frame alone. A direct visit still
    // renders each surface, whose own gate (its read) answers the
    // admin route guard's 403 and falls shut to the no-permission
    // suit: no ledger rows, no dashboard content, no entry anywhere.
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.home })

    expect(
      view.queryByRole('link', { name: zhCN.nav.admin }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('link', { name: zhCN.nav.adminUsage }),
    ).not.toBeInTheDocument()

    navigateTo('#/admin')
    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    // The surface keeps its own heading but renders no ledger chrome:
    // the denied branch never mounts the ledger table (its Tenant
    // column header included).
    expect(
      view.getByRole('heading', { name: zhCN.admin.heading, level: 1 }),
    ).toBeInTheDocument()
    expect(
      view.queryByText(zhCN.admin.tenants.tenantColumn),
    ).not.toBeInTheDocument()

    // The usage dashboard's own fragment denies the same caller the
    // same way: the heading renders, the no-permission suit stands in
    // for its content (the read answers the guard's 403), and neither
    // administration entry exists.
    navigateTo('#/admin/usage')
    expect(
      await view.findByRole('heading', {
        name: zhCN.admin.usage.heading,
        level: 1,
      }),
    ).toBeInTheDocument()
    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(view.queryByText(zhCN.admin.usage.emptyTitle)).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.admin.usage.features.chatTokens),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('link', { name: zhCN.nav.admin }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('link', { name: zhCN.nav.adminUsage }),
    ).not.toBeInTheDocument()
  })

  it('the platform frame offers the administration entry and reads the tenant ledger through it', async () => {
    // The platform-staff gate's first half, at the unit tier: the
    // platform-staff shape (the rig's account signed into the system
    // pseudo-tenant, the demo-server's mirror of the staff account
    // whose admin:* grants live under rbac.SystemDomain) is offered
    // the Administration entry and reaches the ledger through it. The
    // rows name the demo roster's copy -- the ledger-naming property
    // itself is pinned at the view tier.
    const rig = makeAppRig({
      tenantId: SYSTEM_PSEUDO_TENANT_ID,
      initialAdminTenants: [
        {
          tenantId: 'tenant-acme',
          displayName: '',
          status: 'active',
          createdAt: '2026-09-04T00:00:00Z',
        },
        {
          tenantId: 'tenant-globex',
          displayName: '',
          status: 'active',
          createdAt: '2026-09-04T00:00:00Z',
        },
      ],
    })
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.home })

    const adminLink = view.getByRole('link', { name: zhCN.nav.admin })
    expect(adminLink).toHaveAttribute('href', '#/admin')
    expect(adminLink).not.toHaveAttribute('aria-current')
    await user.click(adminLink)
    expect(window.location.hash).toBe('#/admin')

    await view.findByRole('heading', { name: zhCN.admin.heading, level: 1 })
    await view.findByText(zhCN.tenants.acme)
    expect(view.getByText(zhCN.tenants.globex)).toBeInTheDocument()
    expect(
      view.getByRole('link', { name: zhCN.nav.admin }),
    ).toHaveAttribute('aria-current', 'page')
    expect(
      rig.calls.some(
        (call) =>
          call.method === 'GET' && call.path === '/api/v1/admin/tenants',
      ),
    ).toBe(true)
  })

  it('the platform frame offers the usage entry and reads the usage/billing dashboard through it', async () => {
    // The dashboard's own journey: the platform-staff shape (the rig's
    // account signed into the system pseudo-tenant) is offered the
    // usage entry beside the tenant-ledger one and reaches the served
    // rows through it -- recorded usage, balance and subscription state
    // rendered from the real dashboard answer.
    const rig = makeAppRig({
      tenantId: SYSTEM_PSEUDO_TENANT_ID,
      initialAdminTenants: [
        {
          tenantId: 'tenant-acme',
          displayName: '',
          status: 'active',
          createdAt: '2026-09-04T00:00:00Z',
        },
        {
          tenantId: 'tenant-globex',
          displayName: '',
          status: 'active',
          createdAt: '2026-09-04T00:00:00Z',
        },
      ],
      initialUsageSummary: [
        {
          tenantId: 'tenant-acme',
          displayName: '',
          meteringSummaries: [
            {
              feature: 'ai.chat_tokens',
              periodStart: '2026-09-01T00:00:00Z',
              periodEnd: '2026-10-01T00:00:00Z',
              quantity: 20,
            },
          ],
          creditBalance: { available: 990, reserved: 0 },
          activeSubscription: {
            id: 'sub-1',
            planId: 'plan-demo',
            status: 'active',
            createdAt: '2026-09-04T00:00:00Z',
          },
        },
        {
          tenantId: 'tenant-globex',
          displayName: '',
          meteringSummaries: [],
          creditBalance: { available: 1000, reserved: 0 },
        },
      ],
    })
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.home })

    const usageLink = view.getByRole('link', { name: zhCN.nav.adminUsage })
    expect(usageLink).toHaveAttribute('href', '#/admin/usage')
    expect(usageLink).not.toHaveAttribute('aria-current')
    expect(
      view.getByRole('link', { name: zhCN.nav.admin }),
    ).toBeInTheDocument()
    await user.click(usageLink)
    expect(window.location.hash).toBe('#/admin/usage')

    await view.findByRole('heading', {
      name: zhCN.admin.usage.heading,
      level: 1,
    })
    expect(
      view.getByText(zhCN.admin.usage.features.chatTokens),
    ).toBeInTheDocument()
    expect(view.getByText(zhCN.tenants.globex)).toBeInTheDocument()
    expect(view.getByText(zhCN.admin.usage.noUsage)).toBeInTheDocument()
    expect(
      view.getByRole('link', { name: zhCN.nav.adminUsage }),
    ).toHaveAttribute('aria-current', 'page')
    expect(
      view.getByRole('link', { name: zhCN.nav.admin }),
    ).not.toHaveAttribute('aria-current')
    expect(
      rig.calls.some(
        (call) =>
          call.method === 'GET' && call.path === '/api/v1/admin/usage-summary',
      ),
    ).toBe(true)
  })

  it('reaches the cases surface from the nav: the clinic-titled list, the create page, and a case detail', async () => {
    const rig = makeAppRig({
      initialCases: [
        {
          id: 'case-1',
          patient_name: 'Nav Journey Patient',
          patient_ref: '',
          creator_user_id: 'user-1',
          created_at: '2026-09-04T00:00:00Z',
          photos: [],
        },
      ],
    })
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.home })

    // The journey's entrance is the frame's own navigation: the Cases
    // nav item opens the clinic's case list, whose level-one title
    // carries the current clinic's name.
    await user.click(view.getByRole('link', { name: zhCN.nav.cases }))
    expect(window.location.hash).toBe('#/cases')
    const listHeading = await view.findByRole('heading', { level: 1 })
    expect(listHeading).toHaveTextContent(
      `${zhCN.tenants.acme} · ${zhCN.cases.heading}`,
    )
    expect(view.getByText('Nav Journey Patient')).toBeInTheDocument()
    expect(
      view.getByRole('link', { name: zhCN.nav.cases }),
    ).toHaveAttribute('aria-current', 'page')

    // The New case button opens the one-page creation flow.
    await user.click(
      view.getByRole('button', { name: zhCN.cases.list.newCase }),
    )
    expect(window.location.hash).toBe('#/cases/new')
    await view.findByRole('textbox', {
      name: zhCN.cases.create.patientNameLabel,
    })

    // Opening the seeded case renders its detail (empty list of photos
    // answered for a photo-less case).
    navigateTo('#/cases/case-1')
    await view.findByText('Nav Journey Patient')
    expect(view.getByText(zhCN.cases.detail.noPhotos)).toBeInTheDocument()
    expect(
      view.getByRole('link', { name: zhCN.nav.cases }),
    ).toHaveAttribute('aria-current', 'page')
  })

  it('travels home/notes/account through the hash, lands the binding exchange back on the account fragment, and degrades unknown fragments', async () => {
    const server = demoServer({
      publicConfig: { config: { 'brand.site_name': BRAND }, features: [] },
    })
    // The binding callback's answer is held open so the journey can rest
    // on the completion handler's pending notice before the exchange
    // lands and the host's own navigation answers the onBound cue.
    let releaseBinding: (() => void) | undefined
    const rig = makeRealClientRig((call) => {
      if (
        call.method === 'POST' &&
        call.path === '/api/v1/authn/social/github/callback'
      ) {
        return new Promise<Response>((resolve) => {
          releaseBinding = () => {
            resolve(server(call))
          }
        })
      }
      return server(call)
    })
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    const notesLink = () => view.getByRole('link', { name: zhCN.nav.notes })
    const accountLink = () => view.getByRole('link', { name: zhCN.nav.account })
    const homeLink = () => view.getByRole('link', { name: zhCN.nav.home })

    navigateTo('#/notes')
    // The notes surface's read answered the demo's empty list, so the
    // list's empty state stands in for the notes page.
    expect(
      await view.findByText(zhCN.notes.list.emptyTitle),
    ).toBeInTheDocument()
    expect(notesLink()).toHaveAttribute('aria-current', 'page')
    expect(homeLink()).not.toHaveAttribute('aria-current')

    navigateTo('#/account')
    // The account fragment renders the account surface: the host's own
    // heading and intro above the account-ui sections. The heading is
    // role-scoped -- its text is also the account nav item's label.
    expect(
      await view.findByRole('heading', { name: zhCN.account.heading }),
    ).toBeInTheDocument()
    expect(view.getByText(zhCN.account.intro)).toBeInTheDocument()
    expect(accountLink()).toHaveAttribute('aria-current', 'page')
    expect(notesLink()).not.toHaveAttribute('aria-current')

    // The binding subroute renders the same surface with the completion
    // handler exchanging the fragment's (code, state) pair over the
    // app's client; the pending notice rests while the answer is held,
    // and the account nav stays selected.
    navigateTo('#/auth/binding/github?code=c&state=s')
    expect(
      await view.findByText(accountUiZhCN.bindingCallback.pending),
    ).toBeInTheDocument()
    expect(accountLink()).toHaveAttribute('aria-current', 'page')

    // The exchange lands a binding-shaped answer. The host answers the
    // handler's onBound cue by navigating back to the account fragment
    // -- the hash moves, the machine re-renders the plain account
    // surface, and the completion UI unmounts with it.
    act(() => {
      releaseBinding?.()
    })
    await waitFor(() => expect(window.location.hash).toBe('#/account'))
    await waitFor(() =>
      expect(
        view.queryByText(accountUiZhCN.bindingCallback.pending),
      ).not.toBeInTheDocument(),
    )
    expect(accountLink()).toHaveAttribute('aria-current', 'page')

    // Unknown fragments: home content, nothing selected. The home the
    // default no-features rig serves is its honest empty state (the
    // intro renders only beside the card list, which this rig never
    // answers with), so the empty state's title is the home-content
    // probe.
    navigateTo('#/definitely-not-a-route')
    await view.findByText(zhCN.home.emptyTitle)
    expect(homeLink()).not.toHaveAttribute('aria-current')
    expect(notesLink()).not.toHaveAttribute('aria-current')
    expect(accountLink()).not.toHaveAttribute('aria-current')
  })

  it('keeps a half-typed note across a hash round trip and turns the form over once the note lands', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    navigateTo('#/notes')
    const input = await view.findByLabelText(zhCN.notes.create.textLabel)
    const draft = 'Half-typed before the account detour'
    await user.type(input, draft)

    // The account detour and back -- the fragment round trip the
    // browser's Back button makes in this hash-routed app. The
    // surface unmounted with the detour, so only the draft store can
    // have kept the text.
    navigateTo('#/account')
    await view.findByRole('heading', { name: zhCN.account.heading })
    navigateTo('#/notes')
    const restored = await view.findByLabelText(zhCN.notes.create.textLabel)
    expect(
      restored,
      'the half-typed note was lost on the way back from another surface',
    ).toHaveValue(draft)

    // The create consumes the draft: the note lands and the field
    // turns over, so another round trip finds an empty form rather
    // than the same text a second time.
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )
    await waitFor(() => expect(restored).toHaveValue(''))
  })

  it('signs out of the frame into the session-ended screen, then back to sign-in', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    expect(
      await view.findByText(authUiZhCN.sessionEnded.title),
    ).toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )
    expect(await view.findByText(zhCN.signIn.registerPrompt)).toBeInTheDocument()

    // The whole journey stayed on one config fetch: the cache survived
    // the frame mount, the logout and the sign-in surface remount.
    expect(configGets(rig)).toBe(1)
    expect(
      rig.calls.some(
        (call) => call.method === 'POST' && call.path === '/api/v1/authn/logout',
      ),
    ).toBe(true)
  })

  it('announces the session-ended flip from the registered product-shell namespace: human text, never the raw key (reference-app-web.md P2-refappweb-1)', async () => {
    // The ended branch of the shell is the app's one unsolicited
    // whole-page switch -- a server-ended session and an explicit
    // sign-out being the same snapshot flip -- and its polite
    // role="status" announcement is the one string the shell renders
    // from its own product-shell namespace, which the assembly
    // registers at the host's bootstrap like every sibling's
    // (product-shell's resources.ts declares the obligation). An
    // unregistered host keeps the focus
    // half only: the shell's registration guard renders no region, so
    // assistive tech hears nothing when the whole page silently
    // switches under them.
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    // The flip into the ended view mounts the polite region with the
    // zh-CN copy this boot speaks (the suite's pinned language) --
    // the human text, whose absence is the regression, never the raw
    // key fallbackLng:false would render for a missing key.
    const region = await view.findByRole('status')
    await waitFor(() =>
      expect(region.textContent).toBe(
        productShellZhCN.announcements.sessionEnded,
      ),
    )
    // The announcement speaks the active language: a language switch
    // under the standing ended view re-renders the region from the
    // en-US bundle.
    await act(async () => {
      await switchLanguage(view.i18n, 'en-US')
    })
    await waitFor(() =>
      expect(region.textContent).toBe(
        productShellEnUS.announcements.sessionEnded,
      ),
    )
    // Whatever the language, the region never carries the raw key.
    expect(region.textContent).not.toContain('announcements.sessionEnded')
  })

  it('speaks the active language in the frame while the served brand stays verbatim', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    await act(async () => {
      await switchLanguage(view.i18n, 'en-US')
    })

    expect(view.getByRole('link', { name: enUS.nav.home })).toHaveAttribute(
      'aria-current',
      'page',
    )
    expect(view.getByRole('link', { name: enUS.nav.notes })).toBeInTheDocument()
    expect(view.getByRole('link', { name: enUS.nav.account })).toBeInTheDocument()
    // The rig answers with no enabled features, so the home panel is
    // its honest empty state and its title is app copy that answers
    // the switch (the intro renders only beside the card list).
    expect(view.getByText(enUS.home.emptyTitle)).toBeInTheDocument()
    expect(view.getAllByText(BRAND)).toHaveLength(2)
    expect(view.getByText(authUiEnUS.signOut.label)).toBeInTheDocument()
    // The demo roster's display names are app copy and switch language.
    expect(
      view.getByRole('button', { name: enUS.tenants.acme }),
    ).toBeInTheDocument()
  })

  it('a tenant switch revalidates the host-resolved Public config: the brand and the home feature cards answer the new tenant (reference-app-web.md P2-refapp-14)', async () => {
    // The shared demo server answers one static Public config; this
    // journey's responder answers per tenant instead, tracking the
    // tenant each completed switch names -- the shape a host serves
    // whose domain resolver maps more than one tenant. The host
    // switches tenants same-origin (no host header changes), so only a
    // revalidation on the switch can make the config-driven chrome --
    // the AppBar brand, the home heading and the feature cards --
    // converge on the new tenant's answers.
    const ACME_BRAND = 'Acme Smile Lab'
    const GLOBEX_BRAND = 'Globex Smile Lab'
    const inner = demoServer()
    let currentTenant = 'tenant-acme'
    const rig = makeRealClientRig(async (call) => {
      if (
        call.method === 'POST' &&
        call.path === '/api/v1/authn/tenant/switch'
      ) {
        const body = JSON.parse(call.body) as { tenant_id?: string }
        if (typeof body.tenant_id === 'string') {
          currentTenant = body.tenant_id
        }
      }
      if (call.path === '/api/v1/config/public') {
        const globex = currentTenant === 'tenant-globex'
        return jsonResponse(200, {
          config: {
            'brand.site_name': globex ? GLOBEX_BRAND : ACME_BRAND,
          },
          features: globex ? [] : [FEATURE_SMILE_PREVIEW],
        })
      }
      return inner(call)
    })
    const view = rendered(rig)
    const user = userEvent.setup()

    // tenant-acme serves its own brand and the smile-preview card over
    // the first-paint config fetch (the brand renders in two slots:
    // the AppBar and the home heading).
    await signInWithPasswordUi(view, user)
    expect(await view.findAllByText(ACME_BRAND)).toHaveLength(2)
    expect(
      view.getByText(zhCN.features.smilePreview.title),
    ).toBeInTheDocument()
    expect(configGets(rig)).toBe(1)

    // A switch commits: the notes list re-keys per tenant (its own
    // mechanism), and the Public-config cache is re-asked -- the brand
    // (AppBar and home heading: two rendered slots) and the feature
    // cards converge on tenant-globex's answers instead of holding
    // tenant-acme's, and the home panel's copy converges with them:
    // the intro and the cards leave with tenant-acme's answers, the
    // honest empty state arrives with tenant-globex's.
    await user.click(view.getByRole('button', { name: zhCN.tenants.acme }))
    await user.click(
      await view.findByRole('menuitem', { name: zhCN.tenants.globex }),
    )
    expect(await view.findAllByText(GLOBEX_BRAND)).toHaveLength(2)
    expect(view.getByText(zhCN.home.emptyTitle)).toBeInTheDocument()
    expect(
      view.queryByText(zhCN.home.intro),
      'an answer with no enabled features must not render the intro that introduces the card list',
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.smilePreview.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.premiumUpsell.title),
    ).not.toBeInTheDocument()
    // One revalidation fetch served the switched tenant's answers.
    expect(configGets(rig)).toBe(2)
  })
})
