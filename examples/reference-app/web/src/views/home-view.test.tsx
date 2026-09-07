/**
 * HomeView contract: the signed-in landing surface renders only what
 * the server's Public config answer carries. The heading is the served
 * brand, the intro is app copy, and each feature card exists exactly
 * while its flag is enabled in the answer -- an empty feature list
 * renders no "Enabled features" heading, and instead the surface's own
 * honest empty state (never a promise of cards it has none of). Once a
 * session is signed in the clinic it runs under is named at
 * page-title level under the heading. The four-way matrix
 * (none, each flag alone, both) pins that data-driven shape, with the
 * card copy asserted through the app's own zh-CN fixture.
 */

import { describe, expect, it } from 'vitest'
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import {
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
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

  it('a clinic with no enabled feature is told the truth, never to seek an administrator', async () => {
    // The self-registered-practice shape -- the population the
    // new-practice gate meets, a real registrant whose server answer
    // enables nothing. The old copy -- "No features are enabled yet",
    // "Ask an administrator to enable a feature for this clinic" -- told
    // a practice that just registered itself that nothing works and to
    // go find the one person who does not exist: the registrant IS the
    // administrator, nothing in the app was ever disabled, and the
    // whole journey works from that very account. The surface instead
    // says something true: the work lives in the navigation, and
    // nothing needs to be enabled or asked for.
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [],
        },
      }),
    )
    const view = renderWithAppServices(
      <HomeView />,
      {
        session: rig.session,
        api: rig.api,
      },
      { language: 'en-US' },
    )
    await view.findByText(BRAND)
    expect(
      view.queryByText('No features are enabled yet'),
      'the empty-feature home must not claim a fresh clinic has disabled features',
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(
        'Ask an administrator to enable a feature for this clinic. Enabled features will appear here.',
      ),
      'the empty-feature home must not send a self-registered practice to an administrator who does not exist',
    ).not.toBeInTheDocument()
    // The true guide takes their place: the panel has nothing to show,
    // and the clinic's work -- named, ready, needing no one -- is the
    // honest answer. The assertions quote the shipped bundle fixtures,
    // never inline copy.
    expect(view.getByText(enUS.home.emptyTitle)).toBeInTheDocument()
    expect(view.getByText(enUS.home.emptyDescription)).toBeInTheDocument()
    // The intro renders the shipped bundle text too -- which makes no
    // cards-below promise the acceptance gate refuses.
    expect(view.getByText(enUS.home.intro)).toBeInTheDocument()
  })

  it('names the clinic being worked in at page-title level once signed in', async () => {
    // The acceptance shape for the home surface (current-clinic-is-
    // visible): the clinic the session runs under is named inside the
    // main content, under the page heading -- "which clinic am I in" is
    // a question a person asks before they act, and the chrome alone
    // cannot answer it for someone looking at their work.
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': BRAND },
          features: [],
        },
      }),
    )
    await signInWithPassword(rig)
    const view = renderWithAppServices(<HomeView />, {
      session: rig.session,
      api: rig.api,
    })
    await view.findByText(BRAND)
    expect(
      await view.findByText(
        zhCN.clinic.currentClinic.replace('{{name}}', zhCN.tenants.acme),
      ),
    ).toBeInTheDocument()
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
