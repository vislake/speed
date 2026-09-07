/**
 * CasesView contract: the clinic's case list renders what the server
 * answers and reports its two cues upward. The journeys drive a real
 * client bound into the runtime seam and sign in through the real
 * session operation (the surface reads the current tenant from the
 * auth-core hooks), over the demo server's cases endpoints mirroring
 * the real fragment's clinic-wide shape.
 *
 * The block-A title regression lives here first: the cases page's
 * level-one heading carries the current clinic's display name -- the
 * same host roster the tenant switcher reads -- composed with the
 * title "Cases" (never "My Cases"), so a person working in Acme knows
 * where their work lands (the current-clinic gate's cases leg).
 *
 * The list has no permission gate (any tenant member may read it), so
 * the read-failure states are load failures: a coded 5xx and a
 * codeless refusal each render the ui-kit error empty state, never the
 * stale rows of an earlier read and never a permanent pending.
 */

import { waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { RequestFn } from '@speed/api-client'
import type { CasesCase } from '@speed/api-sdk'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { describe, expect, it, vi } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import layoutKitZhCN from '../../../../../web/packages/layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import type { RenderWithAppServicesOptions } from '../test-utils/render.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CasesView } from './cases-view.js'

/** A transport whose every call rejects with a raw, code-less error --
 * the shape a bug-shaped transport throw arrives in: no envelope code,
 * no client.* code, nothing a surface can read. */
const UNNORMALIZED_TRANSPORT_FAILURE: RequestFn = () =>
  Promise.reject(
    new Error('[cases-view.test] an un-normalized transport failure'),
  )

/** The fixed demo epoch the demo server's case answers carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

const CASE_ONE: CasesCase = {
  id: 'case-1',
  patient_name: 'Anna Meyer',
  patient_ref: 'CH-1001',
  creator_user_id: 'user-1',
  created_at: DEMO_CREATED_AT,
  photos: [],
}
const CASE_TWO: CasesCase = {
  id: 'case-2',
  patient_name: 'Ben Chen',
  patient_ref: '',
  creator_user_id: 'user-2',
  created_at: DEMO_CREATED_AT,
  photos: [],
}

/** The date cell text the view's own Intl formatting produces. */
function createdAtText(language: string, value: string): string {
  return new Intl.DateTimeFormat(language, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

/** Renders the cases list over a signed-in rig (the surface reads the
 * current tenant from the auth-core hooks). */
function renderCases(
  rig: RealClientRig,
  options: RenderWithAppServicesOptions = {},
) {
  const onNewCase = vi.fn()
  const onOpenCase = vi.fn()
  const view = renderWithAppServices(
    <CasesView onNewCase={onNewCase} onOpenCase={onOpenCase} />,
    { session: rig.session, api: rig.api },
    options,
  )
  return { ...view, onNewCase, onOpenCase }
}

describe('CasesView', () => {
  it('titles the page with the clinic name at the title level, never "My Cases"', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderCases(rig)

    // The one h1 of the page composes the current clinic's display name
    // (the demo roster's Acme) with the surface's own title. This is the
    // cases leg of the current-clinic gate: the clinic being worked in
    // is named where the work is.
    const heading = await view.findByRole('heading', { level: 1 })
    expect(heading).toHaveTextContent(
      `${zhCN.tenants.acme} · ${zhCN.cases.heading}`,
    )
    // The UI title is Cases -- never "My Cases": no rendered text on the
    // page carries the creator-scoped wording.
    expect(view.queryByText(/My Cases/)).not.toBeInTheDocument()
    expect(view.getByText(zhCN.cases.intro)).toBeInTheDocument()
  })

  it('renders the served clinic list: patient name, reference and opened date per case', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialCases: [CASE_ONE, CASE_TWO] }),
    )
    await signInWithPassword(rig)
    const view = renderCases(rig)

    await view.findByText(CASE_ONE.patient_name)
    expect(view.getByText(CASE_TWO.patient_name)).toBeInTheDocument()
    const date = createdAtText('zh-CN', DEMO_CREATED_AT)
    // The reference-carrying row's secondary line is ref · date; the
    // ref-less row's is the date alone.
    expect(
      view.getByText(`${CASE_ONE.patient_ref} · ${date}`),
    ).toBeInTheDocument()
    expect(view.getByText(date)).toBeInTheDocument()
  })

  it('renders the empty state when the clinic has no cases yet', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderCases(rig)

    await view.findByText(zhCN.cases.list.emptyTitle)
    expect(view.getByText(zhCN.cases.list.emptyDescription)).toBeInTheDocument()
  })

  it('reports the new-case and open-case cues upward', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialCases: [CASE_ONE, CASE_TWO] }),
    )
    await signInWithPassword(rig)
    const view = renderCases(rig)

    await view.findByText(CASE_ONE.patient_name)
    await userEvent.click(view.getByRole('button', { name: zhCN.cases.list.newCase }))
    expect(view.onNewCase).toHaveBeenCalledTimes(1)

    await userEvent.click(view.getByText(CASE_ONE.patient_name))
    await waitFor(() => expect(view.onOpenCase).toHaveBeenCalledWith('case-1'))
  })

  it('renders the pending state until the read answers, then the rows', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialCases: [CASE_ONE] }),
    )
    await signInWithPassword(rig)
    const view = renderCases(rig)

    // The gate's pending spinner (layout-kit's own labelled
    // CircularProgress) stands in until the served list flips the gate.
    expect(
      view.getByRole('progressbar', { name: layoutKitZhCN.routeGuard.pending }),
    ).toBeInTheDocument()
    await view.findByText(CASE_ONE.patient_name)
    expect(
      view.queryByRole('progressbar', { name: layoutKitZhCN.routeGuard.pending }),
    ).not.toBeInTheDocument()
  })

  it('a coded 5xx read failure renders the error empty state, never rows', async () => {
    const rig = makeRealClientRig(
      demoServer({ denyCasesList: true, initialCases: [CASE_ONE] }),
    )
    await signInWithPassword(rig)
    const view = renderCases(rig)

    await view.findByText(uiKitZhCN.emptyState.error.title)
    expect(view.getByText(uiKitZhCN.emptyState.error.description)).toBeInTheDocument()
    expect(view.queryByText(CASE_ONE.patient_name)).not.toBeInTheDocument()
  })

  it('a codeless refusal renders the error empty state, never a permanent pending', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    // Replace the bound transport with the codeless-failing one before
    // the view mounts: the read rejects raw, and the surface must land
    // on the read-error state rather than parking at pending forever.
    bindRequestFn(UNNORMALIZED_TRANSPORT_FAILURE)
    const view = renderCases(rig)

    await view.findByText(uiKitZhCN.emptyState.error.title)
    expect(
      view.queryByRole('progressbar', { name: layoutKitZhCN.routeGuard.pending }),
    ).not.toBeInTheDocument()
  })
})
