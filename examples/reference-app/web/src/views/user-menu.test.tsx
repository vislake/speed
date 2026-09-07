/**
 * UserMenu contract: the AppBar's host composition of tenancy-ui's
 * switcher and auth-ui's sign-out over the session from app services.
 *
 * The roster is app data -- the demo seeds exactly two tenants whose
 * display names are app-namespace copy -- and the current tenant comes
 * from the auth-core hook (the principal's own claim, never local
 * memory). A completed switch lands the menu in the new tenant and
 * evicts the rows the departing access token fetched -- the tenant-
 * namespaced-key discipline, proven at the app layer by seeding the
 * query client the tree renders with: the leaving tenant's notes-list
 * data must be gone, and so must the identity-domain rows the account
 * surface reads (their bare spec-path keys carry no tenant segment --
 * reference-app-web.md P1-apisdk-1), while an unrelated key and the
 * other tenant's rows survive. The switch also re-asks the host-
 * resolved Public config (one revalidation fetch on the wire --
 * reference-app-web.md P2-refapp-14), and the switch request itself
 * carries the requested tenant id in its body. The current row is
 * rendered but disabled: a tenant you are in is not a destination.
 */

import { waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import authUiZhCN from '../../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import {
  FIRST_REGISTERED_CLINIC_TENANT_ID,
  demoServer,
} from '../test-utils/demo-server.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { UserMenu } from './user-menu.js'

/** The demo's seeded tenant ids, as the server and the roster know them. */
const TENANT_ACME = 'tenant-acme'
const TENANT_GLOBEX = 'tenant-globex'

/** A signed-in rig: a completed password sign-in lands the session in
 * the demo's default tenant (tenant-acme). */
async function makeSignedInRig(): Promise<ReturnType<typeof makeRealClientRig>> {
  const rig = makeRealClientRig(demoServer())
  await signInWithPassword(rig)
  return rig
}

async function renderedUserMenu(
  rig: ReturnType<typeof makeRealClientRig>,
) {
  return renderWithAppServices(<UserMenu />, {
    session: rig.session,
    api: rig.api,
  })
}

describe('UserMenu', () => {
  it('shows the current tenant as the trigger, disables the row you are in, and renders sign-out', async () => {
    const rig = await makeSignedInRig()
    const view = await renderedUserMenu(rig)
    const user = userEvent.setup()

    // The trigger reads the current tenant's display name (app copy).
    const trigger = view.getByRole('button', { name: zhCN.tenants.acme })
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    expect(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    ).toBeInTheDocument()

    await user.click(trigger)
    const currentRow = await view.findByRole('menuitem', {
      name: zhCN.tenants.acme,
    })
    // MUI v9 renders a disabled MenuItem as aria-disabled="true" (no
    // native disabled attribute on the li) -- the a11y semantics are
    // asserted directly, as the tenancy-ui package's own suite does.
    expect(currentRow).toHaveAttribute('aria-disabled', 'true')
    expect(
      view.getByRole('menuitem', { name: zhCN.tenants.globex }),
    ).not.toHaveAttribute('aria-disabled')
  })

  it('lands in the switched tenant, evicts the leaving tenant rows AND the identity-domain rows, and revalidates the Public config', async () => {
    const rig = await makeSignedInRig()
    const view = await renderedUserMenu(rig)
    const user = userEvent.setup()

    // The notes surface's cache shape: tenant-scoped keys under the
    // app's shared prefix, plus unrelated rows that must survive.
    const acmeNotesKey = ['tenant', TENANT_ACME, 'notes-list']
    const globexOtherKey = ['tenant', TENANT_GLOBEX, 'other']
    const unrelatedKey = ['preferences']
    view.queryClient.setQueryData(acmeNotesKey, ['note-1'])
    view.queryClient.setQueryData(globexOtherKey, ['globex-row'])
    view.queryClient.setQueryData(unrelatedKey, ['pref-1'])
    // The account surface's identity-domain keys -- bare spec paths,
    // no tenant segment (the login-history key carrying its {limit}
    // params as a further element, the shape the real hook's key has).
    const sessionsKey = ['/api/v1/authn/sessions']
    const loginHistoryKey = ['/api/v1/authn/login-history', { limit: 20 }]
    const identitiesKey = ['/api/v1/authn/identities']
    view.queryClient.setQueryData(sessionsKey, ['session-row'])
    view.queryClient.setQueryData(loginHistoryKey, ['attempt-row'])
    view.queryClient.setQueryData(identitiesKey, ['identity-row'])

    await user.click(view.getByRole('button', { name: zhCN.tenants.acme }))
    await user.click(
      await view.findByRole('menuitem', { name: zhCN.tenants.globex }),
    )

    // The switch commits: the trigger relabels to the new tenant.
    await view.findByRole('button', { name: zhCN.tenants.globex })
    // The leaving tenant's rows and the identity-domain rows are gone;
    // nothing else was touched.
    await waitFor(() => {
      expect(view.queryClient.getQueryState(acmeNotesKey)).toBeUndefined()
    })
    expect(view.queryClient.getQueryState(sessionsKey)).toBeUndefined()
    expect(view.queryClient.getQueryState(loginHistoryKey)).toBeUndefined()
    expect(view.queryClient.getQueryState(identitiesKey)).toBeUndefined()
    expect(view.queryClient.getQueryState(globexOtherKey)).toBeDefined()
    expect(view.queryClient.getQueryState(unrelatedKey)).toBeDefined()

    // The switch request on the wire named the destination tenant in
    // its body -- the server answers with a principal in that tenant.
    const switchCall = rig.calls.find(
      (call) =>
        call.method === 'POST' && call.path === '/api/v1/authn/tenant/switch',
    )
    expect(switchCall).toBeDefined()
    const switchBody = JSON.parse(switchCall?.body ?? '{}') as Record<
      string,
      unknown
    >
    expect(switchBody.tenant_id).toBe(TENANT_GLOBEX)

    // The completed switch re-asked the host-resolved Public config:
    // the menu's own subscription started the shared cache's first
    // fetch, and the switch's revalidation issued a second one.
    const configCalls = rig.calls.filter(
      (call) => call.path === '/api/config/public',
    )
    expect(configCalls).toHaveLength(2)
  })

  it('names a current tenant outside the demo roster on the trigger and in the list -- a self-service account in its own clinic', async () => {
    // A registered account signs into the clinic its registration
    // provisioned (demo-server.ts's self-service mirror of
    // cmd/server/self_service.go): the fixture's derived clinic tenant
    // id is not among the seeded demo tenants the roster knows, so the
    // menu must still render a live trigger naming it -- never the
    // no-current-tenant disabled state a signed-in account would be
    // stranded in.
    const rig = makeRealClientRig(
      demoServer({ tenantId: FIRST_REGISTERED_CLINIC_TENANT_ID }),
    )
    await signInWithPassword(rig)
    const view = await renderedUserMenu(rig)
    const user = userEvent.setup()

    const trigger = view.getByRole('button', {
      name: FIRST_REGISTERED_CLINIC_TENANT_ID,
    })
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    await user.click(trigger)
    const currentRow = await view.findByRole('menuitem', {
      name: FIRST_REGISTERED_CLINIC_TENANT_ID,
    })
    // The clinic row is the current row: rendered, disabled (a tenant
    // you are in is not a destination). The demo roster rows stay
    // listed beside it, as far as the server's own membership answers
    // let a switch through.
    expect(currentRow).toHaveAttribute('aria-disabled', 'true')
    expect(
      view.getByRole('menuitem', { name: zhCN.tenants.acme }),
    ).toBeInTheDocument()
    expect(
      view.getByRole('menuitem', { name: zhCN.tenants.globex }),
    ).toBeInTheDocument()
  })
})
