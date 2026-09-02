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
 */

import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import { FormProvider } from 'react-hook-form'
import type { FieldValues, SubmitHandler, UseFormReturn } from 'react-hook-form'

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
}: FormLayoutProps<TFieldValues>) {
  const handleSubmit = onSubmit === undefined ? undefined : form.handleSubmit(onSubmit)
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
          }}
        >
          {actions}
        </Box>
      )}
    </>
  )
  const flowSx = {
    display: 'flex',
    flexDirection: 'column',
    gap: spacing,
    width: '100%',
    maxWidth: maxWidth === false ? 'none' : maxWidth,
  } as const
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
