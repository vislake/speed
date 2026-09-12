/**
 * gated-read.tsx -- the read gate every query-driven work surface
 * mounts: one classification from a surface's read states to the
 * decision layout-kit's RouteGuard renders, and the wrapper that mounts
 * the gate's three suits.
 *
 * The classification is error-state first, because the error state is
 * where every failure of a read lands, whatever it carried: only the
 * rbac read gate's own refusal (its code, READ_DENIED_CODE) is an
 * authorization fact and maps to 'denied'; every other failed read --
 * a coded transport answer or server 5xx, or a refusal that carries no
 * code at all (a raw AbortError, an error thrown before the api-client
 * could normalize it) -- is a load failure. Classifying by the error's
 * code alone would fail open on that last kind: tanstack query v5
 * never clears a query's `data` on a failed refetch (the last
 * successful answer stays cached while the retry runs), so a codeless
 * refusal would slip past the code check into the stale rows, or,
 * with no data ever served, park at 'pending' forever instead of
 * rendering a failure. Checking `isError` ahead of anything else
 * closes both: a surface is 'allowed' only once every read has
 * answered with no error standing, and no answer at all yet is
 * 'pending'.
 *
 * GatedContent mounts the two suits those facts select. A failed read
 * renders ui-kit's error empty state -- never the no-permission one (a
 * down server is not a permission problem, and a user told they are
 * forbidden while the server is failing reads like a misconfiguration
 * to the operator who must fix it) -- and the gated content renders
 * inside the RouteGuard, whose denied suit is the no-permission empty
 * state. Both placeholders sit at h2, below the page's own h1, so the
 * page's heading order does not change with the state: the gate mounts
 * under the surface's heading, as every one of its callers does.
 *
 * The denial code is the rbac layer's 403 (`rbac.permission_denied`),
 * the only list-read answer that is an authorization fact from every
 * surface this gate serves.
 */

import type { ReactElement, ReactNode } from 'react'
import { RouteGuard } from '@speed/layout-kit'
import type { RouteGuardStatus } from '@speed/layout-kit'
import { EmptyState } from '@speed/ui-kit'
import { apiErrorCodeOf } from './cases-errors.js'

/** The read gate's own refusal code: the rbac layer's 403, the only
 * read answer that is an authorization fact (never a transport answer,
 * never a server 5xx). */
const READ_DENIED_CODE = 'rbac.permission_denied'

/** The read-state facts one query contributes to a surface's gate. A
 * generated hook's result satisfies this structurally; a surface whose
 * gate stands on several reads passes them all, in the order whose
 * failure the gate should report. */
export interface GatedReadFacts {
  readonly isError: boolean
  readonly error: unknown
  readonly data: unknown
}

/** One surface's read gate: whether a read failed, whether that
 * failure is the rbac refusal, and the RouteGuard status they select. */
export interface GatedRead {
  /** Whether any of the reads is in an error state. */
  readonly readFailed: boolean
  /** Whether the failure is the rbac read gate's own refusal. */
  readonly gateDenied: boolean
  /** The gate's status: denied, else allowed once every read has
   * answered, else pending. */
  readonly gateStatus: RouteGuardStatus
}

/**
 * The read gate over one surface's reads (see the file header): the
 * first failing read's code decides the denial, and the gate is
 * 'allowed' only when no read is failing and every read has answered.
 */
export function gatedRead(...reads: readonly GatedReadFacts[]): GatedRead {
  const failed = reads.find((read) => read.isError)
  const gateDenied =
    failed !== undefined && apiErrorCodeOf(failed.error) === READ_DENIED_CODE
  const gateStatus: RouteGuardStatus = gateDenied
    ? 'denied'
    : reads.every((read) => read.data !== undefined)
      ? 'allowed'
      : 'pending'
  return {
    readFailed: failed !== undefined,
    gateDenied,
    gateStatus,
  }
}

export interface GatedContentProps {
  /** The surface's gate, from gatedRead over its reads. */
  readonly gate: GatedRead
  /** The gated content: rendered only while the gate is 'allowed'. */
  readonly children: ReactNode
}

/**
 * The gate's two suits and the content between them (see the file
 * header): a read failure that is not the rbac refusal renders the
 * error empty state, everything else renders the gate over the
 * children.
 */
export function GatedContent({
  gate,
  children,
}: GatedContentProps): ReactElement {
  if (gate.readFailed && !gate.gateDenied) {
    return <EmptyState variant="error" headingLevel="h2" />
  }
  return (
    <RouteGuard
      status={gate.gateStatus}
      deniedFallback={<EmptyState variant="noPermission" headingLevel="h2" />}
    >
      {children}
    </RouteGuard>
  )
}
