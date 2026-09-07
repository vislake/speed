/**
 * CurrentClinicLine contract: the clinic-context line renders the
 * display name of the tenant the session runs under -- read from the
 * principal's own claim, never from local memory -- and nothing at all
 * while no tenant is known (an anonymous render, a tenant id that is
 * not on the app's demo roster). The switch flip is the line's reason
 * for existing: when the session commits a tenant switch the line
 * re-renders with the new clinic's name and the old name is gone, so a
 * person working in the main content area is told where their work now
 * goes -- the reference-app acceptance shape (current-clinic-is-
 * visible) that a chrome-only mention can never satisfy.
 *
 * Text expectations read the bundle values, never inline language (the
 * CJK scan treats test files as English text like everything else).
 */

import { describe, expect, it } from 'vitest'
import { act } from '@testing-library/react'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import {
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CurrentClinicLine } from './current-clinic.js'

/** The clinic line's text for one demo tenant, interpolated the way
 * the component interpolates it. */
function clinicTextOf(tenantName: string): string {
  return zhCN.clinic.currentClinic.replace('{{name}}', tenantName)
}

describe('CurrentClinicLine', () => {
  it('renders the clinic the session runs under, from the principal claim', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderWithAppServices(<CurrentClinicLine />, {
      session: rig.session,
      api: rig.api,
    })
    expect(
      await view.findByText(clinicTextOf(zhCN.tenants.acme)),
    ).toBeInTheDocument()
  })

  it('follows a committed switch: the new clinic is named, the old one is gone', async () => {
    // Fails before the line existed (nothing rendered); the assertion
    // that matters is the flip: a switch commits, and the main-content
    // context line must move with it, or a person whose switch just
    // changed where their records land would never be told.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderWithAppServices(<CurrentClinicLine />, {
      session: rig.session,
      api: rig.api,
    })
    await view.findByText(clinicTextOf(zhCN.tenants.acme))
    await act(async () => {
      await rig.session.switchTenant('tenant-globex')
    })
    expect(
      await view.findByText(clinicTextOf(zhCN.tenants.globex)),
    ).toBeInTheDocument()
    expect(
      view.queryByText(clinicTextOf(zhCN.tenants.acme)),
    ).not.toBeInTheDocument()
  })

  it('renders nothing while no tenant is known', () => {
    // The anonymous render: the session is attached but never signed
    // in, so the hooks' fail-closed snapshot has no tenant -- the line
    // names no clinic rather than guessing one.
    const rig = makeRealClientRig(demoServer())
    const view = renderWithAppServices(
      <CurrentClinicLine />,
      { session: rig.session, api: rig.api },
    )
    expect(view.container.textContent).toBe('')
  })
})
