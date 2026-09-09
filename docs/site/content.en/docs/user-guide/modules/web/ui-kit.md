---
title: "@speed/ui-kit"
weight: 3
description: "The platform's first DOM-rendering package — createAppTheme and AppThemeProvider map the @speed/tokens tree onto an MUI v9 theme, and seven controlled components (PageHeader, EmptyState, ConfirmDialog, FormField, FormLayout, DataTable, FileUploader) render only the state hosts give them."
---

# @speed/ui-kit

The platform's first DOM-rendering package: the theme factory that
turns `@speed/tokens` into an MUI v9 theme (`createAppTheme`,
`AppThemeProvider`), and seven controlled components — `PageHeader`,
`EmptyState`, `ConfirmDialog`, `FileUploader`, `FormField`,
`FormLayout`, `DataTable` — that render only the state hosts give
them. All built-in strings live in the bilingual `ui-kit` namespace,
registered through `@speed/i18n` — never text in code.

## What it is for

- **The theme factory.** `createAppTheme(projectTokens,
  tenantOverrides)` merges each optional token layer over
  `defaultTokens` as a copy-on-write `deepMerge` diff and maps the
  result onto an MUI v9 theme: palette roles key-for-key, the neutral
  ramp onto `palette.grey` (MUI's own A-step aliasing), typography
  sizes onto the variant roles, spacing/breakpoints/z-index direct,
  and the six elevation shadows floored onto MUI's 25-slot ramp. The
  result is locale-free: `AppThemeProvider` merges the MUI locale of
  `i18n.language` at render time, subscribes to `languageChanged`,
  renders `CssBaseline` once, and no `I18nextProvider` (which belongs
  to the host bootstrap).
- **Seven controlled components.** State flows through props; a
  component echoes it back and fires change callbacks — never
  fetching, mutating or keeping business state. The one
  interaction-local detail, `ConfirmDialog`'s double-confirm arming,
  resets whenever the dialog closes.

## When to choose it

When you build a speed frontend on MUI v9 and want shipped chrome
instead of hand-rolled equivalents: these are the components the
session-family packages and shells are built on. MUI is assumed as
the host's component library — `react`/`react-dom`, `@mui/material`
(^9), `@emotion/react`/`@emotion/styled` and `react-hook-form` are
required peers.

## Wiring and minimal use

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'

const i18n = createI18n()
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>{/* screens */}</AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` validates leaf-key parity across both languages
before mutating and runs exactly once per instance, at bootstrap —
never inside a component. Hosts reword the kit by registering their
own bilingual pair under `UI_KIT_NAMESPACE`; `uiKitResources` is the
shipped default.

## Core API and usage essentials

- **`PageHeader`** — the page's semantic `h1`, an optional breadcrumb
  trail in a labelled `nav` landmark (last crumb
  `aria-current="page"`; `href`-less crumbs never interactive), an
  optional description and a trailing action area.
- **`EmptyState`** — the stock `empty`/`noPermission`/`error`
  placeholders with bilingual title and description from the
  namespace; `title`/`description`/`action`/`icon` override them,
  icons decorative by contract. The title is a real heading whose
  level `headingLevel` picks (default `'h6'`): only the host knows
  what precedes it, so pass the level that continues the page's
  order.
- **`ConfirmDialog`** — controlled: `open` shows it; `onConfirm`/
  `onCancel` report the two exits — Escape and the backdrop always
  call `onCancel`. The `'danger'` variant paints the confirm button
  in the error role and, with `doubleConfirm`, fires `onConfirm`
  only on a second click (the first re-labels it); the
  arming click holds the button inert for `CONFIRM_ARM_LOCKOUT_MS`
  (600ms) — a double click cannot skip the guard.
- **The form family.** Screens own a react-hook-form `useForm`;
  `FormLayout` installs the `FormProvider` context, renders the
  `<form>` with `handleSubmit` wired when `onSubmit` is given, the
  field flow (uniform `spacing`, default 2) and right-aligned
  `actions`, plus the opt-in `columns={2}` grid below `sm`.
  `FormField` binds one `Controller` to `name` and hands the host's
  control, rendered through the `render` prop, the bound state plus
  resolved error text (`field`, `invalid`, `isTouched`, `required`,
  `errorMessage`, `errorText`); `required` injects the
  `form.required` rule unless `rules` define one. A ui-kit-namespace
  key message renders as its translation, anything else verbatim;
  `REQUIRED_ERROR_KEY` exports the built-in key.
- **`DataTable`** — fully controlled: `rows` is exactly what the host
  wants shown, never re-sorted or re-sliced. Sorting and filtering
  are state echo plus callbacks (`sort`/`onSortChange` with
  `aria-sort`; `filter` logic stays with the host);
  `onSelectionChange` enables selection over `rowKey` keys;
  `pagination` renders the footer (`count: -1` for unknown totals);
  loading shows a status row only while rows are empty. The table
  always renders in a horizontal-scroll `TableContainer`; a column's
  opt-in `priority` (`high`/`medium`/`low`) hides it below its
  tier's breakpoint — pure CSS, unset columns never hide.
- **`FileUploader`** — the fully-controlled contract applied to
  uploads: the queue renders from the host's `rows` state (status
  `uploading`/`succeeded`/`failed`, optional `progress` fraction,
  optional verbatim `error`), and pick, cancel, retry and remove
  report up through `onSelectFiles`/`onCancel`/`onRetry`/`onRemove`.
  The transport — validation, round trip, progress, aborting — is
  host code; no `File` outlives the handler that reported it.

## Boundaries and pitfalls

- **No fetching, no business state, no validation rules.** Upload
  validation and transfer are host code — ui-kit ships no upload
  endpoint; a real host's transport typically calls a generated
  storage operation through the api-sdk seam. Backend error codes
  render verbatim unless they are ui-kit keys; there is no code-to-text
  resolver or generated-type validation — the error-text contract is
  the seam.
- **The host owns the heading order.** The stock `'h6'` default only
  preserves old behaviour — pass `headingLevel` (and DataTable's
  `emptyHeadingLevel`) explicitly on real pages.
- Do not render components before the namespace is registered: a
  missing key renders as the key itself — register at bootstrap,
  never in a component.
- A language outside MUI's locale table never throws mid-render: MUI's
  built-in texts fall back to its en-US locale while your own
  translations keep rendering in the active language.

## Source

- [web/packages/ui-kit/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/ui-kit/AGENTS.md) — package rules and recorded decisions
- Related: the token tree it maps ([tokens](/docs/user-guide/modules/web/tokens/)), the chrome that reuses its `EmptyState` ([layout-kit](/docs/user-guide/modules/web/layout-kit/)), and the [frontend-building](/docs/user-guide/domains/frontend-building/) domain guide
