# @speed/ui-kit

The platform's first DOM-rendering package: the theme factory that turns
`@speed/tokens` into an MUI v9 theme (`createAppTheme`,
`AppThemeProvider`), and seven controlled core components (`PageHeader`,
`EmptyState`, `ConfirmDialog`, `FileUploader`, `FormField`,
`FormLayout`, `DataTable`) that render only the state hosts give them --
FileUploader renders the upload queue the host owns as `rows` props
while the transfers themselves run in the host's own code. Every
built-in user-facing
string lives in the bilingual `ui-kit` namespace registered through
`@speed/i18n` -- components never ship text in code, and the workspace's
`no-literal-text` ESLint rule refuses it there.

## What ships

| Module | Exports |
|---|---|
| `theme/createAppTheme.ts` | `createAppTheme`, `AppTheme` |
| `theme/AppThemeProvider.tsx` | `AppThemeProvider`, `AppThemeProviderProps` |
| `components/PageHeader.tsx` | `PageHeader`, `PageHeaderProps`, `PageHeaderBreadcrumb` |
| `components/EmptyState.tsx` | `EmptyState`, `EmptyStateProps`, `EmptyStateVariant` |
| `components/ConfirmDialog.tsx` | `ConfirmDialog`, `ConfirmDialogProps`, `ConfirmDialogVariant` |
| `components/FileUploader.tsx` | `FileUploader`, `FileUploaderProps`, `FileUploaderRow`, `FileUploaderRowStatus` |
| `components/FormField.tsx` | `FormField`, `FormFieldProps`, `FormFieldRenderState`, `REQUIRED_ERROR_KEY` |
| `components/FormLayout.tsx` | `FormLayout`, `FormLayoutProps` |
| `components/DataTable.tsx` | `DataTable`, `DataTableColumn`, `DataTableSort`, `DataTableSortDirection`, `DataTableFilter`, `DataTablePagination`, `DataTableProps` |
| `resources.ts` | `UI_KIT_NAMESPACE`, `uiKitResources` |

Everything else (`src/internal/`) is shared component plumbing and is
deliberately not exported. Each component family's contract is
documented below; the type-level contract is the source of truth.

## Quick start

A host bootstraps its i18n instance once, registers the ui-kit
namespace once, and wraps the app in the two providers:

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import {
  AppThemeProvider,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'

const i18n = createI18n() // negotiates the start language
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        {/* screens compose PageHeader, DataTable, ConfirmDialog, ... */}
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` validates before it mutates: both languages must be
covered by bundles with identical leaf key sets, so a missing
translation is a boot-time error, never a silent gap. It must run
exactly once per instance -- never inside a component or provider, or
the double-registration guard throws. This exact composition is
compiled and executed by the package suite (`src/usage-example.test.tsx`),
so the documented usage cannot drift from the API.

An upload panel can share the same host screen under the same
contract. `FileUploader` is fully controlled like every component in
the family: the queue is host-owned `rows` state and every interaction
reports up through a callback (`onSelectFiles` for a pick or drop,
`onCancel` / `onRetry` / `onRemove` keyed by row id). The host below
owns the Files it picked (a retry hands the same bytes back), owns one
`AbortController` per transfer -- so cancelling a row or leaving the
screen aborts the round trip -- and runs its own transport code: one
logical POST per file to the host's storage endpoint, reporting
progress slices and translating an unsuccessful answer into the row's
error text. `FileUploader` never fetches, never holds a File past the
event handler that reported it, and renders exactly the rows it is
given:

```tsx
import { useEffect, useRef, useState } from 'react'
import { FileUploader } from '@speed/ui-kit'
import type { FileUploaderRow } from '@speed/ui-kit'

/**
 * The upload endpoint the host's transport sends its one round trip
 * per file to. A real host's upload code calls a generated api-sdk
 * storage operation here -- the hooks publish only when the merged
 * document gains the storage fragment (deferred alongside org's, see
 * go/storage/AGENTS.md) -- so this is an explicitly labelled fixture
 * placeholder: ui-kit ships no endpoint, and the suite pins the
 * transport shape without pretending to know a wire protocol.
 */
const STORAGE_UPLOAD_URL = 'https://uploads.example.test/objects'

/** True when the given rejection is an abort signal's AbortError. */
function isAbortError(error: unknown): boolean {
  if (error instanceof DOMException) {
    return error.name === 'AbortError'
  }
  return error instanceof Error && error.name === 'AbortError'
}

/**
 * The host's upload transport: one logical POST of the picked bytes per
 * file to the host's storage endpoint, reporting progress slices around
 * the round trip and translating an unsuccessful answer into the row's
 * error text. The fetch is the host's own call -- FileUploader never
 * sees a URL, a signal or a body.
 */
async function uploadToStorage(
  file: File,
  signal: AbortSignal,
  onProgress: (fraction: number) => void,
): Promise<void> {
  onProgress(0.25)
  const body = await file.arrayBuffer()
  const response = await fetch(STORAGE_UPLOAD_URL, {
    method: 'POST',
    headers: { 'content-type': file.type },
    body,
    signal,
  })
  if (!response.ok) {
    throw new Error(`The upload answered ${response.status}.`)
  }
  onProgress(0.75)
}

/**
 * The example upload panel: the host that owns the queue. `rows` is host
 * state -- FileUploader renders exactly what this component writes --
 * and each picked File stays in a host map, because a retry hands the
 * same bytes back to the transport. One AbortController per transfer
 * gives the host both cancellations (abort + drop the row) and the
 * unmount abort; FileUploader itself contributes no state, no fetch and
 * no transfer.
 */
function UploadPanel() {
  const [rows, setRows] = useState<readonly FileUploaderRow[]>([])
  const filesRef = useRef(new Map<string, File>())
  const controllersRef = useRef(new Map<string, AbortController>())
  const nextIdRef = useRef(1)

  // Leaving the screen aborts every transfer this panel owns.
  useEffect(() => {
    const controllers = controllersRef.current
    return () => {
      for (const controller of controllers.values()) {
        controller.abort()
      }
    }
  }, [])

  const patchRow = (id: string, patch: Partial<FileUploaderRow>): void => {
    setRows((current) =>
      current.map((row) => (row.id === id ? { ...row, ...patch } : row)),
    )
  }

  const dropRow = (id: string): void => {
    setRows((current) => current.filter((row) => row.id !== id))
  }

  const transfer = (
    id: string,
    file: File,
    controller: AbortController,
  ): void => {
    uploadToStorage(file, controller.signal, (progress) =>
      patchRow(id, { progress }),
    )
      .then(() => {
        controllersRef.current.delete(id)
        filesRef.current.delete(id)
        patchRow(id, { status: 'succeeded' })
      })
      .catch((error: unknown) => {
        controllersRef.current.delete(id)
        if (isAbortError(error)) {
          // Cancelled (its row was already dropped) or the panel
          // unmounted: there is nothing left to render.
          return
        }
        patchRow(id, {
          status: 'failed',
          error: error instanceof Error ? error.message : String(error),
        })
      })
  }

  const handleSelectFiles = (files: readonly File[]): void => {
    for (const file of files) {
      const id = `upload-${nextIdRef.current}`
      nextIdRef.current += 1
      filesRef.current.set(id, file)
      const controller = new AbortController()
      controllersRef.current.set(id, controller)
      setRows((current) => [
        ...current,
        { id, name: file.name, status: 'uploading' },
      ])
      transfer(id, file, controller)
    }
  }

  const handleCancel = (id: string): void => {
    controllersRef.current.get(id)?.abort()
    filesRef.current.delete(id)
    dropRow(id)
  }

  const handleRetry = (id: string): void => {
    const file = filesRef.current.get(id)
    if (file === undefined) {
      return
    }
    const controller = new AbortController()
    controllersRef.current.set(id, controller)
    patchRow(id, {
      status: 'uploading',
      progress: undefined,
      error: undefined,
    })
    transfer(id, file, controller)
  }

  const handleRemove = (id: string): void => {
    filesRef.current.delete(id)
    dropRow(id)
  }

  return (
    <FileUploader
      multiple
      rows={rows}
      onSelectFiles={handleSelectFiles}
      onCancel={handleCancel}
      onRetry={handleRetry}
      onRemove={handleRemove}
    />
  )
}
```

A transport that rejects before touching the network -- pre-flight
validation (size, type, count) is the host's job -- puts the row into
the failed state showing the rejection's message (host-written and
host-translated, the same contract as the form family's error text);
the row's retry affordance reports up and the host hands the same
`File` back to its transport with a fresh context. Settles announce
once each in a single polite live region; a retry clears it first, so
an identical later failure re-announces instead of sitting silent. This
exact composition -- the `UploadPanel` above rendered beside the
`MembersPage` of `src/usage-example.test.tsx` (a `DataTable` screen
whose sorting, selection, filtering and pagination are all controlled
from host state), the panel's transport a scripted fetch stub answering
one genuine `Response` per round trip in the order the files were
picked -- is compiled and executed by the package suite, so the
documented usage cannot drift from the API.

## The theme factory

Token layers are diffs, applied over the built-in defaults in order:

```ts
const appTheme = createAppTheme(projectTokens, tenantOverrides)
// appTheme.tokens -- the merged token tree
// appTheme.theme  -- the locale-free MUI theme built from it
```

Each layer is optional and merges copy-on-write through the tokens
package's `deepMerge` (no input mutation; untouched branches stay shared
by identity). The factory then maps the merged tree onto MUI's theme
surface:

| Token section | MUI theme slot | Adapter decision |
|---|---|---|
| `color.semantic.<role>` | `palette.<role>` (main/light/dark/contrastText) | key-for-key |
| `color.neutral` | `palette.grey` | steps 50-900 by number; A100/A200/A400/A700 alias the same-number steps (MUI's own grey aliasing); tone 950 has no MUI slot and stays token-only |
| `color.text/background/divider` | same-named slots | direct |
| `typography.fontFamily.sans` | `typography.fontFamily` | direct; the stack ends in CJK-capable fallbacks |
| `typography` sizes/weights | `typography` variant roles | h1-h6 from 5xl..lg (tight line height, semibold); subtitle1/2 from md/sm (normal, medium); body1/2 from md/sm (normal, regular); button from sm (medium); caption from xs; overline from xs (semibold, wide spacing). Converted to rem on MUI's 16px default base; `fontWeight.light` has no token and keeps MUI's 300 |
| `spacing.unit` | the theme spacing unit | the token unit IS the MUI unit (probe-verified against MUI 9.4.0: a numeric spacing option sets the base, so `theme.spacing(n) === n x unit`) |
| `shape.borderRadius` | `shape.borderRadius` | direct |
| `breakpoints.values` | `breakpoints.values` | direct (slot names are MUI's own) |
| `zIndex.values` | `zIndex` | direct (slot names are MUI's own) |
| `shadows` (6 elevation slots) | `theme.shadows` (25-entry ramp) | each elevation i uses the nearest token slot at or below i, floored, never exceeding the design scale; `none` at 0 |

The returned theme is locale-free on purpose: MUI's built-in texts
(table pagination, tooltips) are language-bearing, so the locale merge
happens at render time. `AppThemeProvider` does it for you -- it merges
`muiLocaleFor(i18n.language)` over the base theme, subscribes to
`languageChanged` so MUI texts follow every switch, and renders the
theme-aware `CssBaseline` once. Its props are the i18n instance, the
optional two token layers, and children. Hosts composing their own
theme tree do the same merge explicitly:

```ts
import { createTheme } from '@mui/material/styles'
import { muiLocaleFor } from '@speed/i18n/mui-locale'
const theme = createTheme(appTheme.theme, muiLocaleFor(i18n.language))
```

The provider deliberately renders no `I18nextProvider` and registers no
namespace: both belong to the host bootstrap.

## Components

All components share one contract: **fully controlled**. Every piece of
state is owned by the host and flows through props; a component echoes
state back and fires change callbacks. Components never fetch, never
mutate data, never keep business state -- so the same component works in
a form, a modal and a server-rendered screen without surprises.
`FileUploader` follows the same contract: its queue renders from
host-owned `rows` props, and every pick, cancel, retry and remove
reports up through a callback -- an upload in flight is host state,
running in the host's own transport code, and the component never holds
a File past the event handler that received it. The one
interaction-local detail anywhere in the family is ConfirmDialog's
double-confirm arming (interaction state that resets whenever the
dialog closes); nothing else is local, and no component fetches or
keeps business state.

### PageHeader

The page-level heading block: title as a semantic `h1` (one per page),
an optional breadcrumb trail above it, an optional description in
secondary body text, an optional trailing action area.

The breadcrumb trail is a `breadcrumbs?: PageHeaderBreadcrumb[]` of
`{ label, href?, onClick? }` steps rendered inside MUI's `Breadcrumbs`
within a `nav` landmark. The link contract: a crumb with `href`
renders as a link (attach navigation-interception handlers through
`onClick`); a crumb without `href` renders as plain text and is never
interactive. The last crumb stands for the current page and is marked
`aria-current="page"` whether it links or not. Labels are caller
content -- render them already translated, matching the rest of this
component's props. The nav's accessible name (`pageHeader.breadcrumbNav`)
and the label of MUI's collapse-expand button, shown once a trail
exceeds eight crumbs (`pageHeader.showFullPath`), are the component's
only built-in strings, taken from the ui-kit namespace like every other
component's text -- MUI's stock expand label is an English literal and
never ships unreplaced. Props: `title` (ReactNode, required),
`breadcrumbs?`, `description?`, `actions?`, `sx?`.

### EmptyState

The three stock placeholders, chosen by `variant`: `'empty'` (no data
yet), `'noPermission'` (viewer not allowed) and `'error'` (load
failed). Each variant ships built-in bilingual title and description
from the namespace; `title`, `description`, `action` and `icon` props
override or extend them, for when the stock wording does not fit (an
error with its own retry story). The icon is decorative by contract
(`aria-hidden`) -- text carries the meaning, and a custom `icon` should
stay decorative or come with its own label.

### ConfirmDialog

The controlled confirmation modal. `open` shows it, `onConfirm` /
`onCancel` report the two exits; Escape and backdrop clicks call
`onCancel`, never `onConfirm`. `confirmLoading` renders the confirm
button busy and freezes both exits. Variants: `'default'`, and
`'danger'` for destructive confirmations, which paints the confirm
button in the error palette role and -- paired with `doubleConfirm` --
re-labels the button with the built-in "click again" text on the first
click, firing `onConfirm` only on the second. The two-step guard is
interaction state only and resets whenever the dialog closes. Hosts
should pass the real business content as `title`/`message` (any
ReactNode, so their own translations flow naturally); buttons fall back
to namespace defaults.

### The form family (FormField + FormLayout)

The form family's shape mirrors the roadmap's form milestone: screens
own a react-hook-form `useForm` instance, `FormLayout` supplies the
skeleton and context, `FormField` adapts one field. Validation derived
from generated types is the form milestone's follow-up (see
Deferrals); today rules are RHF's.

**FormLayout** -- the skeleton: installs the `FormProvider` context (so
`FormField` children need no `control` prop), renders the `<form>` with
RHF's `handleSubmit` wired when `onSubmit` is given (native browser
validation off -- `noValidate` -- required markers come from RHF rules,
which speak the ui-kit error contract), the vertical field flow with
uniform `spacing` (default 2 theme units) and the right-aligned
`actions` row. `maxWidth` (px, default 600; `false` widens to the
parent) constrains the flow; the form element itself carries no styling
beyond layout. Pass no `onSubmit` to render a bare field flow (a
section inside a larger form, a filters panel). Props: `form` (the
host's `UseFormReturn`, required), `children`, `onSubmit?`, `actions?`,
`spacing?`, `maxWidth?`.

**FormField** -- one field's RHF plumbing collapsed: a `Controller`
bound to `name` under `control` (or the context FormLayout installs),
with the validation-error text contract applied at render time. It
renders no chrome of its own: the host's control renders through the
`render` prop and receives the bound field state plus the resolved
error text -- the common wiring being MUI TextField's `error`/`helperText`:

```tsx
<FormField
  name="email"
  control={control}
  label="Email"
  required
  render={({ field, invalid, errorText, required }) => (
    <TextField
      {...field}
      label="Email"
      required={required}
      error={invalid}
      helperText={errorText ?? undefined}
    />
  )}
/>
```

`required` is a convenience switch: it injects the `form.required`
rule (unless `rules` already define one) and surfaces itself on the
render state for host asterisks. The render state
(`FormFieldRenderState`) carries `field`, `invalid`, `isTouched`,
`required`, `errorMessage` (raw) and `errorText` (resolved display
text). Error messages follow the validation-error contract: a message
that is a ui-kit-namespace key (`form.required`, `form.invalid`, or any
other ui-kit key) renders as its translation in the current language;
anything else -- already-localized host text, a host-specific code --
renders verbatim. ui-kit resolves keys in its own namespace only and
never guesses another namespace's codes. `REQUIRED_ERROR_KEY` exports
the built-in key for hosts that build rules of their own.

MUI's own controls already render label + helper text with the aria
wiring, so the family deliberately ships no second label/error
renderer: the "uniform error display" is this state contract plus
FormLayout's spacing skeleton.

### DataTable

The fully controlled table view. `rows` is by contract the set of rows
the host wants shown right now -- the component never re-sorts or
re-slices, because an implicit second sort would corrupt the
server-side page a host just fetched and slicing would hide rows the
pagination labels still count. Sorting and filtering therefore appear
as state echo plus callbacks:

- `sort: DataTableSort | null` + `onSortChange` -- renders the
  `aria-sort` state on the header cell and makes each sortable header
  the click target (click cycle: a new column ascends, the active
  column flips); the host applies the sort to the data it passes in.
- `filter: DataTableFilter { value, onValueChange }` -- renders the
  labeled filter input; the field-to-field filtering logic stays with
  the host.

Selection is enabled by passing `onSelectionChange`; `selectedRowKeys`
then holds `rowKey` results (index-keyed by default -- pass an id-based
`rowKey` once a table can reorder). The header checkbox toggles exactly
the currently rendered rows and leaves other pages' keys untouched.
`pagination: DataTablePagination` renders the MUI TablePagination
footer with namespace labels; `count: -1` (unknown total, infinite
scroll) switches the counter to the no-total wording. A `loading` table
shows a status row only while `rows` is empty (never blanks content
mid-read); an empty table renders the stock EmptyState placeholder,
overridable via `emptyTitle`/`emptyDescription`/`emptyAction`. Extra
props: `rowKey?`, `size?` ('small' | 'medium'), `sx?`.

### FileUploader

The file picker and transfer-queue view, fully controlled like every
component in the family. `rows: readonly FileUploaderRow[]` is the
queue the host wants shown right now -- one row per transfer, with the
row's `status` ('uploading' | 'succeeded' | 'failed'), an optional
`progress` fraction within [0, 1] (absent means an indeterminate bar;
an out-of-range or non-finite fraction folds into [0, 1] at render
time) and an optional `error` string rendered verbatim on failed rows.
The component never fetches, never holds a File past the event handler
that reported it and keeps no record of rows that are not in the
current `rows` prop: an upload in flight is host state, executed by
host code.

Every interaction reports up through a callback: `onSelectFiles` (a
pick's or drop's files, in order, the presence of which also renders
the picker trigger), `onCancel(rowId)` on uploading rows,
`onRetry(rowId)` on failed rows and `onRemove(rowId)` on settled rows
-- aborting is the host's own controller to perform, and a retry
re-launches the host's transport with the same `File`. Without
`onSelectFiles` the component is a pure queue view. Failed rows' error
text is host-written and host-translated (the same contract as the form
family's validation-error text); pre-flight validation (size, type,
count) is the host's job, done before its transport starts. A real
host's transport typically calls a generated api-sdk storage operation
(the hooks publish only when the merged document gains the storage
fragment -- deferred alongside org's, see `go/storage/AGENTS.md`);
ui-kit ships no endpoint, and the usage example's scripted fetch stub
stands in for that call.

Other props: `multiple` (several files per selection; false by
default), `accept` (forwarded to the picker; advisory only -- real
validation is the host's upload code), `allowDrop` (adds a drop surface
below the trigger reporting through the same `onSelectFiles`; the
keyboard path never depends on it), `disabled` (renders the
affordances inert and ignores picks and drops), `chooseFilesLabel`
(picker label; the built-in bilingual text by default). The trigger is
a button rendered as a label wrapping a visually hidden but focusable
file input -- one tab stop for the whole control, focus shown through
the label's `:focus-within` rule. Settles announce in a single polite
live region (`role="status"`, rendered only while the queue has
content), stored structurally and rendered in the language active at
render time: only a row leaving `uploading` announces (a retry clears
the region first, so an identical later failure re-announces), while
rows the host seeds on mount and progress-only changes stay quiet.
Props: `rows` (required), `onSelectFiles?`, `onCancel?`, `onRetry?`,
`onRemove?`, `multiple?`, `accept?`, `allowDrop?`, `disabled?`,
`chooseFilesLabel?`, `sx?`.

## Text and i18n

Every string a component renders by itself comes from the `ui-kit`
namespace (components take their text through a hook bound to
`UI_KIT_NAMESPACE`, so wording follows the active language and switches
live). The full key set:

| Key | Purpose | Notes |
|---|---|---|
| `dataTable.ariaLabel` | table aria-label | |
| `dataTable.loading` | status-row text while loading | |
| `dataTable.rowsPerPage` | pagination footer label | |
| `dataTable.displayedRows` | "from-to of count" counter | interpolates `{{from}}`, `{{to}}`, `{{count}}` |
| `dataTable.displayedRowsUnknown` | counter for unknown totals | interpolates `{{from}}`, `{{to}}` |
| `dataTable.selectAllRows` | header checkbox label | |
| `dataTable.selectRow` | row checkbox label | interpolates `{{row}}` |
| `dataTable.filterLabel` | filter input label and placeholder | |
| `emptyState.empty.title` / `.description` | stock "no data yet" | |
| `emptyState.noPermission.title` / `.description` | stock "no access" | |
| `emptyState.error.title` / `.description` | stock "load failed" | |
| `confirmDialog.title` | generic confirm heading | host `title` overrides |
| `confirmDialog.message` | generic caution text | host `message` overrides |
| `confirmDialog.confirmLabel` / `cancelLabel` | button defaults | host labels override |
| `confirmDialog.confirmAgainLabel` | danger double-confirm second click | |
| `form.required` | required-rule message | `REQUIRED_ERROR_KEY` |
| `form.invalid` | generic invalid-value message | |
| `pageHeader.breadcrumbNav` | breadcrumb nav landmark accessible name | |
| `pageHeader.showFullPath` | breadcrumb collapse-expand button label | replaces MUI's stock English "Show path" |
| `fileUploader.chooseFiles` | picker trigger label | `chooseFilesLabel` overrides |
| `fileUploader.dropHint` | drop-surface hint, with `allowDrop` | |
| `fileUploader.statusUploading` | uploading row status text and progress bar aria-label | |
| `fileUploader.statusSucceeded` | succeeded row status text | |
| `fileUploader.statusFailed` | failed row status text | |
| `fileUploader.actionRetry` | failed row retry button label | |
| `fileUploader.actionRemove` | settled row remove button label | |
| `fileUploader.actionCancel` | uploading row cancel button label | |
| `fileUploader.announceUploaded` | live-region announcement when a file uploads | interpolates `{{name}}` |
| `fileUploader.announceFailed` | live-region announcement when a file fails | interpolates `{{name}}` |

The two files (`src/locales/zh-CN.json` and `en-US.json`) carry
identical leaf key sets, enforced by registration and by
`tools/check_i18n_keys.py` in CI. Host content (a PageHeader title, a
ConfirmDialog message, a column header) is the host's own translation
surface, and hosts reword the kit by registering their own bilingual
bundle pair -- identical key structure, their wording -- under
`UI_KIT_NAMESPACE` at bootstrap; `uiKitResources` is simply the shipped
default.

## Accessibility

Every component test runs axe over the rendered unit
(`test-utils/axe.ts`), with two deliberate exceptions: `color-contrast`
is disabled because jsdom does no layout or color computation -- a
contrast result there is neither trustworthy nor actionable, and
contrast lives in the theme (palette roles over surfaces), which the
tokens package pins and the factory maps; and `region` is disabled
because the units under test are components, not full pages. Structure
choices worth knowing: PageHeader renders a real `h1` and wraps its
breadcrumb trail in a labeled `nav` landmark with the current page
marked `aria-current`; EmptyState icons
are `aria-hidden` (text carries meaning); ConfirmDialog wires
`aria-labelledby`/`aria-describedby` ids; DataTable uses real table
semantics with `columnheader` sort buttons and `aria-sort`, selection
checkboxes labeled from the namespace (the partial-page header is
`aria-checked="mixed"`), and a filter input whose accessible name
equals its visible text. Do not disable further axe rules without the
same rationale documented here.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree; one copy makes `useTranslation` and hooks inside shipped components safe. devDependencies pin the tested React 19 |
| `@mui/material` | peer (required, ^9) | theme and components; two MUI copies would split the theme context. devDependency 9.4.0 is the tested point |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements -- the emotion context must be shared with the host |
| `react-hook-form` | peer (required, ^7) | the form family's `FormProvider`/`Controller` identity needs one RHF in the tree, and `FormLayout`'s `UseFormReturn` must be the host's own type. devDependency 7.87.0 is the tested point (the peer range is the ^7 API family) |
| `@speed/tokens`, `@speed/i18n` | dependencies | the token tree this package maps, and the i18n instance/bindings every component renders through |

## Deferrals and recorded decisions

- **Validation from generated types**: zod-style validation derived from
  the API-generated types is the form milestone's follow-up (roadmap),
  deliberately not implemented here. The validation-error contract is
  the seam: a future resolver layer can emit any message and the form
  family renders it correctly today.
- **Error-code mapping**: backend error codes render verbatim unless
  they happen to be ui-kit keys. The resolver round that turns codes
  into text (which namespace owns which codes) is a later milestone; the
  form family resolves keys in the ui-kit namespace only.
- **Shadows adapter**: the floored 25-entry ramp (nearest token slot at
  or below each elevation) is a deliberate decision over interpolation,
  recorded in `createAppTheme.ts` and pinned by its tests.
- **no-literal-text scope**: the workspace rule catches the direct
  routes (bare JSX text, text-bearing attribute literals, plain-literal
  children) over package `src`. Indirect routes (a ternary whose
  branches are literals, a literal passed into a component across a
  boundary) are not flagged -- hosts own their literals; the rule is
  the floor, not the ceiling.
- **Storybook**: no preview-harness round exists yet; components are
  covered by jsdom unit tests plus axe scans, and color-contrast
  verification awaits a browser-side visual round (see Accessibility).
- **Tone 950**: the neutral ramp's 950 step has no MUI grey slot and is
  deliberately not mapped (see the factory table).

## Development

From `web/packages/ui-kit`: `pnpm lint`, `pnpm typecheck`, `pnpm test`
(166 tests across 12 files), `pnpm build`. The test suite runs in jsdom
(`vitest.config.ts`); shared helpers live in `test-utils/`
(`renderWithProviders` builds the host tree -- fresh i18n instance per
call, namespace registered -- and `expectNoAxeViolations` runs axe).
Bilingual fixtures are the shipped locale files under `src/locales/`,
imported by sources and tests; `src/usage-example.test.tsx` compiles and
executes the Quick start composition above, so the documented usage
cannot drift from the API. The workspace `no-literal-text` rule and its
rule tests live at `web/eslint-rules/` (rule tests run from the web
workspace root: `pnpm exec vitest run eslint-rules/no-literal-text.test.mjs`);
the rule applies to this package's `src` but not its test files --
fixture strings are data, not rendered product text.
