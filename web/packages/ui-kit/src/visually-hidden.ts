/**
 * The visually-hidden recipe: the one style object for elements that
 * must stay in the accessibility tree while taking no painted space.
 *
 * The recipe is the clip technique, never `display: none`: a
 * display:none live region is not live (it announces nothing when its
 * text changes) and a display:none input is not focusable (it leaves
 * the tab order). Clipping keeps both properties -- the element keeps
 * its place in the accessibility tree and, for a focusable element, in
 * the tab order -- while the 1x1 box paints nothing.
 *
 * One shared recipe serves everything that hides this way: the kit's
 * own hidden live regions and file input, and each consuming package's
 * hidden live regions (the surface families mount their own for their
 * own announcements). A single exported object is what lets a fix to
 * the recipe -- a clip form a browser drops, the 1px box that would
 * otherwise scroll-anchor -- land in every hidden element at once
 * instead of in one of a dozen hand-copied property sets.
 */

/**
 * The recipe object, usable wherever MUI accepts styles: the `sx` prop
 * of a Box or Alert, or the argument of a `styled()` call, which is how
 * the kit's hidden file input takes it.
 */
export const visuallyHiddenSx = {
  clip: 'rect(0 0 0 0)',
  clipPath: 'inset(50%)',
  height: 1,
  width: 1,
  overflow: 'hidden',
  position: 'absolute',
  bottom: 0,
  left: 0,
  whiteSpace: 'nowrap',
  margin: 0,
} as const
