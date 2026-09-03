/**
 * File-input and drag-and-drop test helpers for the jsdom environment.
 *
 * jsdom ships neither `DataTransfer` nor `DragEvent`, so the canonical
 * user-event recipes would throw. @testing-library/dom's `fireEvent`
 * works around both gaps: `fireEvent.change` copies the passed `files`
 * onto the input node itself (the source defines the property directly
 * on the node, since `input.files` is read-only in real browsers), and
 * `fireEvent.drop` copies the passed `dataTransfer` object onto the
 * created event (its own comment links jsdom issue #1568). These helpers
 * centralise that mechanism so tests never re-derive it; every file
 * selection and drop in the suite goes through them.
 */

import { fireEvent } from '@testing-library/react'

/**
 * Select `files` through the input, exactly as a native picker would
 * deliver them: dispatches a `change` event whose target carries the
 * given file list.
 */
export function selectFiles(input: HTMLInputElement, files: readonly File[]): void {
  fireEvent.change(input, { target: { files } })
}

/**
 * Drop `files` onto `target`, exactly as a native drag-and-drop would:
 * dispatches a `drop` event carrying the given files in its
 * `dataTransfer`.
 */
export function dropFiles(target: Element, files: readonly File[]): void {
  fireEvent.drop(target, { dataTransfer: { files } } as Parameters<
    typeof fireEvent.drop
  >[1])
}
