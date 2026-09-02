/**
 * Hand-authored geometric icons for EmptyState's built-in variants.
 *
 * Drawn as 24px-grid strokes in currentColor so they follow whatever
 * text color the surrounding theme paints -- no icon-font dependency,
 * no MUI icons package. The icons are decorative by contract: the
 * neighbouring title and description carry the meaning, so every icon
 * renders aria-hidden and focusable=false.
 */

interface IconProps {
  /** Box length in px; stroke geometry is sized for the 24 grid. */
  readonly size?: number
}

export function EmptyBoxIcon({ size = 40 }: IconProps) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <rect x="4" y="5" width="16" height="14" rx="2.5" />
      <path d="M8.5 9.5h7" />
      <path d="M8.5 13h7" />
      <path d="M8.5 16.5h4" />
    </svg>
  )
}

export function LockIcon({ size = 40 }: IconProps) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <rect x="6" y="10.5" width="12" height="9.5" rx="2.5" />
      <path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5" />
      <circle cx="12" cy="15" r="0.6" fill="currentColor" stroke="none" />
    </svg>
  )
}

export function ErrorIcon({ size = 40 }: IconProps) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
    >
      <circle cx="12" cy="12" r="8.5" />
      <path d="M12 7.6v5.6" />
      <path d="M12 16.9h0.01" strokeWidth={2.4} />
    </svg>
  )
}
