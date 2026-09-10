/**
 * The demo accounts the reference-app server seeds at boot when
 * APP_DEMO_USERS_PASSWORD is set (internal/app/demo/demo_users.go's
 * demoSeedAccounts table), expressed for the specs that sign in as them.
 *
 * These mirror the server's own table rather than re-deriving it: each
 * account's grant model is what makes it useful to a spec, so the model
 * is recorded here beside the address it signs in with. A change to the
 * server's table without a change here shows up as a failing
 * authorization spec, which is the intended coupling.
 */
import { DEMO_PASSWORD } from '../../playwright.config.js'

/** One seeded demo account and the authorization it carries. */
export interface DemoAccount {
  /** What the account signs in with. */
  readonly email: string
  /** The shared demo passphrase every seeded account is registered with. */
  readonly password: string
}

/**
 * Membership and the built-in owner role in every configured tenant: the
 * account a spec uses when it needs to do something rather than be
 * refused.
 */
export const DEMO_OWNER: DemoAccount = {
  email: 'demo-owner@example.com',
  password: DEMO_PASSWORD,
}

/**
 * notes:read and nothing else, in every configured tenant: the account a
 * spec uses to assert that a read succeeds and the matching write is
 * refused.
 */
export const DEMO_READER: DemoAccount = {
  email: 'demo-reader@example.com',
  password: DEMO_PASSWORD,
}

/**
 * notes:read in the demo's single tenant and nowhere else: the account a
 * spec uses to assert a cross-tenant sign-in is refused.
 */
export const DEMO_ACME_ONLY: DemoAccount = {
  email: 'demo-acme-only@example.com',
  password: DEMO_PASSWORD,
}

/** The tenant ids the reference-app's demo wiring configures. */
export const TENANT_ACME = 'tenant-acme'
export const TENANT_GLOBEX = 'tenant-globex'
