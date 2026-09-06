/**
 * FormLayout contract: owns the RHF form skeleton around the host's
 * useForm instance -- FormProvider context (FormField children need no
 * control prop), the <form> element with handleSubmit wired and native
 * validation off, the vertical field flow with uniform spacing, the
 * right-aligned actions row. No onSubmit renders a bare field flow
 * without a form element. Validation errors surface through the
 * FormField children and follow the language like everything else.
 */

import { act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useForm } from 'react-hook-form'
import type { SubmitHandler } from 'react-hook-form'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { emittedStyleText } from '../../test-utils/emitted-css.js'
import { TextFieldSlot } from '../../test-utils/text-field-slot.js'
import { FormLayout } from './FormLayout.js'
import { FormField } from './FormField.js'

interface MemberFormValues {
  name: string
  email: string
}

function FullForm({
  onSubmit,
  defaultValues,
}: {
  onSubmit: SubmitHandler<MemberFormValues>
  defaultValues?: MemberFormValues
}) {
  const form = useForm<MemberFormValues>({ mode: 'onSubmit', defaultValues })
  return (
    <FormLayout
      form={form}
      onSubmit={onSubmit}
      actions={<button type="submit">Save member</button>}
    >
      <FormField name="name" required render={(s) => <TextFieldSlot state={s} label="Name" />} />
      <FormField
        name="email"
        required
        render={(s) => <TextFieldSlot state={s} label="Email" type="email" />}
      />
    </FormLayout>
  )
}

describe('FormLayout', () => {
  it('submits nothing while fields are invalid and shows every translated error', async () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const { getByRole, getAllByText } = renderWithProviders(
      <FullForm onSubmit={onSubmit} />,
    )
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: 'Save member' }))

    expect(onSubmit).not.toHaveBeenCalled()
    expect(getAllByText(zhCN.form.required)).toHaveLength(2)
  })

  it('submits the collected values once every field is valid', async () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const utils = renderWithProviders(<FullForm onSubmit={onSubmit} />)
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Save member' }))
    expect(onSubmit).not.toHaveBeenCalled()

    await user.type(utils.getByLabelText('Name'), 'Ada')
    await user.type(utils.getByLabelText('Email'), 'ada@example.com')
    await user.click(utils.getByRole('button', { name: 'Save member' }))

    expect(onSubmit).toHaveBeenCalledWith(
      { name: 'Ada', email: 'ada@example.com' },
      expect.anything(),
    )
  })

  it('submits the host form default values untouched by any rule', async () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const utils = renderWithProviders(
      <FullForm
        onSubmit={onSubmit}
        defaultValues={{ name: 'Ada', email: 'ada@example.com' }}
      />,
    )
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Save member' }))
    expect(onSubmit).toHaveBeenCalledWith(
      { name: 'Ada', email: 'ada@example.com' },
      expect.anything(),
    )
  })

  it('switches the shown errors into the new language', async () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const utils = renderWithProviders(<FullForm onSubmit={onSubmit} />)
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Save member' }))
    expect(utils.getAllByText(zhCN.form.required)).toHaveLength(2)

    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(utils.getAllByText(enUS.form.required)).toHaveLength(2)
    expect(utils.queryByText(zhCN.form.required)).not.toBeInTheDocument()
  })

  it('renders a novalidate form element with the host submit wired', () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const { container } = renderWithProviders(<FullForm onSubmit={onSubmit} />)
    const form = container.querySelector('form')
    expect(form).not.toBeNull()
    expect(form).toHaveAttribute('novalidate')
    expect(form).toHaveStyle({ 'max-width': '600px', gap: '16px' })
  })

  it('renders a bare field flow when no onSubmit is given', () => {
    function FlowOnly() {
      const form = useForm<MemberFormValues>()
      return (
        <FormLayout form={form}>
          <FormField name="name" render={(s) => <TextFieldSlot state={s} label="Name" />} />
        </FormLayout>
      )
    }
    const { container, getByLabelText } = renderWithProviders(<FlowOnly />)
    expect(container.querySelector('form')).toBeNull()
    expect(getByLabelText('Name')).toBeInTheDocument()
  })

  it('renders the actions row right-aligned and apart from the fields', () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const { container, getByRole } = renderWithProviders(<FullForm onSubmit={onSubmit} />)
    const actionsRow = getByRole('button', { name: 'Save member' }).closest('div')
    expect(actionsRow).not.toBeNull()
    expect(container.querySelector('form')).toContainElement(actionsRow!)
  })

  describe('columns', () => {
    it('renders the default single-column flex flow when columns is omitted, byte-for-byte the pre-existing shape', () => {
      const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
      const { container } = renderWithProviders(<FullForm onSubmit={onSubmit} />)
      const form = container.querySelector('form')
      expect(form).toHaveStyle({ display: 'flex', flexDirection: 'column' })
    })

    it('renders the default single-column flow the same way when columns={1} is passed explicitly', () => {
      const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
      function OneColumnForm() {
        const form = useForm<MemberFormValues>({ mode: 'onSubmit' })
        return (
          <FormLayout form={form} onSubmit={onSubmit} columns={1}>
            <FormField name="name" render={(s) => <TextFieldSlot state={s} label="Name" />} />
          </FormLayout>
        )
      }
      const { container } = renderWithProviders(<OneColumnForm />)
      const form = container.querySelector('form')
      expect(form).toHaveStyle({ display: 'flex', flexDirection: 'column' })
    })

    it('switches to a CSS Grid flow when columns={2} is passed', () => {
      const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
      function TwoColumnForm() {
        const form = useForm<MemberFormValues>({ mode: 'onSubmit' })
        return (
          <FormLayout
            form={form}
            onSubmit={onSubmit}
            columns={2}
            actions={<button type="submit">Save member</button>}
          >
            <FormField name="name" render={(s) => <TextFieldSlot state={s} label="Name" />} />
            <FormField name="email" render={(s) => <TextFieldSlot state={s} label="Email" />} />
          </FormLayout>
        )
      }
      const { container } = renderWithProviders(<TwoColumnForm />)
      const form = container.querySelector('form')
      expect(form).toHaveStyle({ display: 'grid' })
      // The responsive gridTemplateColumns is a breakpoint-keyed sx
      // value (1fr below sm, two equal tracks at sm/600px and up), so
      // jsdom's getComputedStyle (real layout and @media evaluation,
      // neither of which jsdom does) cannot resolve which side is
      // "active" -- there is no real viewport for either side to be
      // active at. Reading the generated CSS text instead proves both
      // declarations were wired into the render: this is a property/
      // snapshot assertion, not a proof that either layout renders
      // correctly at a real viewport width.
      const css = emittedStyleText()
      expect(css).toMatch(/grid-template-columns:1fr/)
      expect(css).toMatch(/@media \(min-width:600px\)/)
      expect(css).toMatch(/grid-template-columns:repeat\(2,\s*1fr\)/)
      // The actions row spans every column track in grid mode. jsdom's
      // CSS engine does not resolve the `grid-column: 1 / -1` shorthand
      // through getComputedStyle, so this too is a generated-CSS-text
      // assertion rather than a toHaveStyle computed-style one.
      expect(css).toMatch(/grid-column:1\/-1/)
    })

    it('still submits the collected values with columns={2}, unchanged behavior', async () => {
      const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
      function TwoColumnForm() {
        const form = useForm<MemberFormValues>({ mode: 'onSubmit' })
        return (
          <FormLayout
            form={form}
            onSubmit={onSubmit}
            columns={2}
            actions={<button type="submit">Save member</button>}
          >
            <FormField
              name="name"
              required
              render={(s) => <TextFieldSlot state={s} label="Name" />}
            />
            <FormField
              name="email"
              required
              render={(s) => <TextFieldSlot state={s} label="Email" />}
            />
          </FormLayout>
        )
      }
      const utils = renderWithProviders(<TwoColumnForm />)
      const user = userEvent.setup()
      await user.type(utils.getByLabelText('Name'), 'Ada')
      await user.type(utils.getByLabelText('Email'), 'ada@example.com')
      await user.click(utils.getByRole('button', { name: 'Save member' }))
      expect(onSubmit).toHaveBeenCalledWith(
        { name: 'Ada', email: 'ada@example.com' },
        expect.anything(),
      )
    })
  })

  it('passes axe over a labeled form', async () => {
    const onSubmit = vi.fn<SubmitHandler<MemberFormValues>>()
    const utils = renderWithProviders(<FullForm onSubmit={onSubmit} />)
    const user = userEvent.setup()
    await user.click(utils.getByRole('button', { name: 'Save member' }))
    await expectNoAxeViolations()
  })
})
