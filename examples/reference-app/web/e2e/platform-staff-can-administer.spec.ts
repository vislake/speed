/**
 * That the platform's own operator can do their job by clicking.
 *
 * `go/admin` is wired into this app and answers today: probed against
 * the live deployment with a platform-staff token, `/api/v1/admin/tenants`,
 * `/audit-events`, `/roles` and `/impersonation` all answer 200 (`/users`
 * answers 400 for a missing query, which is a shape not a failure). The
 * tenant ledger, the cross-tenant user search, the impersonation
 * pipeline and the read-only audit query are all real, tested, mounted
 * code.
 *
 * What does not exist is a page. `web/src/views/` has no admin view and
 * the frame's navigation offers Home, Cases, Notes, Team, Credits and
 * Account -- so a platform operator cannot reach any of it without
 * curl. This is the same gap as the two the brief named and this suite
 * gated before it (saving the result, adding a colleague), in its third
 * form: capability without a surface.
 *
 * WHO THIS IS FOR, AND WHY THAT MATTERS TO THE GATE
 *
 * Not a dentist. The account is `demo-platform-staff@example.com`,
 * seeded only when the operator sets APP_DEMO_PLATFORM_STAFF_PASSWORD
 * (demo_admin.go), holding a system-domain grant no clinic user has.
 * So this surface must NOT appear for an ordinary owner: a clinic that
 * can see the platform's tenant ledger is a cross-tenant disclosure, and
 * this gate asserts that absence as firmly as it asserts the presence.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * That a signed-in platform operator can reach an administration
 * surface and read the tenant ledger from it, and that a clinic owner
 * cannot. It does not prescribe where the surface lives, what it is
 * called, which of admin's five capability groups it exposes first, or
 * whether impersonation and audit export appear at all -- those are
 * product decisions. Tenant rows and an operator-only entrance are the
 * floor.
 *
 * ONE TRAP RECORDED IN ADVANCE, BECAUSE THIS SUITE HAS PAID FOR IT
 *
 * The ledger's rows carry an EMPTY displayName today (probed live:
 * `{"tenantId":"tenant-64307885-...","displayName":"","status":"active"}`).
 * Rendered as-is, the page would list `tenant-64307885-c306-...` -- the
 * identical defect the Team surface shipped and 42a14614 fixed, where a
 * roster answered "who works here" with UUIDs. So the assertion below
 * refuses a raw tenant id where a tenant's identity belongs. A display
 * name, a clinic name resolved from org, or a short stable label each
 * pass; the id alone does not.
 *
 * @pending in the tag's second sense: the surface does not exist yet
 * rather than being broken.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import {
  SHELL_TEXT,
  expectSignedIn,
  openSurface,
  signInAs,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

/**
 * The platform operator's own account, and the password the suite runs
 * it under.
 *
 * Deliberately NOT defaulted to a value committed here. The demo-user
 * password is a repository constant because those three accounts are
 * the suite's ordinary subjects; this one holds a system-domain grant,
 * and a platform-staff password living in a public repository is a
 * different order of exposure -- the deployment's own operator sets it
 * and keeps it. So a local run supplies it through the environment
 * alongside the server it boots, and a deployment run needs the
 * operator to pass it; without it the gate skips rather than failing,
 * the same shape the @deployment gates use for E2E_BASE_URL.
 */
const PLATFORM_STAFF = {
  email: 'demo-platform-staff@example.com',
  password: process.env.E2E_PLATFORM_STAFF_PASSWORD,
} as const

/**
 * Where the administration surface might live. Expectations of
 * accessible names, not decrees.
 *
 * Deliberately NOT matching "Account" or "Team": both exist and are
 * clinic-scoped. A pattern loose enough to reach an admin page that
 * might sit near them is loose enough to land on one of them and pass
 * -- the mistake block D's nav pattern made once, and the one the
 * add-a-colleague gate had to be written around.
 */
const ADMIN_UI = {
  surface: /admin|administration|platform|operations|tenants/i,
} as const

/** A tenant id as the ledger stores it, which is not a tenant's name. */
const RAW_TENANT_ID = /\btenant-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/i

// @deployment as well as @pending, and that pairing is required rather
// than decorative: this gate SKIPS itself unless
// E2E_PLATFORM_STAFF_PASSWORD is set, and e2e/README.md's own rule says
// a gate that skips itself unless an environment variable is set must
// also carry @deployment -- because with E2E_BASE_URL set the config
// narrows the run to @deployment and a command-line --grep intersects
// with that rather than replacing it. Without the tag the gate is
// excluded at BOTH ends: skipped locally by its own guard, filtered out
// on a deployment for lacking the tag. One gate lived like that (the
// session address-source check) and had never run anywhere. This one
// nearly did too -- the first attempt to run it against a real server
// answered "No tests found", which is exactly how that failure looks.
test(
  'a platform operator can administer tenants by clicking',
  { tag: ['@pending', '@deployment'] },
  async ({ page }) => {
    test.skip(
      PLATFORM_STAFF.password === undefined,
      'E2E_PLATFORM_STAFF_PASSWORD is the operator\'s own secret; without it this gate has no subject to sign in as',
    )

    await visitSignIn(page)
    await submitPasswordSignIn(page, PLATFORM_STAFF.email, PLATFORM_STAFF.password as string)
    await expectSignedIn(page)

    // Reachable at all, asserted on its own so a failure here says the
    // operator has nowhere to go rather than that some control inside
    // is missing.
    await openSurface(page, ADMIN_UI.surface)

    // The tenant ledger is the floor: admin's own first capability, and
    // the one every other operator task starts from.
    const work = page.getByRole('main')
    await expect(
      work,
      'the administration surface does not show the platform\'s tenants, which is the ledger every other operator task starts from',
    ).toContainText(/tenant|clinic/i)

    // And it names them. See the trap recorded in the header: the
    // ledger's displayName is empty today, so rendering the row as
    // stored would repeat the defect 42a14614 fixed on the Team
    // surface.
    const shown = await work.innerText()
    const rawIds = shown.match(new RegExp(RAW_TENANT_ID, 'gi')) ?? []
    expect(
      rawIds,
      `the administration surface identifies tenants by raw id (${rawIds.join(' , ')}). admin's ledger stores an empty displayName, so a row rendered as-is says nothing an operator can act on -- the same shape the Team roster shipped with and 42a14614 closed`,
    ).toEqual([])
  },
)

test(
  'a clinic owner cannot reach the platform administration surface',
  { tag: ['@pending', '@deployment'] },
  async ({ page }) => {
    // The other half, and the half that makes the first one safe. A
    // clinic that can read the platform's tenant ledger is a
    // cross-tenant disclosure, so this is asserted with the same weight
    // as the presence above -- and it is asserted for the account that
    // has EVERY clinic-level power (owner in every configured tenant),
    // so a pass cannot be explained by the account simply being weak.
    await signInAs(page, DEMO_OWNER)
    await expectSignedIn(page)

    const entry = page.getByRole('link', { name: ADMIN_UI.surface })

    // Checked in the drawer too, not only in the visible nav: below the
    // md breakpoint AppShell collapses navigation behind the menu
    // button, so asserting the link's absence on a narrow viewport
    // without opening the drawer is a check satisfied by absence -- the
    // vacuous shape this suite has caught in itself twice.
    const menu = page.getByRole('button', { name: SHELL_TEXT.openNav })
    if (await menu.isVisible().catch(() => false)) {
      await menu.click()
      await expect(page.getByRole('link', { name: /home|cases/i }).first()).toBeVisible()
    }

    await expect(
      entry,
      'a clinic owner is offered an entrance to the platform administration surface, which would put every other practice\'s tenant row in front of one clinic',
    ).toHaveCount(0)
  },
)
