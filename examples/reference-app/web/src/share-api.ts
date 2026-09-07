/**
 * share-api.ts -- the app's hand-written half of go/sharing's HTTP
 * surface: the two paths and the one typed call the block-C surfaces
 * need. go/sharing's OpenAPI fragment ships a backend leg only -- it is
 * deliberately not part of the merged application document that drives
 * @speed/api-sdk (go/sharing/AGENTS.md's own note), so no generated
 * operation exists for either route and the app reaches them through
 * the api-client RequestFn the host bound, the same seam every
 * generated call travels. The path literals are hand-kept in step with
 * the module's own exported Go constants (sharing.PathShares and
 * sharing.PathAccess, go/sharing/module.go) exactly as @speed/api-client's
 * config fetchers hand-keep the two pre-auth config paths -- the
 * no-generated-surface mirror of that same discipline.
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
