/**
 * UserMenu contract: the AppBar's host composition of tenancy-ui's
 * switcher and auth-ui's sign-out over the session from app services.
 *
 * The roster is app data -- the demo seeds exactly two tenants whose
 * display names are app-namespace copy -- and the current tenant comes
 * from the auth-core hook (the principal's own claim, never local
 * memory). A completed switch lands the menu in the new tenant and
 * evicts the leaving tenant's query rows -- the tenant-namespaced-key
 * discipline, proven at the app layer by seeding the query client the
 * tree renders with: the leaving tenant's notes-list data must be gone
 * while an unrelated key and the other tenant's rows survive, and the
 * switch request on the wire carries the requested tenant id in its
 * body. The current row is rendered but disabled: a tenant you are in
 * is not a destination.
 */

import { waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import authUiZhCN from '../../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
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

  it('lands in the switched tenant and evicts the leaving tenant query rows only', async () => {
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

    await user.click(view.getByRole('button', { name: zhCN.tenants.acme }))
    await user.click(
      await view.findByRole('menuitem', { name: zhCN.tenants.globex }),
    )

    // The switch commits: the trigger relabels to the new tenant.
    await view.findByRole('button', { name: zhCN.tenants.globex })
    // The leaving tenant's rows are gone; nothing else was touched.
    await waitFor(() => {
      expect(view.queryClient.getQueryState(acmeNotesKey)).toBeUndefined()
    })
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
  })
})
