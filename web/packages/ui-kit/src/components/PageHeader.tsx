/**
 * PageHeader: the single page-level heading block.
 *
 * Renders the page title as a semantic h1 (one PageHeader per page --
 * the h1 contract is the caller's; this component just makes it easy)
 * in the heading typography role, an optional breadcrumb trail above
 * the title, an optional description in secondary body text, and an
 * optional action area (buttons, menus) on the end.
 *
 * Visible text here is caller content -- it renders the host's own
 * translations, so every crumb label, title and description is the
 * host's translation surface. The only built-in strings are
 * accessibility glue taken from the ui-kit namespace: the accessible
 * name of the breadcrumb navigation landmark, and the label of the
 * collapse-expand button MUI's Breadcrumbs shows once a trail exceeds
 * eight crumbs (MUI's stock label there is an English literal, so it
 * must never ship unreplaced).
 */

import type { MouseEventHandler, ReactNode } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Breadcrumbs from '@mui/material/Breadcrumbs'
import Link from '@mui/material/Link'
import Typography from '@mui/material/Typography'
import { useUiKitTranslation } from '../internal/translation.js'

/**
 * One breadcrumb step: a label plus optional link behaviour.
 *
 * The label is host content -- render it already translated. A crumb
 * with `href` renders as a link (attach navigation-interception
 * handlers through `onClick`); a crumb without `href` renders as
 * plain text and is never interactive. The last crumb stands for the
 * current page and is marked `aria-current="page"` whether it links
 * or not.
 */
export interface PageHeaderBreadcrumb {
  /** The crumb's text; host content, already translated. */
  readonly label: ReactNode
  /**
   * The crumb's destination. Set to render the crumb as a link;
   * absent, the crumb renders as plain text.
   */
  readonly href?: string
  /** Click handler for link crumbs; runs when the crumb link is clicked. */
  readonly onClick?: MouseEventHandler<HTMLAnchorElement>
}

export interface PageHeaderProps {
  /** The page title; rendered as an h1 in the heading-4 role. */
  readonly title: ReactNode
  /** Optional supporting paragraph under the title. */
  readonly description?: ReactNode
  /**
   * Optional breadcrumb trail, from the top level down to the current
   * page, rendered above the title inside a navigation landmark whose
   * accessible name comes from the ui-kit namespace (see
   * PageHeaderBreadcrumb for the link contract).
   */
  readonly breadcrumbs?: readonly PageHeaderBreadcrumb[]
  /** Optional trailing actions (primary buttons, menus). */
  readonly actions?: ReactNode
  /** Extra styling applied to the header box. */
  readonly sx?: SxProps<Theme>
}

/**
 * The page heading block: optional breadcrumb trail, then title,
 * optional description and trailing actions on one baseline-spaced row.
 */
export function PageHeader({ title, description, actions, breadcrumbs, sx }: PageHeaderProps) {
  const { t } = useUiKitTranslation()
  const crumbs = breadcrumbs ?? []
  const lastCrumbIndex = crumbs.length - 1
  return (
    <Box component="header" sx={sx}>
      {crumbs.length > 0 && (
        <Breadcrumbs
          aria-label={t('pageHeader.breadcrumbNav')}
          // The namespace wording keeps the collapse-expand button
          // bilingual; MUI's stock 'Show path' literal never ships.
          expandText={t('pageHeader.showFullPath')}
          sx={{ marginBottom: 1.5 }}
        >
          {crumbs.map((crumb, index) => {
            const isCurrentPage = index === lastCrumbIndex
            const currentMark = isCurrentPage ? ('page' as const) : undefined
            if (crumb.href !== undefined) {
              return (
                <Link
                  key={index}
                  href={crumb.href}
                  onClick={crumb.onClick}
                  aria-current={currentMark}
                >
                  {crumb.label}
                </Link>
              )
            }
            return (
              <span key={index} aria-current={currentMark}>
                {crumb.label}
              </span>
            )
          })}
        </Breadcrumbs>
      )}
      <Box
        sx={{
          display: 'flex',
          alignItems: 'flex-start',
          justifyContent: 'space-between',
          gap: 2,
          flexWrap: 'wrap',
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
    </Box>
  )
}
