/**
 * gated-read.test.tsx -- the read gate's contract: the classification
 * (an unanswered read is pending, a served read is allowed, the rbac
 * refusal is denied, and every other error state -- coded or codeless
 * -- is a read failure that still fails the gate closed), and the two
 * suits GatedContent mounts for those facts.
 *
 * The component cases assert the rendered state, not the props: the
 * error suit is ui-kit's error empty state and never the no-permission
 * one (a down server is not a permission problem), the no-permission
 * suit is the denied state, and the gated content appears only while
 * the gate is allowed.
 */

import { describe, expect, it } from 'vitest'
import uiKitZhCN from '../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import { GatedContent, gatedRead } from './gated-read.js'
import { renderWithProviders } from './test-utils/render.js'

/** One read's facts, as a generated hook's result carries them. */
function read(state: {
  isError?: boolean
  error?: unknown
  data?: unknown
}): { isError: boolean; error: unknown; data: unknown } {
  return {
    isError: state.isError ?? false,
    error: state.error ?? null,
    data: state.data,
  }
}

describe('gatedRead', () => {
  it('allows the gate once every read has answered with no error', () => {
    expect(gatedRead(read({ data: { notes: [] } }))).toEqual({
      readFailed: false,
      gateDenied: false,
      gateStatus: 'allowed',
    })
  })

  it('keeps the gate pending while no answer has arrived', () => {
    expect(gatedRead(read({}))).toEqual({
      readFailed: false,
      gateDenied: false,
      gateStatus: 'pending',
    })
    // One leg unanswered is enough to hold the gate.
    expect(
      gatedRead(read({ data: { rows: [] } }), read({})).gateStatus,
    ).toBe('pending')
  })

  it('denies the gate on the rbac refusal', () => {
    expect(
      gatedRead(
        read({ isError: true, error: { code: 'rbac.permission_denied' } }),
      ),
    ).toEqual({
      readFailed: true,
      gateDenied: true,
      gateStatus: 'denied',
    })
  })

  it('fails closed on a coded failure that is not the refusal', () => {
    const gate = gatedRead(
      read({ isError: true, error: { code: 'org.internal_error' } }),
    )
    expect(gate.readFailed).toBe(true)
    expect(gate.gateDenied).toBe(false)
    expect(gate.gateStatus).toBe('pending')
  })

  it('fails closed on a failure that carries no code at all', () => {
    const gate = gatedRead(read({ isError: true, error: new Error('boom') }))
    expect(gate.readFailed).toBe(true)
    expect(gate.gateDenied).toBe(false)
  })

  it('never lets a failed read open on another leg answer, even a stale one', () => {
    // The refused leg still holds an earlier read's data: the error
    // state wins, never the stale rows (the file header's ordering).
    const refused = gatedRead(
      read({
        isError: true,
        error: { code: 'rbac.permission_denied' },
        data: { notes: [{ id: 'stale' }] },
      }),
      read({ data: { notes: [] } }),
    )
    expect(refused.gateDenied).toBe(true)
    expect(refused.gateStatus).toBe('denied')
  })

  it('reports the first failing read in the order given', () => {
    const gate = gatedRead(
      read({ isError: true, error: { code: 'rbac.permission_denied' } }),
      read({ isError: true, error: { code: 'org.internal_error' } }),
    )
    expect(gate.gateDenied).toBe(true)
  })
})

describe('GatedContent', () => {
  const allowed = gatedRead(read({ data: { rows: [] } }))

  it('renders the gated content while the gate is allowed', () => {
    const view = renderWithProviders(
      <GatedContent gate={allowed}>
        <p>the gated content</p>
      </GatedContent>,
    )
    expect(view.getByText('the gated content')).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.error.title),
    ).not.toBeInTheDocument()
  })

  it('renders the error suit for a read failure that is not the refusal', () => {
    const view = renderWithProviders(
      <GatedContent
        gate={gatedRead(read({ isError: true, error: new Error('boom') }))}
      >
        <p>the gated content</p>
      </GatedContent>,
    )
    expect(
      view.getByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(view.queryByText('the gated content')).not.toBeInTheDocument()
  })

  it('renders the no-permission suit for the rbac refusal', () => {
    const view = renderWithProviders(
      <GatedContent
        gate={gatedRead(
          read({ isError: true, error: { code: 'rbac.permission_denied' } }),
        )}
      >
        <p>the gated content</p>
      </GatedContent>,
    )
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.error.title),
    ).not.toBeInTheDocument()
    expect(view.queryByText('the gated content')).not.toBeInTheDocument()
  })

  it('renders neither suit while the gate is pending', () => {
    const view = renderWithProviders(
      <GatedContent gate={gatedRead(read({}))}>
        <p>the gated content</p>
      </GatedContent>,
    )
    expect(view.queryByText('the gated content')).not.toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.error.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
  })
})
