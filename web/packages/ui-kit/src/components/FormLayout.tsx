/**
 * FormLayout: the form skeleton of the form family.
 *
 * Owns the parts every react-hook-form screen repeats, controlled by the
 * host's own useForm instance:
 *
 *  - the FormProvider context, so the FormField children need no control
 *    prop of their own;
 *  - the <form> element with RHF's handleSubmit wired (native browser
 *    validation is off: required markers come from RHF rules, which speak
 *    the ui-kit validation-error contract);
 *  - the vertical field flow with uniform spacing;
 *  - the right-aligned actions row (submit button etc. -- submit busy
 *    state stays with the host, who owns the request).
 *
 * Pass no onSubmit to render a bare field flow (a form section inside a
 * larger form, a filters panel). The form element carries no styling
 * beyond the layout; hosts keep their own width constraints.
 *
 * Columns (opt-in, additive): `columns` defaults to 1, which is exactly
 * today's unconditional single-column flex flow -- byte-for-byte
 * unchanged for every existing consumer that does not pass it. Setting
 * `columns={2}` switches the flow to a CSS Grid with two responsive
 * column tracks (`sm` -- 600px -- and up; a single track below it) so
 * each direct child (typically one FormField per cell) lays out two per
 * row on wider viewports and collapses to one column on narrow ones.
 * `sm` is chosen deliberately over `md`: a two-field row (e.g.
 * name/email) still has legible field widths as soon as a viewport
 * clears the phone/tablet-portrait boundary, and a form whose fields
 * genuinely need the full row can still pass `columns={1}` regardless
 * of viewport. The actions row spans every column track in grid mode so
 * it always reads as one full-width row, matching its single-column
 * appearance today.
 */

import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import { FormProvider } from 'react-hook-form'
import type { FieldValues, SubmitHandler, UseFormReturn } from 'react-hook-form'

/** The field-flow column count. 2 is a responsive CSS Grid (1 column
 * below the `sm` breakpoint, 2 at `sm` and up); 1 is today's single-
 * column flex flow. */
export type FormLayoutColumns = 1 | 2

export interface FormLayoutProps<TFieldValues extends FieldValues = FieldValues> {
  /** The host's useForm instance (control, handleSubmit, reset all live with the host). */
  readonly form: UseFormReturn<TFieldValues>
  /** The field flow: FormField instances or any custom controls. */
  readonly children: ReactNode
  /**
   * Submit handler; when given, the layout renders a <form> that
   * validates through RHF (noValidate) before calling it with the
   * values. Async errors inside the handler are the host's to catch.
   */
  readonly onSubmit?: SubmitHandler<TFieldValues>
  /** The bottom action row (submit button, cancel link); right-aligned. */
  readonly actions?: ReactNode
  /** Vertical gap between rows, in theme spacing units. Defaults to 2. */
  readonly spacing?: number
  /** Content width in px. Defaults to 600; false widens to the parent. */
  readonly maxWidth?: number | false
  /**
   * Field-flow column count. Defaults to 1 (today's unconditional
   * single-column flow, unchanged for every consumer that omits this
   * prop). See the module doc comment for the responsive breakpoint
   * this switches on and the reasoning behind it.
   */
  readonly columns?: FormLayoutColumns
}

/**
 * The form skeleton: FormProvider context, optional <form> submission,
 * field flow and actions row.
 */
export function FormLayout<TFieldValues extends FieldValues>({
  form,
  children,
  onSubmit,
  actions,
  spacing = 2,
  maxWidth = 600,
  columns = 1,
}: FormLayoutProps<TFieldValues>) {
  const handleSubmit = onSubmit === undefined ? undefined : form.handleSubmit(onSubmit)
  const isGrid = columns === 2
  const body = (
    <>
      {children}
      {actions !== undefined && (
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'flex-end',
            alignItems: 'center',
            gap: 1,
            // Span every grid track so the actions row always reads as
            // one full-width row in the two-column layout, matching its
            // single-column appearance; a no-op in the default flex flow.
            ...(isGrid ? { gridColumn: '1 / -1' } : {}),
          }}
        >
          {actions}
        </Box>
      )}
    </>
  )
  const widthSx = {
    width: '100%',
    maxWidth: maxWidth === false ? 'none' : maxWidth,
  } as const
  const flowSx = isGrid
    ? ({
        display: 'grid',
        gridTemplateColumns: { xs: '1fr', sm: 'repeat(2, 1fr)' },
        columnGap: spacing,
        rowGap: spacing,
        ...widthSx,
      } as const)
    : ({
        display: 'flex',
        flexDirection: 'column',
        gap: spacing,
        ...widthSx,
      } as const)
  if (handleSubmit !== undefined) {
    return (
      <FormProvider {...form}>
        <Box component="form" noValidate onSubmit={handleSubmit} sx={flowSx}>
          {body}
        </Box>
      </FormProvider>
    )
  }
  return (
    <FormProvider {...form}>
      <Box sx={flowSx}>{body}</Box>
    </FormProvider>
  )
}
