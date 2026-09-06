/**
 * Reads the CSS text emotion (MUI's styling engine) has injected into
 * the document for the current render, across every <style> tag it
 * owns.
 *
 * MUI's `sx` prop compiles most values into emotion-generated
 * stylesheet rules rather than an inline `style` attribute, and a
 * breakpoint-keyed value (e.g. `{ xs: '1fr', sm: 'repeat(2, 1fr)' }`)
 * compiles to a base rule plus an `@media` rule. jsdom evaluates
 * neither real layout nor `@media` conditions, so `getComputedStyle`
 * (the basis of jest-dom's `toHaveStyle`) cannot see which side of a
 * breakpoint is "active" -- there is no real viewport for either side
 * to be active at. Reading the actual generated CSS text instead is a
 * property/snapshot proof that the intended declaration (base value
 * *and* its `@media` guard) was wired into the page; it does not prove
 * the layout looks correct at any real viewport width, which jsdom
 * cannot render at all.
 */
export function emittedStyleText(): string {
  return Array.from(document.querySelectorAll('style'))
    .map((style) => style.textContent ?? '')
    .join('\n')
}
