/**
 * That a sign-in channel the surface offers is one a person can finish.
 *
 * The SMS channel is offered as a tab, accepts a phone number, and
 * answers "Code sent to +8613800138000" -- a specific statement of fact
 * about the person's own phone. In this deployment it is false: the
 * Mailer/SMS seam resolves to the console sender, so the code went to the
 * server's standard output. The person waits, resends, checks their spam
 * folder, tries another number, and concludes the product is broken. The
 * product lied to them, in the same shape as the registration dead end:
 * it invited them down a path it cannot walk.
 *
 * The gate holds the honest form of that, which is a choice between two
 * outcomes rather than one prescribed design:
 *
 *   - the channel is not offered, when nothing can deliver a code; or
 *   - the channel is offered and says plainly that the code went to the
 *     server log, not to a phone.
 *
 * What it refuses is the third state, the one shipping today: offered,
 * and claiming delivery to a phone that will never receive anything.
 *
 * FOR THIS DEPLOYMENT the first outcome is the one taken: this is a
 * demo, and the SMS channel cannot be configured here -- a demo shown
 * to people must not offer an entrance nobody can walk through, so the
 * tab is hidden rather than captioned. The second branch stays in the
 * gate because it is the right answer for a developer's own machine,
 * where seeing the code in the server log is exactly what one wants --
 * but it is not the answer for anything a prospect will open.
 */
import { expect, test } from '@playwright/test'
import { SIGN_IN_TEXT, readSettledText, visitSignIn } from './test-utils/journeys.js'

/** A well-formed E.164 number, so the refusal cannot be about format. */
const VALID_PHONE = '+8613800138000'

/**
 * The claim the surface ships. Quoted from auth-ui's own en-US
 * bundle (smsSignIn.sentNotice, "Code sent to {{phone}}"), reduced to the
 * part that does not interpolate.
 */
const CLAIM_OF_DELIVERY = 'Code sent to'

/**
 * What an honest console-mode notice has to make clear. Any wording
 * counts as long as a person learns the code is not coming to their
 * phone -- the gate reads for the substance, not a phrase.
 */
const HONEST_ABOUT_CONSOLE = /server log|console|not configured|development/i

// The demo deployment declares channels={['password']}, so the sign-in
// surface offers only the channel it can actually deliver -- no SMS
// tab, and no claim about a phone that would never receive anything.
//
// @budget rather than untagged: it drives the sign-in surface and the
// per-IP pool cannot absorb another visit in the default tier.
test(
  'the SMS channel is either absent or honest about where the code went',
  { tag: '@budget' },
  async ({ page }) => {
    await visitSignIn(page)

    const smsTab = page.getByRole('tab', { name: SIGN_IN_TEXT.smsTab })
    if ((await smsTab.count()) === 0) {
      // Not offered. Correct for a deployment with no way to send a
      // message, and nothing more to check.
      return
    }

    await smsTab.click()
    await page.getByRole('textbox', { name: 'Phone number' }).fill(VALID_PHONE)
    await page.getByRole('button', { name: 'Send code' }).click()

    // SOMETHING has to answer, and it must SAY something, not merely
    // exist: silence after Send code leaves a person pressing the button
    // again -- worse than the claim of delivery this gate was written
    // for, since a person who is told the wrong thing at least knows the
    // button did something. auth-ui keeps an empty live region on the
    // surface at all times so an announcement can be placed into it
    // without a container appearing -- correct for a screen reader, and
    // it satisfies a plain existence check while holding "". So the
    // notice is asserted non-empty, and polled rather than read once:
    // the notice arrives a moment after the press, and a point-in-time
    // sample taken too early reports the surface as silent while the
    // notice is a beat away.
    const notice = page.getByRole('status').or(page.getByRole('alert'))
    await expect
      .poll(
        async () => (await notice.allTextContents()).join(' ').trim(),
        { timeout: 15_000 },
      )
      .not.toBe('')

    // Read from the BODY, not from a main landmark: the sign-in surface
    // has no `main` -- there is no app frame yet, which is the whole
    // point of a sign-in page -- so a main-landmark read would wait out
    // its full timeout and fail with a locator error instead of naming
    // the defect. Settled, not sampled: reading immediately misreports
    // the surface as silent, since the notice arrives a beat after the
    // press.
    const text = await readSettledText(page.locator('body'))

    if (CLAIM_OF_DELIVERY.split(' ').every((word) => text.includes(word))) {
      // The surface claims a phone received a code. That must be true,
      // which in this deployment it is not -- so a surface that claims it
      // must also be a surface with something that can deliver, and the
      // honest-console wording is what says it does not.
      expect(
        HONEST_ABOUT_CONSOLE.test(text),
        'the surface tells a person their phone received a code while the code went to the server log',
      ).toBe(true)
    }
  },
)

test(
  'a phone number without a country code is refused with instructions, not a bare rejection',
  async ({ page }) => {
    // This deployment offers only the password channel, so the surface
    // that renders this message is unreachable here and the test stands
    // down rather than running nowhere wearing a green tick. What it
    // guards -- auth-ui's authn.invalid_phone answer, which says what is
    // wrong, what to do, and shows an example -- still ships in
    // @speed/auth-ui and its own package tests cover it; what is gone is
    // the consumer-level proof that a real deployment renders it.
    //
    // Deliberately not relocated to another surface to keep it running:
    // the closest candidate is the registration password-policy message,
    // which is a different message, and a test that quietly changes its
    // subject to stay green is worth less than one that says it stopped.
    await visitSignIn(page)
    const smsTab = page.getByRole('tab', { name: SIGN_IN_TEXT.smsTab })
    test.skip((await smsTab.count()) === 0, 'the SMS channel is not offered in this deployment')

    await smsTab.click()
    await page.getByRole('textbox', { name: 'Phone number' }).fill('13800138000')
    await page.getByRole('button', { name: 'Send code' }).click()

    const alert = page.getByRole('alert')
    await expect(alert).toBeVisible()
    // The three things that make it usable: it says what is wrong, what
    // to do, and shows an example.
    await expect(alert).toContainText(/not valid|invalid/i)
    await expect(alert).toContainText(/country code/i)
    await expect(alert).toContainText('+')
  },
)
