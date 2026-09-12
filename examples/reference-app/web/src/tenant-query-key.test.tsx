/**
 * tenant-query-key.test.tsx -- the tenant-namespaced query-key contract:
 * the key is the shared prefix, the current tenant id and the bare key's
 * own elements; the tenant id is the principal's own claim, null while
 * anonymous (so the caller's `enabled` gate can fail closed) and moved
 * by a tenant switch; and the memo holds one key while neither the
 * tenant nor a stable bare key changes.
 *
 * The session is driven through the demo server's real sign-in, exactly
 * as the app's own bootstrap and the view suites do -- the hook reads
 * the auth-core store, so the tests attach the rig's session the way a
 * host does.
 */

import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { attachSession } from '@speed/auth-core'
import { demoServer } from './test-utils/demo-server.js'
import {
  makeRealClientRig,
  signInWithPassword,
} from './test-utils/real-client.js'
import { TENANT_QUERY_PREFIX, useTenantQueryKey } from './tenant-query-key.js'

describe('useTenantQueryKey', () => {
  it('namespaces the bare key under the prefix and the signed-in tenant', async () => {
    const rig = makeRealClientRig(demoServer())
    // A completed password sign-in lands the demo session in tenant-acme.
    await signInWithPassword(rig)
    attachSession(rig.session)

    const { result } = renderHook(() => useTenantQueryKey(['team']))

    expect(result.current.tenantId).toBe('tenant-acme')
    expect(result.current.queryKey).toEqual([
      TENANT_QUERY_PREFIX,
      'tenant-acme',
      'team',
    ])
  })

  it('carries a null tenant while anonymous, alongside the bare key', () => {
    const rig = makeRealClientRig(demoServer())
    // No sign-in: the attached session is anonymous, the state the
    // hooks report before any authentication.
    attachSession(rig.session)

    const { result } = renderHook(() => useTenantQueryKey(['team']))

    expect(result.current.tenantId).toBeNull()
    expect(result.current.queryKey).toEqual([TENANT_QUERY_PREFIX, null, 'team'])
  })

  it('moves the tenant segment when the principal switches tenants', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    attachSession(rig.session)

    const { result } = renderHook(() => useTenantQueryKey(['team']))
    expect(result.current.queryKey).toEqual([
      TENANT_QUERY_PREFIX,
      'tenant-acme',
      'team',
    ])

    await act(async () => {
      await rig.session.switchTenant('tenant-globex')
    })

    expect(result.current.tenantId).toBe('tenant-globex')
    expect(result.current.queryKey).toEqual([
      TENANT_QUERY_PREFIX,
      'tenant-globex',
      'team',
    ])
  })

  it('keeps one key for one tenant and a stable bare key', () => {
    const rig = makeRealClientRig(demoServer())
    attachSession(rig.session)
    const stableTail = ['admin', 'usage']

    const { result, rerender } = renderHook(() =>
      useTenantQueryKey(stableTail),
    )
    const first = result.current.queryKey

    rerender()
    expect(result.current.queryKey).toBe(first)
  })

  it('rebuilds the key when a bare key element changes', () => {
    const rig = makeRealClientRig(demoServer())
    attachSession(rig.session)

    const { result, rerender } = renderHook(
      ({ caseId }: { readonly caseId: string }) =>
        useTenantQueryKey([caseId]),
      { initialProps: { caseId: 'case-1' } },
    )
    expect(result.current.queryKey).toEqual([
      TENANT_QUERY_PREFIX,
      null,
      'case-1',
    ])

    rerender({ caseId: 'case-2' })
    expect(result.current.queryKey).toEqual([
      TENANT_QUERY_PREFIX,
      null,
      'case-2',
    ])
  })
})
