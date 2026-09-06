/**
 * notes-view.tsx -- the notes surface of the reference-app web host:
 * the list read through the generated notes hooks over a tenant-
 * namespaced query key, gated by layout-kit's RouteGuard with the
 * status derived from that same query, and a create flow driven by the
 * notesCreateNote mutation through ui-kit's FormLayout/FormField.
 *
 * The gate is the list query itself: the server's rbac layer answers a
 * caller without notes:read with 403 rbac.permission_denied, so the
 * query is the real permission fetch -- the router-level RouteGuard
 * behind real fetches that the auth-ui census defers to this shell.
 * The three statuses map onto the query's own states, checked in an
 * order that fails closed: a read error is classified first -- only
 * the rbac read gate's own refusal is an authorization fact and maps
 * to 'denied'; a read that failed for any other reason (a transport
 * answer, a 5xx) is a load failure and renders the ui-kit error empty
 * state in its own suit, never the no-permission one
 * (reference-app-web.md P2-2). Whichever suit, a failed read means no
 * surface, whether or not an earlier read on the very same query
 * already left rows in the cache. tanstack query v5 never clears a
 * query's `data` on a failed refetch (the last successful answer keeps
 * rendering while the retry runs), so checking `data` ahead of
 * `isError` -- this view's original shape -- left a permission revoked
 * mid-session, or a different account signing into a tenant an earlier
 * read already cached, showing the earlier read's stale rows instead
 * of failing closed (reference-app-web.md P1-1). A served list is
 * 'allowed' next, and no answer at all yet is 'pending'.
 * The create form lives inside the allowed branch, and a refused
 * create (a caller without notes:write answers the same 403) stays on
 * the page with its code text -- the write gate is probed by the
 * mutation, never pre-empted client-side.
 *
 * The list query key is tenant-namespaced per the frontend standard
 * (['tenant', tenantId, ...] over the generated bare key) so a tenant
 * switch can never read the previous tenant's cached notes -- user-
 * menu.tsx evicts the departing tenant's ['tenant', tenantId] queries
 * on every switch, and main.tsx's evictTenantQueriesOnSessionEnd evicts
 * the same prefix again the moment the session ends (a sign-out or a
 * session death), so a different account signing into the same tenant
 * afterward starts from an empty cache rather than inheriting rows an
 * earlier session's reads left behind. The create invalidates exactly
 * the namespaced key.
 *
 * Created-at times render through Intl in the surface language (never
 * hand-formatted); an unparseable value renders as an empty cell
 * rather than reaching Intl and throwing.
 */

import type { ReactElement } from 'react'
import { useMemo, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import type { NotesNote } from '@speed/api-sdk'
import {
  getNotesListNotesQueryKey,
  useNotesCreateNote,
  useNotesListNotes,
} from '@speed/api-sdk'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { RouteGuard } from '@speed/layout-kit'
import type { RouteGuardStatus } from '@speed/layout-kit'
import type { DataTableColumn } from '@speed/ui-kit'
import { DataTable, EmptyState, FormField, FormLayout } from '@speed/ui-kit'
import { useForm } from 'react-hook-form'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The notes text limit the server handler enforces (kept in step with
 * the notes module's own limit so a client-side refusal and a server
 * refusal agree). */
const NOTE_TEXT_LIMIT = 4000

/** The create form's one field: the note text. */
interface NoteDraft {
  readonly text: string
}

/** The collapse target below: not a code @speed/api-client ever emits
 * (its failures are client.network / client.timeout / client.protocol /
 * client.http.*), so the resolver treats it as unknown. */
const UNKNOWN_FAILURE_CODE = 'client.unknown'

/** The read gate's own refusal code: the rbac layer's 403, the only
 * list-read answer that is an authorization fact (never a transport
 * answer, never a server 5xx). */
const NOTES_READ_DENIED_CODE = 'rbac.permission_denied'

/**
 * The code of an ApiError-shaped failure, or null for a failure that
 * carries none (a bug-shaped throw, an un-normalized answer).
 */
function apiErrorCodeOf(error: unknown): string | null {
  if (typeof error !== 'object' || error === null) {
    return null
  }
  const code = (error as { code?: unknown }).code
  return typeof code === 'string' && code.length > 0 ? code : null
}

/**
 * The submit path's failure classifier: an ApiError-shaped failure
 * keeps its code, anything else -- a bug-shaped throw, an un-normalized
 * answer -- collapses to a code that is deliberately not whitelisted,
 * so the resolver renders the unknown fallback. A submit that throws at
 * all always has a code to show.
 */
function submitErrorCodeOf(error: unknown): string {
  return apiErrorCodeOf(error) ?? UNKNOWN_FAILURE_CODE
}

/** The reachable codes of a note-create attempt, each mapped to the
 * app-namespace key carrying its current-language text. A create answer
 * that is not on this list (a future server code, a client.http.<status>
 * transport answer) resolves to the unknown fallback, so the surface
 * never shows a raw key or another language's text. Exported for the
 * surface's error-surface whitelist to be deep-imported by the
 * codes-alignment suite -- an import from this view's own module, never
 * from a package entry point (the package boundaries' whitelists are
 * each package's own, and the app's whitelist lives where the app
 * renders it). */
export const NOTE_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  'rbac.permission_denied': 'notes.errors.permissionDenied',
  'notes.text_required': 'notes.errors.textRequired',
  'notes.text_too_long': 'notes.errors.textTooLong',
  'notes.internal_error': 'notes.errors.internalError',
  'client.network': 'notes.errors.client',
  'client.timeout': 'notes.errors.client',
  'client.protocol': 'notes.errors.client',
}

/**
 * The query-driven notes surface: heading, then the gated content --
 * the create form and the served list.
 */
export function NotesView(): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const queryClient = useQueryClient()
  // useCurrentTenant returns { tenantId } | null, not a bare string --
  // the plain string is what the frontend standard's own tenant-
  // namespaced-key convention documents (['tenant', tenantId, ...]) and
  // what user-menu.tsx's tenant-switch eviction and main.tsx's session-
  // end eviction both key their removeQueries call on, so it is
  // extracted here rather than embedding the hook's object wholesale --
  // a previous version of this view did exactly that, which meant
  // every removeQueries call keyed on a bare tenant id could never
  // structurally match the real cached key (['tenant', {tenantId},
  // ...]) and silently evicted nothing (reference-app-web.md P1-1's
  // root cause: without a real eviction, only a query key that itself
  // changes -- as it does on an actual tenant switch -- ever produced
  // fresh data; the same tenant reused across two sessions never did).
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null

  // The tenant-namespaced list key. The view only ever mounts inside
  // the signed-in frame (where the tenant is always present); a null
  // tenant disables the query and the gate stays pending, failing
  // closed rather than inventing a tenant.
  const notesListKey = useMemo(
    () => ['tenant', tenantId, ...getNotesListNotesQueryKey()],
    [tenantId],
  )
  const notesQuery = useNotesListNotes({
    query: { queryKey: notesListKey, enabled: tenantId !== null },
  })

  const createNoteMutation = useNotesCreateNote()
  const createForm = useForm<NoteDraft>({ defaultValues: { text: '' } })
  const [submitErrorCode, setSubmitErrorCode] = useState<string | null>(null)

  // The read error, classified: only the rbac read gate's own 403 is an
  // authorization fact. Every other failure a list read can answer
  // with -- a transport answer (client.network / client.timeout /
  // client.http.<status>), a 5xx the server answered under its own
  // code -- is a load failure and renders the read-error state below,
  // never the no-permission suit (reference-app-web.md P2-2: a down
  // server is not a permission problem, and a user told they are
  // forbidden while the server is failing reads like a
  // misconfiguration to the operator who must fix it).
  const listErrorCode = notesQuery.isError
    ? apiErrorCodeOf(notesQuery.error)
    : null
  const gateDenied = listErrorCode === NOTES_READ_DENIED_CODE

  // The gate: a read error fails it closed first and unconditionally --
  // even when the query still holds an earlier read's data, since
  // tanstack query v5 never clears `data` on a failed refetch (see the
  // file header). Checking `data` before `isError` (this view's
  // original shape) was the fail-open bug reference-app-web.md P1-1
  // named: a permission revoked between two reads, or a different
  // account signing into a tenant an earlier account's read already
  // cached, refetches into a 403 whose stale `data` kept the gate
  // 'allowed' -- so a served list is checked only once no error
  // stands, and no answer at all yet is pending.
  const gateStatus: RouteGuardStatus = gateDenied
    ? 'denied'
    : notesQuery.data !== undefined
      ? 'allowed'
      : 'pending'

  /** Creates the note, then turns the form over and refetches the list
   * so the served answer is the source of the new row. A refused
   * create leaves the draft on the form and shows the answer's code
   * text; an empty or over-long text is the server's refusal to make,
   * never a client-side substitution for it (the required rule here
   * only catches the empty string, exactly as the real handler's trim
   * does not). */
  async function handleCreate(values: NoteDraft): Promise<void> {
    setSubmitErrorCode(null)
    try {
      await createNoteMutation.mutateAsync({ data: { text: values.text } })
    } catch (error) {
      setSubmitErrorCode(submitErrorCodeOf(error))
      return
    }
    createForm.reset()
    await queryClient.invalidateQueries({ queryKey: notesListKey })
  }

  /** Resolves a submit-failure code to its current-language text. */
  function submitErrorText(code: string): string {
    const key = NOTE_ERROR_TEXT_KEYS[code] ?? 'notes.errors.unknown'
    return t(key)
  }

  // Created-at cells render through Intl in the surface language;
  // an unparseable value renders as an empty cell.
  const formatCreatedAt = useMemo(() => {
    const formatter = new Intl.DateTimeFormat(i18n.language, {
      dateStyle: 'medium',
      timeStyle: 'short',
    })
    return (value: string | undefined): string => {
      if (value === undefined) {
        return ''
      }
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [i18n.language])

  const columns: readonly DataTableColumn<NotesNote>[] = useMemo(
    () => [
      {
        id: 'text',
        header: t('notes.create.textLabel'),
        cell: (note) => note.text ?? '',
      },
      {
        id: 'created_at',
        header: t('notes.createdColumn'),
        cell: (note) => formatCreatedAt(note.created_at),
      },
    ],
    [formatCreatedAt, t],
  )

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('notes.heading')}
      </Typography>
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('notes.intro')}
      </Typography>
      {listErrorCode !== null && !gateDenied ? (
        // The read failed for a reason other than authorization: the
        // error empty state in its own suit, not the no-permission one.
        // Both placeholders sit at the same heading level (h2, below
        // this view's h1) so the page's heading order does not change
        // with the state.
        <EmptyState variant="error" headingLevel="h2" />
      ) : (
        <RouteGuard
          status={gateStatus}
          deniedFallback={
            <EmptyState variant="noPermission" headingLevel="h2" />
          }
        >
          <FormLayout
            form={createForm}
            onSubmit={handleCreate}
            actions={
              <Button
                type="submit"
                variant="contained"
                disabled={createForm.formState.isSubmitting}
              >
                {t('notes.create.submit')}
              </Button>
            }
          >
            <FormField
              name="text"
              required
              rules={{
                maxLength: {
                  value: NOTE_TEXT_LIMIT,
                  message: t('notes.create.textTooLong'),
                },
              }}
              render={({ field, invalid, errorText }) => (
                <TextField
                  {...field}
                  label={t('notes.create.textLabel')}
                  fullWidth
                  multiline
                  minRows={3}
                  maxRows={10}
                  error={invalid}
                  helperText={errorText ?? undefined}
                />
              )}
            />
            {submitErrorCode !== null && (
              <Alert severity="error" role="alert" sx={{ width: '100%' }}>
                {submitErrorText(submitErrorCode)}
              </Alert>
            )}
          </FormLayout>
          <DataTable
            rows={notesQuery.data?.notes ?? []}
            columns={columns}
            rowKey={(note) => note.id ?? ''}
            loading={notesQuery.isFetching}
            emptyTitle={t('notes.list.emptyTitle')}
            emptyDescription={t('notes.list.emptyDescription')}
            sx={{ marginTop: 3 }}
          />
        </RouteGuard>
      )}
    </Box>
  )
}
