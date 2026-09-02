/**
 * The canonical host control used across the form-family tests: a MUI
 * TextField wired to a FormField render state the way the package docs
 * show -- the field handlers spread onto the control, `invalid` into
 * the error flag, the resolved error text into the helper text. MUI's
 * TextField renders the label and the helper line with their own aria
 * wiring.
 *
 * Shared here (not duplicated per test file) so the host pattern the
 * tests assert against is the pattern the docs describe, in one place.
 *
 * The state is deliberately the minimal structural slice (no generics):
 * a slot consumes only what it spreads onto the control, so it stays
 * assignable from any form's FormFieldRenderState.
 */

import TextField from '@mui/material/TextField'

export interface TextFieldSlotState {
  readonly field: {
    readonly name: string
    readonly value: unknown
    readonly onChange: (...event: unknown[]) => void
    readonly onBlur: () => void
    readonly ref: (instance: unknown) => void
  }
  readonly invalid: boolean
  readonly errorText: string | null
}

export function TextFieldSlot({
  state,
  label,
  type,
}: {
  readonly state: TextFieldSlotState
  readonly label: string
  readonly type?: string
}) {
  return (
    <TextField
      {...state.field}
      label={label}
      type={type}
      error={state.invalid}
      helperText={state.errorText ?? undefined}
      fullWidth
    />
  )
}
