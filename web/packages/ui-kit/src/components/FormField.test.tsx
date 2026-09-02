/**
 * FormField contract: one field of react-hook-form plumbing with the
 * validation-error text contract -- required injects the 'form.required'
 * rule (never overriding an explicit one), errors surface after submit,
 * a namespace-key error message renders as the active language's text
 * and follows language switches, any other message renders verbatim.
 * The render state carries field/invalid/touched/required plus the raw
 * and resolved error text; the field needs a control or a FormProvider
 * context and fails loudly without either.
 */

import { act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useForm } from 'react-hook-form'
import type { RegisterOptions, SubmitHandler } from 'react-hook-form'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { TextFieldSlot } from '../../test-utils/text-field-slot.js'
import { FormField } from './FormField.js'
import type { FormFieldRenderState } from './FormField.js'

interface FormValues {
  name: string
}

function renderFieldForm(options: {
  defaultValues?: FormValues
  rules?: RegisterOptions<FormValues, 'name'>
  required?: boolean
  withDefaultValueProp?: boolean
} = {}) {
  const onSubmit = vi.fn<SubmitHandler<FormValues>>()
  const states: FormFieldRenderState<FormValues, 'name'>[] = []
  function Harness() {
    const { control, handleSubmit } = useForm<FormValues>({
      defaultValues: options.defaultValues,
      mode: 'onSubmit',
    })
    return (
      <form noValidate onSubmit={handleSubmit(onSubmit)}>
        <FormField<FormValues, 'name'>
          name="name"
          control={control}
          rules={options.rules}
          required={options.required}
          defaultValue={options.withDefaultValueProp ? 'Ada' : undefined}
          render={(state) => {
            states.push(state)
            return <TextFieldSlot state={state} label="Name" />
          }}
        />
        <button type="submit">Submit</button>
      </form>
    )
  }
  const utils = renderWithProviders(<Harness />)
  return { onSubmit, states, ...utils }
}

function nameInput(utils: ReturnType<typeof renderFieldForm>): HTMLInputElement {
  return utils.getByLabelText('Name') as HTMLInputElement
}

describe('FormField', () => {
  it('binds the field to the form default value', () => {
    const utils = renderFieldForm({ defaultValues: { name: 'Ada' } })
    expect(nameInput(utils).value).toBe('Ada')
  })

  it('honors the defaultValue prop when the form carries none', () => {
    const utils = renderFieldForm({ withDefaultValueProp: true })
    expect(nameInput(utils).value).toBe('Ada')
  })

  it('reports required: submit without a value fails with the translated error', async () => {
    const utils = renderFieldForm({ required: true })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))

    expect(utils.onSubmit).not.toHaveBeenCalled()
    expect(utils.getByText(zhCN.form.required)).toBeInTheDocument()
    const input = nameInput(utils)
    expect(input).toHaveAttribute('aria-invalid', 'true')

    const state = utils.states.at(-1)
    expect(state?.invalid).toBe(true)
    expect(state?.errorMessage).toBe('form.required')
    expect(state?.errorText).toBe(zhCN.form.required)
    expect(state?.required).toBe(true)
  })

  it('resolves the error text into the current language on switch', async () => {
    const utils = renderFieldForm({ required: true })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))
    expect(utils.getByText(zhCN.form.required)).toBeInTheDocument()

    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(utils.getByText(enUS.form.required)).toBeInTheDocument()
    expect(utils.queryByText(zhCN.form.required)).not.toBeInTheDocument()
    expect(utils.states.at(-1)?.errorText).toBe(enUS.form.required)
  })

  it('passes a validation error through verbatim when it is not a namespace key', async () => {
    const utils = renderFieldForm({
      required: true,
      rules: { required: 'Give it a name' },
    })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))

    expect(utils.getByText('Give it a name')).toBeInTheDocument()
    expect(utils.queryByText(zhCN.form.required)).not.toBeInTheDocument()
    expect(utils.states.at(-1)?.errorMessage).toBe('Give it a name')
    expect(utils.states.at(-1)?.errorText).toBe('Give it a name')
  })

  it('resolves any ui-kit-namespace key a custom rule returns', async () => {
    // RHF runs non-required rules only against non-empty values, so the
    // failing input carries a value here.
    const utils = renderFieldForm({
      defaultValues: { name: 'bad' },
      rules: { validate: (value: string) => (value === 'bad' ? 'form.invalid' : true) },
    })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))

    expect(utils.getByText(zhCN.form.invalid)).toBeInTheDocument()
    expect(utils.states.at(-1)?.errorMessage).toBe('form.invalid')
    expect(utils.states.at(-1)?.errorText).toBe(zhCN.form.invalid)
  })

  it('does not inject the required rule when rules already define one', async () => {
    // 'form.required' absent on purpose: an explicit rule wins, so the
    // verbatim message above is what shows.
    const utils = renderFieldForm({
      required: true,
      rules: { required: 'Give it a name' },
    })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))
    expect(utils.getByText('Give it a name')).toBeInTheDocument()
    expect(utils.queryByText(zhCN.form.required)).not.toBeInTheDocument()
  })

  it('clears the error and submits the values once the field is valid', async () => {
    const utils = renderFieldForm({ required: true })
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))
    expect(utils.onSubmit).not.toHaveBeenCalled()

    await user.type(nameInput(utils), 'Ada')
    await user.click(utils.getByRole('button', { name: 'Submit' }))
    expect(utils.onSubmit).toHaveBeenCalledWith(
      { name: 'Ada' },
      expect.anything(),
    )
    expect(utils.queryByText(zhCN.form.required)).not.toBeInTheDocument()
    expect(utils.states.at(-1)?.invalid).toBe(false)
  })

  it('submits freely when required is off and no rules apply', async () => {
    const utils = renderFieldForm()
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Submit' }))
    expect(utils.onSubmit).toHaveBeenCalledTimes(1)
    expect(utils.states.at(-1)?.required).toBe(false)
    expect(utils.states.at(-1)?.invalid).toBe(false)
    expect(utils.states.at(-1)?.errorText).toBeNull()
  })

  it('marks the field touched once it has been blurred', async () => {
    const utils = renderFieldForm({ required: true })
    const user = userEvent.setup()
    const input = nameInput(utils)
    await user.click(input)
    await user.tab()
    expect(utils.states.at(-1)?.isTouched).toBe(true)
  })

  it('fails loudly without a control or FormProvider context', () => {
    expect(() =>
      renderWithProviders(
        <FormField name="name" render={(state) => <TextFieldSlot state={state} label="Name" />} />,
      ),
    ).toThrow(/no control available/i)
  })
})
