/**
 * share-api.ts -- the app's hand-written half of go/sharing's HTTP
 * surface: the two paths and the one typed call the block-C surfaces
 * need. go/sharing's fragment joins the merged document and the
 * generated @speed/api-sdk like every platform module's does; this app
 * reaches both routes through the api-client RequestFn the host bound,
 * the same seam every generated call travels. The path literals are
 * hand-kept in step with the module's own exported Go constants
 * (sharing.PathShares and sharing.PathAccess, go/sharing/module.go),
 * exactly as @speed/api-client's config fetchers hand-keep the two
 * pre-auth config paths.
 *
 * The wire shapes below mirror go/sharing/api/openapi.yaml's
 * SharingCreateShareRequest / SharingCreateShareResponse schemas,
 * field-for-field, never the generator's Go types.
 */

import type { RequestFn } from '@speed/api-client'

/** POST /api/v1/sharing/shares -- sharing.PathShares. */
export const SHARE_CREATE_PATH = '/api/v1/sharing/shares'
/** GET /api/v1/sharing/access -- sharing.PathAccess. */
export const SHARE_ACCESS_PATH = '/api/v1/sharing/access'

/** The created share's metadata, as its owner sees it (the spec's
 * SharingShare schema; the token never appears in any later read). */
export interface CreatedShare {
  readonly id: string
  readonly resourceRef: string
  /** When this share stops being accessible. Never absent. */
  readonly expiresAt: string
  readonly viewCount: number
  readonly passwordProtected: boolean
  readonly sensitive: boolean
  readonly revokedAt: string | null
  readonly createdAt: string
}

/** The create operation's 201 answer: the share, plus its bearer token
 * -- returned exactly once, in this response only. */
export interface CreateShareResponse {
  readonly share: CreatedShare
  readonly token: string
}

/**
 * Creates a share link for resourceRef in the caller's (the signed-in
 * clinic user's) tenant, through the app's one RequestFn. No explicit
 * expiry is sent: the server resolves the tenant's forced default, and
 * a never-expiring request is refused by the module outright. A refusal
 * rejects an ApiError carrying the envelope's code.
 */
export async function createPatientShare(
  api: RequestFn,
  resourceRef: string,
): Promise<CreateShareResponse> {
  return api<CreateShareResponse>(SHARE_CREATE_PATH, {
    method: 'POST',
    body: { resourceRef },
  })
}

/**
 * Revokes one of this tenant's shares by its owner-facing id (POST
 * /api/v1/sharing/shares/{shareId}/revoke). The block-C share action
 * uses it as its one compensation leg: when one half of a minted
 * before/after pair cannot be created, the half that succeeded is
 * revoked again so a failed share action never leaves a live,
 * unhanded-out link behind. Best-effort by contract -- the caller
 * never shows the answer to a person (the mint's own refusal is what
 * the surface renders); a revoke that itself fails leaves the orphan
 * share to the module's own default-expiry sweep, which is the same
 * end every unopened share meets.
 */
export async function revokePatientShare(
  api: RequestFn,
  shareId: string,
): Promise<void> {
  await api<unknown>(`${SHARE_CREATE_PATH}/${encodeURIComponent(shareId)}/revoke`, {
    method: 'POST',
  })
}
