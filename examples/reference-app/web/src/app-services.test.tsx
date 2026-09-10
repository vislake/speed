/**
 * app-services contract: the shell's one server-driven text and the
 * shared fetch behind it.
 *
 * useBrandName renders go/config's Public brand.site_name answer
 * verbatim when it is a non-empty string -- data, never a translation
 * -- and the app namespace's fallback while the value loads, when the
 * fetch failed, or when the answer is not a string. The fallback is
 * itself translated (both languages asserted), because it is the one
 * brand text that is app copy.
 *
 * The single-fetch claim is the second half of this module's contract:
 * every consumer on the page -- the header brand, the sign-in brand,
 * the home brand and each feature check -- passes the same RequestFn
 * from app services, and the api-client config cache shares one fetch
 * per RequestFn identity, so N consumers never mean N requests. Two
 * mounted probes asserting one observed GET pins that at the app layer.
 *
 * The fallback copy is asserted through the app namespace's own
 * fixtures (imported relatively), never inline -- the CJK scan treats
 * test files as English text like everything else, and inline copy
 * would drift from the resources.
 */

import type { ReactElement } from 'react'
import { describe, expect, it } from 'vitest'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import { jsonResponse } from './test-utils/real-client.js'
import { makeRealClientRig } from './test-utils/real-client.js'
import { demoServer } from './test-utils/demo-server.js'
import { renderWithAppServices } from './test-utils/render.js'
import { useBrandName } from './app-services.js'

function BrandProbe(): ReactElement {
  const brand = useBrandName()
  return <div>{brand}</div>
}

const CONFIG_GETS = (rigCalls: readonly { path: string }[]): number =>
  rigCalls.filter((call) => call.path === '/api/v1/config/public').length

describe('useBrandName', () => {
  it('renders the served brand.site_name value verbatim', async () => {
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: { config: { 'brand.site_name': 'Demo Smile Lab' }, features: [] },
      }),
    )
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await findByText('Demo Smile Lab')).toBeInTheDocument()
  })

  it('falls back to the app namespace while the value is absent', async () => {
    const rig = makeRealClientRig(demoServer())
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    })
    // The empty Public answer carries no brand.site_name: the fallback
    // renders once the fetch settles (and is also the pre-fetch text,
    // which is exactly the no-flash property the shell wants).
    expect(await findByText(zhCN.brand.fallback)).toBeInTheDocument()
    expect(CONFIG_GETS(rig.calls)).toBe(1)
  })

  it('falls back when the served value is not a string', async () => {
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: { config: { 'brand.site_name': 42 }, features: [] },
      }),
    )
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await findByText(zhCN.brand.fallback)).toBeInTheDocument()
  })

  it('falls back when the config fetch fails, without throwing', async () => {
    const rig = makeRealClientRig(() => {
      throw new TypeError('network down')
    })
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await findByText(zhCN.brand.fallback)).toBeInTheDocument()
  })

  it('falls back when a shape-mismatched 200 answers the config fetch, without throwing', async () => {
    // The reference-app-web acceptance guard for the config render: the
    // Public-config wire shape is a hand-maintained seam (no spec
    // fragment exists for either config endpoint), and a 200 whose body
    // is not that shape -- an empty {} document, with no `config`
    // member at all -- must read as "no brand configured" and render
    // the fallback. Before the null-guard this threw a TypeError while
    // rendering the AppBar brand on every page: a shape-mismatched 200
    // white-screened the app, and no error boundary exists below this
    // hook to catch it.
    const server = demoServer({
      publicConfig: { config: { 'brand.site_name': 'Demo Smile Lab' }, features: [] },
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/config/public') {
        return jsonResponse(200, {})
      }
      return server(call)
    })
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    })
    expect(await findByText(zhCN.brand.fallback)).toBeInTheDocument()
  })

  it('shares one config fetch across every consumer on the page', async () => {
    const rig = makeRealClientRig(
      demoServer({
        publicConfig: {
          config: { 'brand.site_name': 'Demo Smile Lab' },
          features: [],
        },
      }),
    )
    const { findAllByText } = renderWithAppServices(
      <>
        <BrandProbe />
        <BrandProbe />
      </>,
      { session: rig.session, api: rig.api },
    )
    expect((await findAllByText('Demo Smile Lab')).length).toBe(2)
    expect(CONFIG_GETS(rig.calls)).toBe(1)
  })

  it('renders the fallback in the active language', async () => {
    const rig = makeRealClientRig(demoServer())
    const { findByText } = renderWithAppServices(<BrandProbe />, {
      session: rig.session,
      api: rig.api,
    }, { language: 'en-US' })
    expect(await findByText(enUS.brand.fallback)).toBeInTheDocument()
  })
})
