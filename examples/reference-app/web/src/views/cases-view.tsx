/**
 * cases-view.tsx -- the cases list surface of the reference-app web
 * host: the clinic's case list, read through the generated
 * cases_listCases hook over a tenant-namespaced query key, rendered as
 * a list a practice reads, with a button that opens the one-page case
 * creation flow.
 *
 * The list is the clinic's list, not the caller's: the server answers
 * every case of the current tenant whatever creator each case carries
 * (the product decision the cases fragment records), so the heading is
 * "Cases" -- never "My Cases" -- and the current clinic's name rides
 * in the title (CasesSurfaceHeading). There is no rbac gate on this
 * surface: any authenticated member of the tenant may read it, so a
 * failed read is a load failure (the error empty state), never a
 * denial suit -- and a served list is rendered only once it is served;
 * while it is loading the gate's pending state stands in.
 *
 * The query key is tenant-namespaced (['tenant', tenantId, ...] over
 * the generated bare key) exactly like the notes list's, so a tenant
 * switch can never read the previous tenant's cached cases: user-menu
 * evicts the departing tenant's ['tenant', tenantId] queries on every
 * switch and main.tsx's evictQueriesOnSessionEnd empties the whole
 * cache the moment the session ends. A create that lands navigates
 * back here and remounts the view; with the app's staleTime-0 policy
 * the mount re-reads under the current token, so the new case appears
 * on the list the person just came back to.
 *
 * Each row carries the patient's name, an optional patient reference
 * and the opened-on date (through the shared Intl formatter -- an
 * unparseable value renders as an empty cell). Rows are buttons that
 * report the case id upward (onOpenCase); navigation is the host's
 * answer, exactly as the other surfaces' navigation cues are.
 */

import type { ReactElement } from 'react'
import { useMemo } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import List from '@mui/material/List'
import ListItem from '@mui/material/ListItem'
import ListItemButton from '@mui/material/ListItemButton'
import ListItemText from '@mui/material/ListItemText'
import Typography from '@mui/material/Typography'
import {
  getCasesListCasesQueryKey,
  useCasesListCases,
} from '@speed/api-sdk'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { RouteGuard } from '@speed/layout-kit'
import { EmptyState } from '@speed/ui-kit'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { CasesSurfaceHeading } from './case-surface-heading.js'

/** The cases list page: heading, then the clinic's cases. */
export function CasesView({
  onNewCase,
  onOpenCase,
}: {
  /** The host's answer to the "New case" button: navigate to the create page. */
  readonly onNewCase: () => void
  /** The host's answer to opening one case: navigate to its detail page. */
  readonly onOpenCase: (caseId: string) => void
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null

  // The tenant-namespaced list key (see the file header); a null tenant
  // disables the query and the gate stays pending, failing closed
  // rather than inventing a tenant.
  const casesListKey = useMemo(
    () => ['tenant', tenantId, ...getCasesListCasesQueryKey()],
    [tenantId],
  )
  const casesQuery = useCasesListCases({
    query: { queryKey: casesListKey, enabled: tenantId !== null },
  })

  const formatDate = useDateFormatter(i18n.language)

  // A failed read renders the error state before anything else is
  // consulted (never stale rows, never a permanent pending -- the notes
  // gate's own ordering rule); a served list is allowed; no answer yet
  // is pending.
  const readFailed = casesQuery.isError
  const gateStatus = casesQuery.data !== undefined ? 'allowed' : 'pending'

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <CasesSurfaceHeading title={t('cases.heading')} />
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('cases.intro')}
      </Typography>
      <Button variant="contained" onClick={onNewCase}>
        {t('cases.list.newCase')}
      </Button>
      {readFailed ? (
        // The read failed: the error empty state. Whatever the failure
        // carried (a coded transport answer, a server 5xx, a codeless
        // throw), a failed read renders here, never stale rows.
        <EmptyState variant="error" headingLevel="h2" sx={{ marginTop: 3 }} />
      ) : (
        <RouteGuard status={gateStatus}>
          {casesQuery.data?.cases.length === 0 ? (
            <EmptyState
              variant="empty"
              headingLevel="h2"
              title={t('cases.list.emptyTitle')}
              description={t('cases.list.emptyDescription')}
              sx={{ marginTop: 3 }}
            />
          ) : (
            <List sx={{ marginTop: 1 }}>
              {(casesQuery.data?.cases ?? []).map((record) => {
                const secondary =
                  record.patient_ref.length > 0
                    ? `${record.patient_ref} · ${formatDate(record.created_at)}`
                    : formatDate(record.created_at)
                return (
                  <ListItem key={record.id} disablePadding>
                    <ListItemButton
                      onClick={() => onOpenCase(record.id)}
                      divider
                    >
                      <ListItemText
                        primary={record.patient_name}
                        secondary={secondary}
                      />
                    </ListItemButton>
                  </ListItem>
                )
              })}
            </List>
          )}
        </RouteGuard>
      )}
    </Box>
  )
}
