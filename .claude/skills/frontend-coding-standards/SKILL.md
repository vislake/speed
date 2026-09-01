---
name: frontend-coding-standards
description: speed Frontend Coding Standards — Mandatory package boundaries, component, state, theming, i18n, accessibility and API-client rules for writing, editing and reviewing all React/TypeScript code under web/
triggers:
  - writing frontend code
  - editing frontend code
  - creating React components
  - editing React components
  - adding hooks or stores
  - frontend code review
  - frontend refactoring
  - modifying MUI theme
  - adding i18n text
  - frontend styling
  - fixing frontend bugs
globs:
  - "web/**/*.ts"
  - "web/**/*.tsx"
  - "web/packages/*/locales/**/*.json"
  - "web/**/vite.config.ts"
---

# speed Frontend Coding Standards

This is the single authoritative standard for the frontend. Design rationale lives in `docs/internal/12-frontend.md` (Chinese, internal design discussion); the discipline list lives in the root `CLAUDE.md`. **This document tells you how to write the code.**

**The premise that overrides everything else**: these packages are installed into business projects via `npm install` — they are not an application. A component's props are public API, and changing a required prop breaks every delivered project. Every dependency bundled here ends up in someone else's build output.

---

## 1. Package Boundaries and Layering

16 independent npm packages. The dependency graph lives in `docs/internal/12-frontend.md` and **only flows bottom-up**:

```
tokens / i18n -> ui-kit -> (auth|billing|notification)-core -> *-ui -> layout-kit -> shells
api-client -> api-sdk -> the core packages
```

**Required:**
- The pure UI layer (`ui-kit`) carries **no business, tenant or permission semantics**. Components are controlled and props-driven so they survive a different auth scheme.
- Headless packages (`*-core`) expose state and hooks, never UI. UI packages (`*-ui`) consume only the data structures from core.
- `react`, `react-dom`, `@mui/material` and `@emotion/*` are declared as `peerDependencies`. Shipping them as regular dependencies gives consumers duplicate React/MUI instances.
- Every package carries its own `locales/{zh-CN,en-US}.json` under a namespace named after the package.

**Prohibited:**
- **DO NOT** let same-layer packages depend on each other (`auth-ui` must not depend on `billing-ui`).
- **DO NOT** use barrel files (an `index.ts` re-exporting everything) — it breaks tree shaking. Import from the specific file.
- **DO NOT** bundle brand assets (logos, font files) inside a package — reference the consuming project's `public/` through tokens.

## 2. API Calls: Generated Code Only

**This is the hardest rule on the frontend.**

```tsx
// Correct
import { useBillingListPlans } from '@speed/api-sdk';
const { data, isLoading } = useBillingListPlans();

// Wrong — rejected in review and by CI
fetch('/api/v1/billing/plans')
axios.get('/api/v1/billing/plans')
const myRequest = (url) => fetch(...)     // a homegrown wrapper is equally wrong
```

- **DO NOT** hand-write any backend call. `fetch` / `axios` are permitted only inside `@speed/api-client`, enforced by an ESLint rule.
- **DO NOT** hand-write endpoint paths or response types — they come from `api/openapi.yaml` through `task api:gen`.
- **DO NOT** edit any file in `@speed/api-sdk` — it is generated and overwritten wholesale on every release.
- When an endpoint does not fit your need, change the spec, not the frontend. The sequence is in `docs/internal/21-api-contract.md`.

**Exceptions** (not expressible in OpenAPI, and each must be confined to exactly one place): the in-app-message SSE connection lives in `notification-core`; direct file upload lives in the `FileUploader` inside `ui-kit`.

## 3. Components

```tsx
type PlanCardProps = {
  plan: Plan;
  selected?: boolean;
  onSelect?: (planId: string) => void;
};

export function PlanCard({ plan, selected = false, onSelect }: PlanCardProps) {
  const { t } = useTranslation('billing');
  return (
    <Card sx={{ p: 2, borderColor: selected ? 'primary.main' : 'divider' }}>
      ...
    </Card>
  );
}
```

**Prohibited:**
- **DO NOT** use `React.FC` — annotate the props type directly.
- **DO NOT** use the inline `style` attribute — use `sx`.
- **DO NOT** hardcode pixel values in `sx` — use `theme.spacing()` or MUI spacing multiples.
- **DO NOT** hardcode colors — use palette tokens (`primary.main`, `text.secondary`, `divider`).
- **DO NOT** use `!important` — override through the theme.
- **DO NOT** call APIs directly in a component — use hooks.
- **DO NOT** hardcode user-visible text in any language (see §6).

## 4. State Management

**Dual track**: server state through TanStack Query (the hooks generated into api-sdk), client state through Zustand. Redux is not used.

### Query keys must be tenant-namespaced

```ts
['tenant', tenantId, 'members'] // correct: switching tenants changes the key, stale data invalidates itself
['members']                     // wrong: you will read the previous tenant's cache
```

After `switchTenant` succeeds, explicitly call `queryClient.removeQueries({ queryKey: ['tenant', oldId] })` so an operator switching tenants repeatedly does not accumulate every tenant's data in memory.

### The tenant ID is not a request header

- **DO NOT** put `tenantId` into request headers. Tenant context travels in the access token, and the server trusts only the token.
- The local `currentTenantId` in Zustand is used for exactly three things: query key namespacing, UI display, and as the argument when calling the switch-tenant endpoint.

### Token storage

- The refresh token lives in an httpOnly, Secure, SameSite cookie.
- The access token stays in memory. **DO NOT** put it in `localStorage` — an XSS then walks away with a long-lived credential.

## 5. Theming and Multi-Brand

Three layers of token overrides: `defaultTokens` (built into the package) -> `projectTokens` (the consuming project, at build time) -> `tenantOverrides` (runtime white-labeling).

- **DO NOT** read brand-specific values inside components — always go through the theme.
- **DO NOT** assume light mode only — components must render correctly in light and dark.
- Consuming projects define only the tokens they change; everything else falls back to the defaults.

## 6. Internationalization

```tsx
const { t } = useTranslation('auth');
<Button aria-label={t('login.submit')}>{t('login.submit')}</Button>
```

- **DO NOT** leave literal user-facing text in a package, in any language — CI scans for bare text nodes in JSX.
- New keys must be added to both `zh-CN` and `en-US`; CI verifies the key sets match.
- Dates, numbers and currency always go through `Intl.DateTimeFormat` / `Intl.NumberFormat`. **DO NOT** format by hand — CNY and USD differ in symbol placement and decimal digits.
- Error text is resolved from the error code. **DO NOT** display the `message` field returned by the API — it is an English fallback meant for log triage.
- Layouts must not depend on fixed widths to fit text: English runs 30–50% longer than Chinese. Every component needs Storybook stories in both languages.

## 7. Permissions and Feature Flags

```tsx
const canManage = usePermission('billing:manage');
const billingEnabled = useFeature('billing');

<RequirePermission perm="billing:manage" fallback={null}>
  <Button>{t('billing.upgrade')}</Button>
</RequirePermission>
```

- Permissions arrive as a flat list already computed by the server; the frontend performs set lookup only. **DO NOT** reimplement policy evaluation client-side.
- Declare menu and route visibility with `requiredPermission` / `requiredFeature` on `NavItem`. **DO NOT** hand-roll conditional menu assembly.
- **Frontend permission checks are a UX affordance, not a security boundary** — the server validates independently.
- `admin-shell` (platform staff) and `product-shell` (tenant users) operate in different permission domains. During impersonation a persistent, highly visible indicator is mandatory.

## 8. Accessibility

- Icon-only buttons **must** carry `aria-label={t(...)}`.
- Every interactive element is keyboard operable; focus styles must never be removed.
- Dialogs and drawers need correct focus trapping and `aria-labelledby`.
- Color contrast meets WCAG AA (4.5:1 body, 3:1 large text). **DO NOT** convey state through color alone — pair it with an icon or text.
- Support browser font scaling. **DO NOT** define font sizes in `px` — use rem or MUI typography variants.

## 9. TypeScript

- The single source of truth for backend-related types is `@speed/api-sdk`. **DO NOT** hand-write response types.
- Strict mode is on. **DO NOT** use `any` (`@typescript-eslint/no-explicit-any` is an error).
- Use `type` rather than `interface` for component props, avoiding accidental declaration merging.
- Public packages export complete type declarations; `tsc --noEmit` and `publint` validate the published artifact.

## 10. Forms

- react-hook-form with zod.
- Derive zod schemas from the api-sdk types where possible. **DO NOT** maintain separate validation rules on each side.
- Validation errors render through `FormField`. **DO NOT** lay out error text per form.

## 11. Diagnostics and Logging

The browser has no structured log backend, so the equivalent discipline is **structured reporting**: anything worth diagnosing in production goes through the reporter in `@speed/api-client`, not `console`.

```ts
// Correct
reporter.error('checkout failed', { planId, provider, code: err.code, traceId: err.traceId });

// Wrong
console.log('checkout failed for plan ' + planId);
```

- **DO NOT** use `console.log`. `console.warn` / `console.error` are acceptable for local development only; production-relevant signals go through the reporter.
- Keep the message a constant string and put variables in the attributes object — the same rule as the backend, for the same reason: concatenated messages cannot be grouped or filtered.
- Attribute keys mirror the backend (`tenant_id`, `user_id`, `trace_id`, `duration_ms`) so a frontend report can be correlated with server traces.
- **DO NOT** report PII, tokens or full form contents.
- Always include the `traceId` from the API error envelope — it is the only way to connect a user report to server logs.

## 12. Testing

| Tier | Tooling | Scope |
|---|---|---|
| Unit / component | Vitest + Testing Library | hook logic, component behaviour |
| Visual and docs | Storybook | every public component, in both languages |
| End-to-end | Playwright | the reference-app journeys |

**File and directory layout — not optional:**
- **One test file per source file, full stop.** `PlanCard.tsx` is tested by the one `PlanCard.test.tsx`, however many scenarios that covers — do not split by theme. Splitting is a habit to resist, not a style choice; the only legitimate reason is a genuinely unwieldy file (a high bar, not a soft trigger), and even then the split file keeps the target's name as a prefix (`PlanCard.pricing.test.tsx`, not bare `pricing.test.tsx`). Never a generic word (`misc.test.ts`, `extra.test.ts`). If you find an existing target already fragmented into multiple theme-split files with no real justification, consolidate next time you touch that area.
- Shared test helpers, fixtures and custom render utilities go in a dedicated `test-utils/` directory per package, never duplicated across test files.
- End-to-end/integration specs live physically apart from unit tests, under `e2e/` (Playwright's own convention), never mixed into a package's `src/`. Name each spec for the journey it covers (`checkout-alipay.spec.ts`), not `e2e.spec.ts`.

- Test behaviour, not implementation: assert what the user sees. **DO NOT** assert how many times an internal function was called.
- **Every bug fix ships with a test that reproduces the bug.**
- **DO NOT** ignore warnings — React console warnings, a11y warnings and lint warnings are all first-class issues.

## 13. Language

All code comments, TSDoc, package `README`s and Storybook descriptions are written in **English**. Only `docs/internal/` is in Chinese. See the language rule in `docs/internal/13-documentation-standards.md`.

## 14. Pre-Commit Checklist

- [ ] No hand-written `fetch` / `axios`; every call comes from `@speed/api-sdk`
- [ ] API change started from the spec, generated artifacts committed
- [ ] No hardcoded text; both zh-CN and en-US present
- [ ] No hardcoded colors or pixel values; everything through theme tokens
- [ ] Query keys are tenant-namespaced
- [ ] Icon-only buttons have `aria-label`
- [ ] New public components have bilingual Storybook stories and prop docs
- [ ] Correct in both light and dark mode
- [ ] peerDependencies not accidentally moved into dependencies
- [ ] Bug fix includes a reproducing test
- [ ] Diagnostics go through the reporter with a constant message and structured attributes
- [ ] No new console warnings
