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
 */

import type { ReactNode } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useUiKitTranslation } from '../internal/translation.js'
import { EmptyBoxIcon, ErrorIcon, LockIcon } from '../internal/icons.js'

export type EmptyStateVariant = 'empty' | 'noPermission' | 'error'

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
      <Typography variant="h6">{title ?? t(`emptyState.${variant}.title`)}</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ maxWidth: '46ch' }}>
        {description ?? t(`emptyState.${variant}.description`)}
      </Typography>
      {action !== undefined && <Box sx={{ marginTop: 1 }}>{action}</Box>}
    </Box>
  )
}
