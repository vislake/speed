/**
 * Hand-authored geometric icons for AppShell's chrome.
 *
 * Drawn as 24px-grid strokes in currentColor so they follow whatever
 * text color the surrounding theme paints -- no icon-font dependency,
 * no MUI icons package (following ui-kit's internal/icons.tsx
 * convention). The icon is decorative by contract: the enclosing
 * IconButton carries the accessible name (from the layout-kit
 * namespace), so the icon itself renders aria-hidden and
 * focusable=false.
 */

interface IconProps {
  /** Box length in px; stroke geometry is sized for the 24 grid. */
  readonly size?: number
}

/** The nav-toggle (hamburger) icon shown when the mobile drawer is closed. */
export function MenuIcon({ size = 24 }: IconProps) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M4 6.5h16" />
      <path d="M4 12h16" />
      <path d="M4 17.5h16" />
    </svg>
  )
}

/** The close (X) icon shown when the mobile drawer is open, in the same slot as MenuIcon. */
export function CloseIcon({ size = 24 }: IconProps) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M6 6l12 12" />
      <path d="M18 6L6 18" />
    </svg>
  )
}
