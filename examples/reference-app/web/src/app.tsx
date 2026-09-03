/**
 * app.tsx -- the reference-app web host's own view layer, composed
 * over the @speed packages exactly where a delivered consumer project
 * composes them: ProductShell (the auth-core-driven three-branch view
 * machine) around the AppShell frame the shell renders while the
 * snapshot is authenticated, with the nav, the header brand and the
 * user menu all host content computed from the app's own namespace
 * and the server's Public config.
 *
 * Routing is the app's one hand-rolled slice of chrome: the AppShell
 * nav items are anchors into the location hash (the shell never does
 * path-matching -- each item carries its own `selected`, computed
 * here), and this module parses that fragment into one of the four
 * content kinds the app knows. The three business surfaces live in
 * views/: home (config/feature-driven), notes and account. The
 * binding callback is the account surface's subroute: the fragment
 * /auth/binding/<provider>?code=<code>&state=<state> is what the
 * account-ui BindingCallbackHandler completes at, so it parses here
 * (provider validated against the demo provider set -- no unknown
 * provider ever drives an exchange) and renders inside the account
 * surface, the host answering the handler's onBound cue by navigating
 * back to the account fragment (the account-ui family never navigates
 * itself). Everything else degrades to home with no nav item
 * selected; there is no 404 chrome in this round.
 *
 * The brand in the AppBar and on the sign-in/home headings is the
 * server's own answer: the page renders the same Public
 * brand.site_name value the config module serves (useBrandName, in
 * app-services.tsx), never a local copy.
 */

import type { ReactElement } from 'react'
import { Typography } from '@mui/material'
import type { SocialProvider } from '@speed/account-ui'
import { useTranslation } from '@speed/i18n'
import { ProductShell } from '@speed/product-shell'
import type { AppShellNavItem } from '@speed/layout-kit'
import { useBrandName } from './app-services.js'
import { REFERENCE_APP_NAMESPACE } from './resources.js'
import { useHashRoute } from './useHashRoute.js'
import { HomeView } from './views/home-view.js'
import { NotesView } from './views/notes-view.js'
import { AccountView } from './views/account-view.js'
import { SignInView } from './views/sign-in-view.js'
import { UserMenu } from './views/user-menu.js'

/** The home fragment: the bare hash (''), '#' and '#/' all mean it. */
export const ROUTE_HOME = '/'
/** The notes list fragment. */
export const ROUTE_NOTES = '/notes'
/** The account surface fragment. */
export const ROUTE_ACCOUNT = '/account'
/** The account surface's binding-callback subroute prefix: the rest of
 * the path is the provider, the query the (code, state) pair the
 * exchange completes with. */
export const BINDING_ROUTE_PREFIX = '/auth/binding/'

/** The demo's social provider set: the authn spec's five providers,
 * in the account-ui vocabulary. The demo server configures none of
 * them for real OAuth, but the binding surface is exercised end to
 * end against scripted answers, so the callback route must recognize
 * them. */
export const DEMO_SOCIAL_PROVIDERS: readonly SocialProvider[] = [
  'google',
  'github',
  'wechat',
  'dingtalk',
  'feishu',
]

function isSocialProvider(value: string): value is SocialProvider {
  return (DEMO_SOCIAL_PROVIDERS as readonly string[]).includes(value)
}

/** The fragment kinds the app renders, with the binding target a
 * provider plus the (code, state) pair its callback route completes
 * with. */
export type AppFragment =
  | { readonly kind: 'home' }
  | { readonly kind: 'notes' }
  | { readonly kind: 'account' }
  | { readonly kind: 'binding'; readonly target: BindingTarget }
  | { readonly kind: 'unknown' }

export interface BindingTarget {
  readonly provider: SocialProvider
  readonly code: string
  readonly state: string
}

/** The fragment's path part without the query, canonicalized to a
 * leading-slash form ('' -- the bare page, whose hash the hook yields
 * for a fresh visit -- is the home path '/'). */
function pathOf(fragment: string): string {
  const queryIndex = fragment.indexOf('?')
  const path = queryIndex === -1 ? fragment : fragment.slice(0, queryIndex)
  return path === '' ? '/' : path.startsWith('/') ? path : `/${path}`
}

/** The fragment's raw query part without the leading '?'. */
function queryOf(fragment: string): string {
  const queryIndex = fragment.indexOf('?')
  return queryIndex === -1 ? '' : fragment.slice(queryIndex + 1)
}

/** Parses a hash fragment (without the '#') into the app fragment the
 * view layer renders. Non-route fragments degrade to unknown (home
 * content, no selected nav item). */
export function parseHashFragment(fragment: string): AppFragment {
  const path = pathOf(fragment)
  if (path === ROUTE_HOME) {
    return { kind: 'home' }
  }
  if (path === ROUTE_NOTES) {
    return { kind: 'notes' }
  }
  if (path === ROUTE_ACCOUNT) {
    return { kind: 'account' }
  }
  if (path.startsWith(BINDING_ROUTE_PREFIX)) {
    const provider = path.slice(BINDING_ROUTE_PREFIX.length)
    const params = new URLSearchParams(queryOf(fragment))
    const code = params.get('code')
    const state = params.get('state')
    if (isSocialProvider(provider) && code !== null && state !== null) {
      return { kind: 'binding', target: { provider, code, state } }
    }
    return { kind: 'account' }
  }
  return { kind: 'unknown' }
}

/** The nav item id selected for a fragment, or null when none is. */
function selectedNavId(fragment: AppFragment): string | null {
  switch (fragment.kind) {
    case 'home':
      return NAV_HOME
    case 'notes':
      return NAV_NOTES
    case 'account':
    case 'binding':
      return NAV_ACCOUNT
    case 'unknown':
      return null
  }
}

const NAV_HOME = 'nav-home'
const NAV_NOTES = 'nav-notes'
const NAV_ACCOUNT = 'nav-account'

function navHref(route: string): string {
  return `#${route}`
}

/** The AppView: the product-shell composition of this app, mounting
 * the frame's chrome and the surface for the current fragment. */
export function AppView(): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const fragment = useHashRoute()
  const parsed = parseHashFragment(fragment)
  const selected = selectedNavId(parsed)

  const navItems: readonly AppShellNavItem[] = [
    {
      id: NAV_HOME,
      label: t('nav.home'),
      href: navHref(ROUTE_HOME),
      selected: selected === NAV_HOME,
    },
    {
      id: NAV_NOTES,
      label: t('nav.notes'),
      href: navHref(ROUTE_NOTES),
      selected: selected === NAV_NOTES,
    },
    {
      id: NAV_ACCOUNT,
      label: t('nav.account'),
      href: navHref(ROUTE_ACCOUNT),
      selected: selected === NAV_ACCOUNT,
    },
  ]

  let content: ReactElement
  switch (parsed.kind) {
    case 'home':
    case 'unknown':
      content = <HomeView />
      break
    case 'notes':
      content = <NotesView />
      break
    case 'account':
      content = <AccountView />
      break
    case 'binding':
      // The binding subroute completes the exchange inside the account
      // surface; onBound is the host's cue that the exchange landed a
      // binding-shaped answer (the identities list refetched). The
      // answer here is navigation back to the account fragment, which
      // unmounts the completion handler -- its pending notice rests
      // until this cue, so the cue's answer is what takes it off the
      // page.
      content = (
        <AccountView
          bindingTarget={parsed.target}
          onBound={() => {
            window.location.hash = ROUTE_ACCOUNT
          }}
        />
      )
      break
  }

  return (
    <ProductShell
      navItems={navItems}
      header={<HeaderBrand />}
      userMenu={<UserMenu />}
      signIn={<SignInView />}
    >
      {content}
    </ProductShell>
  )
}

/** The AppBar brand: the server-served site name, truncated rather
 * than wrapped so a long configured brand never breaks the header. */
function HeaderBrand(): ReactElement {
  const brand = useBrandName()
  return (
    <Typography
      noWrap
      variant="h6"
      sx={{ color: 'inherit', fontWeight: 600 }}
    >
      {brand}
    </Typography>
  )
}
