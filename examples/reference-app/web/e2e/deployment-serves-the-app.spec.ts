/**
 * That opening the product's address shows the product.
 *
 * This is the acceptance gate for the plainest possible expectation a
 * person has of a deployed application: they type the address, and the
 * application appears. The reference app failed it -- the deployed image
 * carries the Go server alone, so the root path fell through to the API
 * middleware chain and answered a bare
 * `{"code":"tenancy.tenant_unresolved"}` with HTTP 403. Nothing else the
 * product does was reachable, because nothing rendered.
 *
 * The spec is written to run against a REAL DEPLOYMENT, not against the
 * dev server the rest of the suite boots: the defect it guards lives in
 * what the image ships, and a dev server that serves index.html from
 * source can never see it. Point it at a deployment and it checks that
 * deployment:
 *
 *   E2E_BASE_URL=https://speed-reference-app.fly.dev pnpm test:e2e \
 *     deployment-serves-the-app
 *
 * Run without E2E_BASE_URL it still passes against the local dev server,
 * which is correct but weaker -- it then only says the app's own entry
 * point works, which is what the dev server exists to do. The assertions
 * are deliberately about the shape of the answer (an HTML document, a
 * mounted application, a sign-in surface) rather than any copy, so this
 * gate stays true through every later change to what the first screen
 * says.
 */
import { expect, test } from '@playwright/test'
import { SIGN_IN_TEXT } from './test-utils/journeys.js'

test('the address serves an HTML document, not an API answer', async ({ page }) => {
  const response = await page.goto('/')

  expect(response, 'the root path answered nothing at all').not.toBeNull()
  expect(
    response?.status(),
    'the root path must answer a page, not an API refusal',
  ).toBeLessThan(400)

  const contentType = response?.headers()['content-type'] ?? ''
  expect(
    contentType,
    `the root path answered ${contentType || 'no content type'} -- a browser needs HTML here`,
  ).toContain('text/html')
})

test('the served page mounts the application and shows its sign-in surface', async ({ page }) => {
  await page.goto('/')

  // The mount point the host page provides, filled by the app's own
  // bootstrap: an empty #root would mean the document arrived but its
  // script did not, which is what a broken asset path looks like.
  await expect(page.locator('#root')).not.toBeEmpty()

  // A first-time visitor's surface. Asserted through its role rather
  // than its wording, so this gate survives a copy change.
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
  await expect(page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel })).toBeVisible()
})

test('the page loads without a failed asset or a console error', async ({ page }) => {
  const failures: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') {
      failures.push(`console: ${message.text()}`)
    }
  })
  page.on('response', (response) => {
    if (response.status() >= 400 && !response.url().includes('/api/')) {
      failures.push(`asset ${response.status()}: ${response.url()}`)
    }
  })

  await page.goto('/')
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()

  // A page that renders while its console fills with errors is a page
  // that is one browser version away from not rendering. API answers are
  // excluded: a pre-auth visitor's refused calls are the app working, not
  // failing.
  expect(failures, `the page reported ${failures.length} failure(s)`).toEqual([])
})
