/**
 * AsyncSection: the four-state branch of a section backed by an
 * asynchronous read.
 *
 * A read that has not answered yet, a settled read that delivered
 * nothing, a settled read that is empty and a settled read with content
 * are the same four states on every list-bearing surface, and each one
 * has a fixed rendering: the loading announcement, a retryable error
 * placeholder, an empty placeholder, or the caller's content. This
 * component owns that mapping, so the surfaces cannot drift into
 * rendering the same state differently.
 *
 * The states are passed in already derived -- `pending` (no answer
 * yet), the settled `payload` (undefined: a failed load, or an answer
 * whose body omitted it) and `empty` (the settled payload counts as
 * empty) -- because only the caller knows how its query's pending flag
 * folds cached data, and which emptiness counts. The content is a
 * render callback receiving the narrowed payload, not a node: a
 * caller's content expression typically maps the payload, which must
 * neither be evaluated nor re-narrowed while the payload is absent.
 * The error and empty placeholders are `EmptyState` with the section's
 * own copy and heading level: a surface whose section header hides
 * while its placeholder shows passes the header's level, so the page's
 * heading order never skips.
 */

import type { ReactNode } from 'react'
import Button from '@mui/material/Button'
import { EmptyState, type EmptyStateHeadingLevel } from './EmptyState.js'

/** The error placeholder's copy and its retry. */
export interface AsyncSectionErrorState {
  /** The retryable failure's title. */
  readonly title: string
  /** The retryable failure's description. */
  readonly description: string
  /** The retry button's label. */
  readonly retryLabel: string
  /** Fired by the retry button. */
  readonly onRetry: () => void
}

/** The empty placeholder's copy. */
export interface AsyncSectionEmptyState {
  /** The empty state's title. */
  readonly title: string
  /** The empty state's description. */
  readonly description: string
}

export interface AsyncSectionProps<Payload> {
  /** No answer yet: renders `loading`. */
  readonly pending: boolean
  /** The settled payload; `undefined` renders the error placeholder
   * (a failed load, or an answer whose body omitted the payload). */
  readonly payload: Payload | undefined
  /** Whether the settled payload renders as empty: takes effect only
   * with `emptyState` provided. */
  readonly empty: boolean
  /** The loading announcement -- a `ListSkeleton` shaped like the rows,
   * or any caller-built node. */
  readonly loading: ReactNode
  /** The retryable error placeholder. */
  readonly errorState: AsyncSectionErrorState
  /** The empty placeholder; omitted by a surface whose payload cannot be
   * empty (a settings document always arrives). */
  readonly emptyState?: AsyncSectionEmptyState
  /** The heading element the placeholders' titles render as; defaults to
   * 'h2', the section-title level the placeholders stand in for. */
  readonly headingLevel?: EmptyStateHeadingLevel
  /** The settled content, built from the narrowed payload. */
  readonly children: (payload: Payload) => ReactNode
}

/** The four-state branch of a section backed by an asynchronous read. */
export function AsyncSection<Payload>({
  pending,
  payload,
  empty,
  loading,
  errorState,
  emptyState,
  headingLevel = 'h2',
  children,
}: AsyncSectionProps<Payload>) {
  if (pending) {
    return loading
  }
  if (payload === undefined) {
    return (
      <EmptyState
        variant="error"
        title={errorState.title}
        description={errorState.description}
        action={
          <Button onClick={errorState.onRetry}>{errorState.retryLabel}</Button>
        }
        headingLevel={headingLevel}
      />
    )
  }
  if (empty && emptyState !== undefined) {
    return (
      <EmptyState
        variant="empty"
        title={emptyState.title}
        description={emptyState.description}
        headingLevel={headingLevel}
      />
    )
  }
  return children(payload)
}
