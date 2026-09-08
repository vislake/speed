/**
 * notes-draft.ts -- the in-memory home of the notes create form's
 * half-typed text, between NotesView mounts.
 *
 * Why the draft outlives the view: the notes surface unmounts whenever
 * the frame moves to another hash route (NotesView is a child of the
 * route switch in app.tsx), and a clinician who leaves a half-typed
 * patient note to check another surface -- and comes back with the
 * browser's Back button, the most reflexive action there is -- must
 * find the text where they left it. A hash navigation never reloads
 * the document, so the half-typed text is expected to survive it; the
 * form's own state cannot, unmounting with the view, so the store
 * carries it across mounts. The page's own memory is the right level
 * for the draft for the same reason the session is memory-only:
 * nothing here writes storage, so a reload starts over -- but a reload
 * is not what a hash navigation is, and within the life of one
 * document the half-typed record survives every surface switch.
 *
 * The store is deliberately dumb: a module-scoped string with three
 * functions, read at form mount (useForm's defaultValues), written on
 * every change (the view mirrors the live form state into it), and
 * cleared when the draft can no longer belong to the person who typed
 * it:
 *
 *  - a successful create -- the text became a note, and the create
 *    path turns the form over (notes-view.tsx's handleCreate);
 *  - a session end -- the next account signing into this page must
 *    not inherit the departing account's half-typed record, the same
 *    rule main.tsx's evictQueriesOnSessionEnd applies to the query
 *    cache (a draft is the same class of leftover as a cached row);
 *  - a refused create does NOT clear it -- the surface keeps the text
 *    for the retry, exactly as the form itself does while it stays
 *    mounted.
 *
 * A tenant switch deliberately does not clear it: the switch keeps the
 * same person in the same mounted frame (the surface does not remount
 * across a switch), so the form's own state already carries the text
 * across it -- clearing only the store would make the two disagree
 * about what the next mount should restore.
 */

/** The half-typed text. Module-scoped by design: the store must
 * survive NotesView unmounts, and React state cannot. */
let draftText = ''

/** The current draft: the text the next NotesView mount seeds its
 * create form with. */
export function readNotesDraft(): string {
  return draftText
}

/** Replaces the stored draft. The view mirrors every change of its
 * live form into the store through this function. */
export function writeNotesDraft(text: string): void {
  draftText = text
}

/** Discards the draft. Called by the create path after a successful
 * create and by the session-end eviction (see the file header). */
export function clearNotesDraft(): void {
  draftText = ''
}
