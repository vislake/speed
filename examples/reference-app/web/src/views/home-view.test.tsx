/**
 * HomeView contract: the signed-in landing surface renders only what
 * the server's Public config answer carries. The heading is the served
 * brand, the intro is app copy, and each feature card exists exactly
 * while its flag is enabled in the answer -- an empty feature list
 * renders no "Enabled features" heading at all. The four-way matrix
 * (none, each flag alone, both) pins that data-driven shape, with the
 * card copy asserted through the app's own zh-CN fixture.
 */

import { describe, expect, it } from 'vitest'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import {
  FEATURE_PREMIUM_UPSELL,
  FEATURE_SMILE_PREVIEW,
  HomeView,
} from './home-view.js'

/** The demo server's served brand, scripted as Public config data. */
const BRAND = 'Demo Smile Lab'

describe('HomeView', () => {
  it('renders brand and intro over one config fetch', async () => {
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [FEATURE_SMILE_PREVIEW],
        },
      }),
    )
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(view.getByText(zhCN.home.intro)).toBeInTheDocument()
    expect(rig.calls).toHaveLength(1)
  })

  it('shows no feature heading and no card while the list is empty', async () => {
    // The list is what is empty: a served brand is present, no feature
    // flags enabled -- the no-heading shape is a data answer, not a
    // missing-config state.
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [],
        },
      }),
    )
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    await view.findByText(BRAND)
    expect(view.queryByText(zhCN.features.heading)).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.smilePreview.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.premiumUpsell.title),
    ).not.toBeInTheDocument()
  })

  it('renders the plain flag\'s card only while that flag is enabled', async () => {
    // The answer enables the plain flag alone: its card renders, the
    // dependent flag's does not -- the view resolves no dependency
    // closure of its own.
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [FEATURE_SMILE_PREVIEW],
        },
      }),
    )
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(
      view.getByText(zhCN.features.smilePreview.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(zhCN.features.smilePreview.description),
    ).toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.premiumUpsell.title),
    ).not.toBeInTheDocument()
  })

  it('renders the dependent flag\'s card exactly as the answer serves it', async () => {
    // The answer the server resolves never carries the dependent flag
    // without its dependency, but the view is a pure passthrough of the
    // served list: an answer naming the dependent flag alone renders
    // its card and none other -- no client-side closure in either
    // direction.
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [FEATURE_PREMIUM_UPSELL],
        },
      }),
    )
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(
      view.queryByText(zhCN.features.smilePreview.title),
    ).not.toBeInTheDocument()
    expect(
      view.getByText(zhCN.features.premiumUpsell.title),
    ).toBeInTheDocument()
  })

  it('renders every card when every demo flag is enabled', async () => {
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [FEATURE_SMILE_PREVIEW, FEATURE_PREMIUM_UPSELL],
        },
      }),
    )
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(
      view.getByText(zhCN.features.smilePreview.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(zhCN.features.premiumUpsell.title),
    ).toBeInTheDocument()
  })
})
