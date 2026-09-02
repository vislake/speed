/**
 * PageHeader: the single page-level heading block.
 *
 * Renders the page title as a semantic h1 (one PageHeader per page --
 * the h1 contract is the caller's; this component just makes it easy)
 * in the heading typography role, an optional description in secondary
 * body text, and an optional action area (buttons, menus) on the end.
 *
 * Text here is caller content -- it renders the host's own
 * translations, so PageHeader carries no built-in strings and no
 * ui-kit namespace dependency.
 */

import type { ReactNode } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'

export interface PageHeaderProps {
  /** The page title; rendered as an h1 in the heading-4 role. */
  readonly title: ReactNode
  /** Optional supporting paragraph under the title. */
  readonly description?: ReactNode
  /** Optional trailing actions (primary buttons, menus). */
  readonly actions?: ReactNode
  /** Extra styling applied to the header box. */
  readonly sx?: SxProps<Theme>
}

/**
 * The page heading block: title, optional description and trailing
 * actions on one baseline-spaced row.
 */
export function PageHeader({ title, description, actions, sx }: PageHeaderProps) {
  return (
    <Box
      component="header"
      sx={{
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'space-between',
        gap: 2,
        flexWrap: 'wrap',
        ...sx,
      }}
    >
      <Box sx={{ minWidth: 0 }}>
        <Typography component="h1" variant="h4">
          {title}
        </Typography>
        {description !== undefined && (
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ marginTop: 0.5, maxWidth: '70ch' }}
          >
            {description}
          </Typography>
        )}
      </Box>
      {actions !== undefined && (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexShrink: 0 }}>
          {actions}
        </Box>
      )}
    </Box>
  )
}
