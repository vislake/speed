/**
 * use-content-object-url.ts -- the object URL of one content read's
 * bytes, the hook every surface that renders read bytes goes through
 * (the case photo, the simulation comparison's result image, the
 * download control).
 *
 * The effect owns exactly the URL it created: the URL is created when
 * the bytes arrive, and revoked when they are replaced or the consumer
 * leaves the page, because revoking the previous URL is the cleanup of
 * the previous effect run -- so a session that renders many photos or
 * simulations never leaks one.
 *
 * The return is null until the bytes have arrived: the caller renders
 * its own placeholder (an empty frame, no control) rather than an
 * image or a download of nothing.
 */

import { useEffect, useState } from 'react'
import { base64ToBytes } from './base64.js'

/** One content read's payload: the stored bytes, base64-encoded in
 * JSON, and the media type storage's probe assigned them (the shape
 * both the photo-content and the simulation-content operations
 * answer). */
export interface ContentBytes {
  readonly content_base64: string
  readonly media_type: string
}

/**
 * The object URL of the given content read's bytes, or null while
 * there are none (see the file header). The argument is the generated
 * content hook's `data` -- the value the effect is keyed on, so a
 * refetched answer re-creates the URL and revokes the previous one.
 */
export function useContentObjectUrl(
  content: ContentBytes | undefined,
): string | null {
  const [objectUrl, setObjectUrl] = useState<string | null>(null)
  useEffect(() => {
    if (content === undefined) {
      return
    }
    const bytes = base64ToBytes(content.content_base64)
    const url = URL.createObjectURL(
      new Blob([bytes], { type: content.media_type }),
    )
    setObjectUrl(url)
    return () => {
      URL.revokeObjectURL(url)
    }
  }, [content])
  return objectUrl
}
