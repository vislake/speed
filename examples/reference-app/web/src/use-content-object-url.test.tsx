/**
 * use-content-object-url.test.tsx -- the hook's contract: no URL before
 * the bytes arrive, one URL per answer (built from the decoded bytes
 * and the probe's media type), the previous URL revoked when the answer
 * is replaced, and the last one revoked on unmount -- so a session that
 * renders many contents never leaks a blob URL.
 */

import { renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useContentObjectUrl } from './use-content-object-url.js'

/** The blob-URL stand-ins (jsdom implements neither createObjectURL nor
 * revokeObjectURL): counting doubles recording every blob materialized
 * and every URL revoked. */
function installBlobURLDoubles(): {
  readonly blobs: Blob[]
  readonly revoked: string[]
  restore: () => void
} {
  const originalCreate = URL.createObjectURL
  const originalRevoke = URL.revokeObjectURL
  const blobs: Blob[] = []
  const revoked: string[] = []
  URL.createObjectURL = vi.fn<(blob: Blob) => string>((blob) => {
    blobs.push(blob)
    return `blob:content-${blobs.length}`
  }) as typeof URL.createObjectURL
  URL.revokeObjectURL = vi.fn<(url: string) => void>((url) => {
    revoked.push(url)
  }) as typeof URL.revokeObjectURL
  return {
    blobs,
    revoked,
    restore: () => {
      URL.createObjectURL = originalCreate
      URL.revokeObjectURL = originalRevoke
    },
  }
}

/** The bytes 0x01 0x02 0x03, base64-encoded, with one media type. */
const CONTENT = {
  content_base64: 'AQID',
  media_type: 'image/png',
}

/** The same bytes under a second media type, as a replacement answer. */
const REPLACEMENT = {
  content_base64: 'AQID',
  media_type: 'image/jpeg',
}

let doubles: ReturnType<typeof installBlobURLDoubles> | null = null

afterEach(() => {
  doubles?.restore()
  doubles = null
})

describe('useContentObjectUrl', () => {
  it('has no URL and creates no blob before the bytes arrive', () => {
    doubles = installBlobURLDoubles()

    const { result } = renderHook(() => useContentObjectUrl(undefined))

    expect(result.current).toBeNull()
    expect(doubles.blobs).toHaveLength(0)
  })

  it('creates the URL from the decoded bytes and the answered media type', async () => {
    doubles = installBlobURLDoubles()

    const { result } = renderHook(() => useContentObjectUrl(CONTENT))

    expect(result.current).toBe('blob:content-1')
    expect(doubles.blobs).toHaveLength(1)
    expect(doubles.blobs[0]?.type).toBe('image/png')
    const bytes = new Uint8Array(await doubles.blobs[0]!.arrayBuffer())
    expect(Array.from(bytes)).toEqual([1, 2, 3])
  })

  it('revokes the previous URL when the bytes are replaced', () => {
    doubles = installBlobURLDoubles()

    const { result, rerender } = renderHook(
      ({ content }: { readonly content: typeof CONTENT }) =>
        useContentObjectUrl(content),
      { initialProps: { content: CONTENT } },
    )
    const first = result.current

    rerender({ content: REPLACEMENT })

    expect(result.current).toBe('blob:content-2')
    expect(doubles.revoked).toEqual([first])
    expect(doubles.blobs[1]?.type).toBe('image/jpeg')
  })

  it('revokes the live URL when the consumer unmounts', () => {
    doubles = installBlobURLDoubles()

    const { result, unmount } = renderHook(() => useContentObjectUrl(CONTENT))
    const live = result.current

    unmount()

    expect(doubles.revoked).toEqual([live])
  })
})
