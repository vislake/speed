/**
 * EmptyState: the three stock empty/blocked/error placeholders.
 *
 * Variants are 'empty' (a space has no data yet), 'noPermission' (the
 * viewer is not allowed to see the content) and 'error' (the content
 * failed to load). Every variant ships built-in bilingual title and
 * description text from the ui-kit namespace (see the resource table in
 * the README); title, description and action are overridable props, and
 * the icon can be swapped wholesale. When a variant's text does not fit
 * the situation (an error with its own retry story), pass the props --
 * the namespace defaults are only ever the fallback.
 *
 * The icon is decorative by contract (aria-hidden): the text carries
 * the meaning, so a custom icon prop should stay decorative too, or
 * come with its own accessible label.
 *
 * The title renders as a REAL heading element (an `h1`-`h6`, never a
 * styled `<div>`), because EmptyState is routinely the only content of
 * a section whose own heading it stands in for (see, for one, every
 * account-ui section's empty/error branch). That heading's LEVEL is
 * therefore page-structure knowledge EmptyState cannot supply on its
 * own -- only the host knows what heading, if any, precedes it on the
 * real page -- so `headingLevel` is a required-in-spirit prop a host
 * embedding this under a real h1/h2 MUST set to the level that
 * continues the page's own order without a skip; it defaults to 'h6'
 * for backward compatibility, not because 'h6' is usually correct.
 * The visual size stays the fixed `variant="h6"` look regardless of
 * `headingLevel` -- MUI's
 * `component` override is what lets the semantic level and the visual
 * style vary independently, the same split this package's other
 * multi-level headings (PageHeader's h1, a section's own h2) already
 * rely on.
 */

import type { ReactNode } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useUiKitTranslation } from '../internal/translation.js'
import { EmptyBoxIcon, ErrorIcon, LockIcon } from '../internal/icons.js'

export type EmptyStateVariant = 'empty' | 'noPermission' | 'error'

/** The semantic heading levels EmptyState's title can render as. */
export type EmptyStateHeadingLevel = 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6'

export interface EmptyStateProps {
  /** Which stock placeholder to render; defaults to 'empty'. */
  readonly variant?: EmptyStateVariant
  /** Overrides the variant's built-in title. */
  readonly title?: ReactNode
  /** Overrides the variant's built-in description. */
  readonly description?: ReactNode
  /** Optional action (a Button, a link); rendered under the description. */
  readonly action?: ReactNode
  /** Replaces the variant's stock icon. Keep it decorative (see header note). */
  readonly icon?: ReactNode
  /**
   * The real heading element (`h1`-`h6`) the title renders as. Set this
   * to whatever level continues the real page's own heading order at
   * the point this EmptyState appears -- see this file's header comment.
   * Defaults to 'h6' for backward compatibility.
   */
  readonly headingLevel?: EmptyStateHeadingLevel
  /** Extra styling applied to the placeholder box. */
  readonly sx?: SxProps<Theme>
}

const VARIANT_ICONS = {
  empty: EmptyBoxIcon,
  noPermission: LockIcon,
  error: ErrorIcon,
} as const

/**
 * The stock placeholder block: centered icon, title and description
 * from the ui-kit namespace unless overridden, optional action slot.
 */
export function EmptyState({
  variant = 'empty',
  title,
  description,
  action,
  icon,
  headingLevel = 'h6',
  sx,
}: EmptyStateProps) {
  const { t } = useUiKitTranslation()
  const StockIcon = VARIANT_ICONS[variant]
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        textAlign: 'center',
        gap: 1.5,
        paddingY: 6,
        paddingX: 3,
        ...sx,
      }}
    >
      <Box sx={{ color: 'text.secondary' }}>{icon ?? <StockIcon />}</Box>
      <Typography variant="h6" component={headingLevel}>
        {title ?? t(`emptyState.${variant}.title`)}
      </Typography>
      <Typography variant="body2" color="text.secondary" sx={{ maxWidth: '46ch' }}>
        {description ?? t(`emptyState.${variant}.description`)}
      </Typography>
      {action !== undefined && <Box sx={{ marginTop: 1 }}>{action}</Box>}
    </Box>
  )
}
