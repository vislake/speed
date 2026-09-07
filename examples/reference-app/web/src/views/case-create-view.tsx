/**
 * case-create-view.tsx -- the one-page case creation flow of the
 * reference-app web host: a page where the person picks the patient's
 * photos, enters the patient name and submits once. Each picked file is
 * uploaded the moment it is selected (through the generated
 * cases_uploadPhoto operation -- the surface-level one-shot form of
 * go/storage's three-step protocol, whose answer is the photo object's
 * id), the queue renders from host-owned rows via ui-kit's FileUploader
 * (the widget never transports anything itself), and the single submit
 * creates the case with the uploaded photos' object ids
 * (cases_createCase with photo_object_ids) -- never an empty-case-first
 * two-step flow.
 *
 * The create button stays disabled until every picked file has settled
 * one way or the other: a photo still uploading is not an object id
 * yet, and a photo that failed is not attached -- its row shows the
 * refusal's bilingual text with Retry and Remove, and the form can only
 * proceed once every remaining row succeeded. A case with no photos at
 * all is legal (intake-first) and submits without photo_object_ids.
 * Removing or cancelling a row abandons any upload still in flight; a
 * photo object that landed server-side but was never attached is
 * reclaimed by go/storage's own expiry sweep like any other object.
 *
 * Uploads run through the shared photo byte bound the server enforces;
 * the browser file's bytes are read once (FileReader) and base64-
 * encoded for the JSON-only transport. Every refusal -- upload or
 * create -- renders its code's bilingual text from the cases error map
 * (cases-errors.ts); an unknown code degrades to the generic fallback,
 * never a raw key.
 */

import type { ReactElement } from 'react'
import { useRef, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import { useCasesCreateCase, useCasesUploadPhoto } from '@speed/api-sdk'
import { useTranslation } from '@speed/i18n'
import { FileUploader } from '@speed/ui-kit'
import type { FileUploaderRow } from '@speed/ui-kit'
import { FormField, FormLayout } from '@speed/ui-kit'
import { useForm } from 'react-hook-form'
import { casesErrorCodeOf, casesErrorTextKey } from '../cases-errors.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { CasesSurfaceHeading } from './case-surface-heading.js'

/**
 * The patient-name length bound the server enforces is 200 characters
 * (Unicode code points -- internal/cases's patientNameMaxLength). A
 * client-side mirror must not refuse a name the server would accept, so
 * this bound counts UTF-16 code units at the worst case the server's
 * rune count allows: every code point takes at most two UTF-16 units,
 * so 200 code points can never exceed 400 units. The rule is a courtesy
 * that stops a mistyped novel before the network; the server's own
 * rune-counted refusal remains the authority (and renders its own text).
 */
const PATIENT_NAME_MAX_UTF16_UNITS = 200 * 2

/** The one-page form's fields: the patient name only (a patient
 * reference is deliberately not collected here -- the photo and the
 * name are what the acceptance flow fills in one step). */
interface CaseDraft {
  readonly patientName: string
}

/** One queue row the host renders: the FileUploader row plus the
 * bookkeeping the host's own transport needs (the File for retries,
 * the landed object id). */
interface PhotoRow extends FileUploaderRow {
  /** The picked file; kept for a retry, dropped with the row. */
  readonly file: File
  /** The uploaded object id once the row has settled succeeded. */
  readonly objectId: string | null
}

/** Reads a File's bytes as the upload op's base64 payload (the data
 * URL's own payload after its comma). A file that reads to nothing
 * yields '' and the server's required-refusal answers. */
function fileToContentBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => {
      reject(reader.error ?? new Error('file could not be read'))
    }
    reader.onload = () => {
      const dataUrl =
        typeof reader.result === 'string' ? reader.result : ''
      const comma = dataUrl.indexOf(',')
      resolve(comma === -1 ? '' : dataUrl.slice(comma + 1))
    }
    reader.readAsDataURL(file)
  })
}

/** The one-page creation flow. */
export function CasesCreateView({
  onCreated,
}: {
  /** The host's answer to a completed create: navigate back to the cases list. */
  readonly onCreated: () => void
}): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const createForm = useForm<CaseDraft>({ defaultValues: { patientName: '' } })
  const createMutation = useCasesCreateCase()
  const uploadMutation = useCasesUploadPhoto()

  const [rows, setRows] = useState<readonly PhotoRow[]>([])
  const [submitErrorCode, setSubmitErrorCode] = useState<string | null>(null)
  const nextRowId = useRef(0)
  const fileInput = useRef<HTMLInputElement | null>(null)
  // The transport's own file ledger: row id -> picked File, kept for
  // retries and read by the async upload regardless of render state
  // (an upload started from an event handler must not depend on the
  // rows snapshot that handler closed over). Dropped with the row.
  const filesByRowId = useRef<Map<string, File>>(new Map())

  /** Adds one row per picked file and starts each file's upload. */
  function handleFilesPicked(files: readonly File[]): void {
    if (files.length === 0) {
      return
    }
    const added: PhotoRow[] = files.map((file) => {
      const id = `case-photo-${nextRowId.current++}`
      return { id, name: file.name, status: 'uploading', file, objectId: null }
    })
    for (const row of added) {
      filesByRowId.current.set(row.id, row.file)
    }
    setRows((current) => [...current, ...added])
    for (const row of added) {
      void uploadOne(row.id)
    }
  }

  /** Uploads one row's file and settles the row: succeeded with the
   * object id the create will reference, or failed with its refusal's
   * text. Updates target the row by id, so a row removed while its
   * upload was in flight is a no-op. */
  async function uploadOne(rowId: string): Promise<void> {
    const file = filesByRowId.current.get(rowId)
    if (file === undefined) {
      return
    }
    try {
      const contentBase64 = await fileToContentBase64(file)
      const answer = await uploadMutation.mutateAsync({
        data: { content_base64: contentBase64 },
      })
      setRows((current) =>
        current.map((row) =>
          row.id === rowId
            ? { ...row, status: 'succeeded', objectId: answer.object_id }
            : row,
        ),
      )
    } catch (error) {
      setRows((current) =>
        current.map((row) =>
          row.id === rowId
            ? {
                ...row,
                status: 'failed',
                error: t(casesErrorTextKey(casesErrorCodeOf(error))),
              }
            : row,
        ),
      )
    }
  }

  /** Removes a settled row (and forgets its file and object id);
   * cancelling an uploading row removes it too -- the host performs no
   * abort, and an object that still landed server-side is reclaimed by
   * go/storage's own expiry sweep. */
  function removeRow(rowId: string): void {
    filesByRowId.current.delete(rowId)
    setRows((current) => current.filter((row) => row.id !== rowId))
  }

  /** The queue a create may proceed with: every row settled succeeded
   * (a still-uploading or failed row blocks the submit). */
  const createBlocked = rows.some((row) => row.status !== 'succeeded')
  const photoObjectIDs: string[] = rows
    .filter((row) => row.status === 'succeeded')
    .map((row) => row.objectId as string)

  /** The single submission: creates the case with the uploaded photos'
   * object ids. A refusal keeps the draft on the form and renders the
   * answer's code text; success hands the host the navigation cue. */
  async function handleCreate(values: CaseDraft): Promise<void> {
    if (createBlocked) {
      return
    }
    setSubmitErrorCode(null)
    try {
      await createMutation.mutateAsync({
        data: {
          patient_name: values.patientName,
          photo_object_ids:
            photoObjectIDs.length > 0 ? photoObjectIDs : undefined,
        },
      })
    } catch (error) {
      setSubmitErrorCode(casesErrorCodeOf(error))
      return
    }
    createForm.reset()
    onCreated()
  }

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <CasesSurfaceHeading title={t('cases.create.heading')} />
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('cases.intro')}
      </Typography>
      <FormLayout
        form={createForm}
        onSubmit={handleCreate}
        actions={
          <Button
            type="submit"
            variant="contained"
            disabled={createBlocked || createForm.formState.isSubmitting}
          >
            {t('cases.create.submit')}
          </Button>
        }
      >
        <FormField
          name="patientName"
          required
          rules={{
            maxLength: {
              value: PATIENT_NAME_MAX_UTF16_UNITS,
              message: t('cases.create.patientNameTooLong'),
            },
          }}
          render={({ field, invalid, errorText }) => (
            <TextField
              {...field}
              label={t('cases.create.patientNameLabel')}
              fullWidth
              error={invalid}
              helperText={errorText ?? undefined}
            />
          )}
        />
        {submitErrorCode !== null && (
          <Alert severity="error" role="alert" sx={{ width: '100%' }}>
            {t(casesErrorTextKey(submitErrorCode))}
          </Alert>
        )}
      </FormLayout>
      <Box sx={{ marginTop: 2 }}>
        {/* The picker is the host's own control: a real button whose
            activation opens the chooser (the input itself is mechanical
            and hidden). The queue below renders through ui-kit's
            FileUploader without a trigger of its own. */}
        <input
          ref={fileInput}
          type="file"
          accept="image/*"
          multiple
          hidden
          onChange={(event) => {
            const files = Array.from(event.target.files ?? [])
            event.target.value = ''
            handleFilesPicked(files)
          }}
        />
        <Button
          variant="outlined"
          onClick={() => fileInput.current?.click()}
        >
          {t('cases.create.addPhoto')}
        </Button>
        <FileUploader
          rows={rows}
          onRetry={(rowId) => {
            setRows((current) =>
              current.map((row) =>
                row.id === rowId
                  ? { ...row, status: 'uploading', error: undefined }
                  : row,
              ),
            )
            void uploadOne(rowId)
          }}
          onRemove={removeRow}
          onCancel={removeRow}
          sx={{ marginTop: 1 }}
        />
      </Box>
    </Box>
  )
}
