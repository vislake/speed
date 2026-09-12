/**
 * notes-view.tsx -- the notes surface of the reference-app web host:
 * the list read through the generated notes hooks over a tenant-
 * namespaced query key, gated by layout-kit's RouteGuard with the
 * status derived from that same query, and a create flow driven by the
 * notesCreateNote mutation through ui-kit's FormLayout/FormField.
 *
 * The gate is the list query itself: the server's rbac layer answers a
 * caller without notes:read with 403 rbac.permission_denied, so the
 * query is the real permission fetch behind the router-level
 * RouteGuard, and gated-read.tsx classifies its outcome (error-state
 * first, refusal to 'denied', everything else to the load-failure
 * suit). The create form lives inside the allowed branch, and a refused
 * create (a caller without notes:write answers the same 403) stays on
 * the page with its code text -- the write gate is probed by the
 * mutation, never pre-empted client-side.
 *
 * The list query key is tenant-namespaced per the frontend standard
 * (['tenant', tenantId, ...] over the generated bare key) so a tenant
 * switch can never read the previous tenant's cached notes -- user-
 * menu.tsx evicts the departing tenant's ['tenant', tenantId] queries
 * (and the identity-domain rows) on every switch, and the bootstrap's
 * shipped session-end default (main.tsx's sessionEnded override)
 * empties the whole cache the moment the session ends (a sign-out or a
 * session death), so a different account
 * signing in afterward -- into any tenant -- starts from an empty
 * cache rather than inheriting rows an earlier session's reads left
 * behind. The create invalidates exactly the namespaced key.
 *
 * Created-at times render through Intl in the surface language (never
 * hand-formatted); an unparseable value renders as an empty cell
 * rather than reaching Intl and throwing.
 */

import type { ReactElement } from 'react'
import { useEffect, useMemo, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import type { NotesNote } from '../app-api/index.js'
import {
  getNotesListNotesQueryKey,
  useNotesCreateNote,
  useNotesListNotes,
} from '../app-api/index.js'
import { useTranslation } from '@speed/i18n'
import type { DataTableColumn } from '@speed/ui-kit'
import { DataTable, FormField, FormLayout } from '@speed/ui-kit'
import { useForm } from 'react-hook-form'
import { apiErrorCodeOf } from '../cases-errors.js'
import { gatedRead } from '../gated-read.js'
import { GatedContent } from '../gated-read.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { usePreferredTimeZone } from '../use-preferred-time-zone.js'
import { CurrentClinicLine } from './current-clinic.js'
import {
  clearNotesDraft,
  readNotesDraft,
  writeNotesDraft,
} from './notes-draft.js'

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

  // The view only ever mounts inside the signed-in frame (where the
  // tenant is always present); a null tenant disables the query and the
  // gate stays pending, failing closed rather than inventing a tenant.
  const { tenantId, queryKey: notesListKey } = useTenantQueryKey(
    getNotesListNotesQueryKey(),
  )
  const notesQuery = useNotesListNotes({
    query: { queryKey: notesListKey, enabled: tenantId !== null },
  })

  const createNoteMutation = useNotesCreateNote()
  // The create form seeds from the draft store (notes-draft.ts): a
  // remount of this view -- a hash navigation away and back -- must
  // restore the half-typed text the clinician left behind, not start
  // from an empty field. The store is read once, at form creation,
  // exactly like any defaultValues source.
  const createForm = useForm<NoteDraft>({
    defaultValues: { text: readNotesDraft() },
  })
  const [submitErrorCode, setSubmitErrorCode] = useState<string | null>(null)

  // Mirrors every change of the live form into the draft store, so the
  // store always holds the current text whatever unmounts next. The
  // subscription lives in this view's own effect, so unmounting the
  // view unsubscribes it: no stale subscription writes after the view
  // is gone.
  useEffect(() => {
    const subscription = createForm.watch((values) => {
      writeNotesDraft(values.text ?? '')
    })
    return () => {
      subscription.unsubscribe()
    }
  }, [createForm])

  // The gate is the list read itself (gated-read.tsx's error-first
  // classification and why it must be so).
  const gate = gatedRead(notesQuery)

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
    // The note landed: the form turns over and the draft store with
    // it -- the text became a record, so nothing remains to restore on
    // a later mount (notes-draft.ts's clear contract). The reset names
    // the empty values explicitly: a no-argument reset would return
    // the form to this instance's mount-time defaults, which, on a
    // mount that restored a draft, means the note that just landed.
    createForm.reset({ text: '' })
    clearNotesDraft()
    await queryClient.invalidateQueries({ queryKey: notesListKey })
  }

  /** Resolves a submit-failure code to its current-language text. */
  function submitErrorText(code: string): string {
    const key = NOTE_ERROR_TEXT_KEYS[code] ?? 'notes.errors.unknown'
    return t(key)
  }

  // Created-at cells render through Intl in the surface language and
  // the viewer's display timezone (usePreferredTimeZone's profile ->
  // device -> UTC chain; every formatter passes it explicitly);
  // an unparseable value renders as an empty cell.
  const timeZone = usePreferredTimeZone()
  const formatCreatedAt = useMemo(() => {
    const formatter = new Intl.DateTimeFormat(i18n.language, {
      dateStyle: 'medium',
      timeStyle: 'short',
      timeZone,
    })
    return (value: string | undefined): string => {
      if (value === undefined) {
        return ''
      }
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [i18n.language, timeZone])

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
      {/* The clinic-context line sits at page-title level, under the
          h1: a person writing a patient record here must be able to
          see which clinic the record will land in from where they are
          writing, never only from the chrome (current-clinic.tsx has
          the full story). */}
      <CurrentClinicLine />
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('notes.intro')}
      </Typography>
      <GatedContent gate={gate}>
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
      </GatedContent>
    </Box>
  )
}
