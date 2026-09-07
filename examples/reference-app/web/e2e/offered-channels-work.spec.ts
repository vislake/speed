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
 * FOR THIS DEPLOYMENT the first outcome is the expected one: the product
 * owner has stated that this is a demo and the SMS channel cannot be
 * configured here. A demo shown to people must not offer an entrance
 * nobody can walk through, so the resolution is to hide the tab rather
 * than to caption it. The second branch stays in the gate because it is
 * the right answer for a developer's own machine, where seeing the code
 * in the server log is exactly what one wants -- but it is not the answer
 * for anything a prospect will open.
 */
import { expect, test } from '@playwright/test'
import { SIGN_IN_TEXT, readSettledText, visitSignIn } from './test-utils/journeys.js'

/** A well-formed E.164 number, so the refusal cannot be about format. */
const VALID_PHONE = '+8613800138000'

/**
 * The claim the surface makes today. Quoted from auth-ui's own en-US
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

// The defect is closed: the demo deployment declares
// channels={['password']} (ba061cd), so the sign-in surface offers only
// the channel it can actually deliver -- no SMS tab, and no claim about a
// phone that would never receive anything. Verified on the fix branch
// before it landed, with the surface printed to confirm the pass was the
// tab being gone rather than this gate's own absent-channel branch
// short-circuiting.
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

    // SOMETHING has to answer, and this is now asserted rather than
    // assumed -- because it turned out not to be true.
    //
    // With the read fixed, this gate passed: no claim of delivery
    // appeared, so the branch below never ran. But the reason no claim
    // appeared is that NOTHING appeared. Pressing Send code produces no
    // notice at all, honest or otherwise -- the body reads
    // "PASSWORD SMS CODE Phone number SEND CODE No account yet?
    // REGISTER" after the press, exactly as before it.
    //
    // That is worse than the lie this gate was written for, and the gate
    // was letting it through: a person who is told the wrong thing at
    // least knows the button did something. Silence leaves them pressing
    // it again. So the gate now fails on silence, and the timeout it
    // used to wait out is the assertion instead of an accident.
    // Required to SAY something, not merely to exist.
    //
    // auth-ui keeps an empty live region on the surface at all times so
    // an announcement can be placed into it without a container
    // appearing -- correct for a screen reader, and it satisfied a plain
    // toBeVisible() while holding "". So this gate passed with the
    // surface completely silent after Send code, which is the
    // check-satisfied-by-absence trap this suite records elsewhere,
    // reached through a container rather than a missing element.
    //
    // And polled rather than read once, which is what the empty region
    // was hiding: the notice DOES arrive, a moment after the press. My
    // first read was a point-in-time sample -- the fifth time this suite
    // has made that mistake -- and it reported the surface as silent
    // while the real defect, the claim of delivery, was a beat away. The
    // wrong diagnosis was more alarming than the truth and would have
    // sent a round looking for a missing notice that is not missing.
    const notice = page.getByRole('status').or(page.getByRole('alert'))
    await expect
      .poll(
        async () => (await notice.allTextContents()).join(' ').trim(),
        { timeout: 15_000 },
      )
      .not.toBe('')

    // Read from the BODY, not from a main landmark.
    //
    // The sign-in surface has no `main` -- there is no app frame yet,
    // which is the whole point of a sign-in page -- so
    // getByRole('main').textContent() waited out the full timeout and
    // this gate failed with "locator.textContent: Test timeout"
    // instead of naming the defect. That matters more than usual here:
    // this gate is the acceptance criterion for the round now fixing
    // the channel, and a criterion that cannot state what it wants
    // leaves the person implementing it guessing.
    // Settled, not sampled: reading this immediately is what produced
    // the "the surface is completely silent" misdiagnosis -- the notice
    // arrives a beat after the press.
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
    // This one is NOT pending: it passes today, and it passes because
    // someone wrote the best error message in this product. It is here to
    // keep it that way -- and as the standard the other messages are held
    // to, since a refusal that says only "invalid" leaves a person
    // guessing while this one leaves them typing.
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
