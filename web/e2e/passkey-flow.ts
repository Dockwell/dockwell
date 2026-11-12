// SPDX-License-Identifier: AGPL-3.0-only

// How often your passkey is asked for again (ADR 0075): Profile's choice, confirmed
// with a passkey, then an action a step-up guards done with no prompt at all within
// the hour; the owner's floor in Settings, Security, and what it does to the choice;
// then each put back, so the flows after this one are asked as before.

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";

const SEEN_WITHIN = 40_000;

function fail(message: string): never {
  throw new Error(message);
}

/** Waits until policy is what applies on the page: chosen, and no longer being confirmed. */
async function settled(page: Page, policy: string) {
  await page.waitForFunction(
    (p) =>
      (document.querySelector(`[data-policy="${p}"] input`) as HTMLInputElement | null)?.checked === true &&
      !document.querySelector('.step-up-choices[aria-busy="true"]'),
    policy,
    { timeout: SEEN_WITHIN },
  );
}

export async function passkeyFlow(page: Page, { origin, step, shoot }: Pick<FlowRun, "origin" | "step" | "shoot">) {
  // Each passkey the panel asks for begins a step-up, so counting them counts the prompts.
  let asked = 0;
  const count = (request: { url: () => string }) => {
    if (request.url().endsWith("/dockwell.v1.AuthService/BeginStepUp")) asked++;
  };
  page.on("request", count);
  try {
    step("passkey: Profile offers each time, once an hour and only when I sign in, each time chosen");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/profile`);
    const choice = page.getByRole("region", { name: "Ask for my passkey" });
    const option = (name: RegExp) => choice.getByRole("radio", { name });
    // What applies is read from the control plane: each time, until someone chooses.
    await settled(page, "EACH_TIME");
    await choice
      .getByText("Changing how you sign in, this choice, and approving someone else's change always ask.")
      .waitFor();
    await shoot(page, "passkey-each-time");

    step("passkey: once an hour takes the passkey, and says until when it covers this session");
    let before = asked;
    await option(/^Once an hour/).check();
    await settled(page, "HOURLY");
    await choice.getByText(/^Confirmed at \d\d:\d\d · asks again at \d\d:\d\d$/).waitFor({ timeout: SEEN_WITHIN });
    if (asked !== before + 1) fail(`choosing asked for ${asked - before} passkeys, want 1`);
    await shoot(page, "passkey-hourly");

    step("passkey: within the hour, a ticket that adds a server comes with no passkey prompt");
    before = asked;
    await page.goto(`${origin}/servers`);
    await page.getByRole("button", { name: "Add server" }).first().click();
    const adding = page.getByRole("dialog", { name: "Add a server" });
    await adding.getByRole("button", { name: "Make the command" }).click();
    const command =
      (await adding.locator(".servers-add-command code").first().textContent({ timeout: SEEN_WITHIN })) ?? "";
    if (!/(?:--join|dockwell join) \S+/.test(command)) fail(`the sheet's command has no ticket: ${command}`);
    if (asked !== before) fail(`making a ticket within the hour asked for ${asked - before} passkeys, want none`);
    await adding.getByRole("button", { name: "Done" }).click();

    step("passkey: the owner sets the floor to each time, with a passkey");
    await page.goto(`${origin}/settings/security`);
    const floor = page.getByRole("group", { name: "The least anyone may choose" });
    await floor.waitFor();
    // A new install lets each person choose.
    await settled(page, "AT_SIGN_IN");
    await shoot(page, "security-floor");
    before = asked;
    await floor.getByRole("radio", { name: /^Each time/ }).check();
    await settled(page, "EACH_TIME");
    if (asked !== before + 1)
      fail(`setting the floor asked for ${asked - before} passkeys, want 1, though the hour covers the rest`);
    await shoot(page, "security-floor-each-time");

    step("passkey: Profile reads the choice as the floor, with the looser choices off and why");
    await page.goto(`${origin}/profile`);
    await choice.getByText("Your choice reads as each time, the least the owner allows.").waitFor();
    if (!(await option(/^Once an hour/).isDisabled())) fail("once an hour is open under a floor of each time");
    await option(/^Once an hour/)
      .locator("xpath=..")
      .getByText("The owner asks each time.")
      .waitFor();
    await shoot(page, "passkey-floor");

    step("passkey: on a phone, the choices stack and nothing scrolls sideways");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${origin}/profile`);
    await choice.getByText("Your choice reads as each time, the least the owner allows.").waitFor();
    const wide = await page.evaluate(() => document.documentElement.scrollWidth);
    if (wide > 390) fail(`Profile is ${wide} px wide on a 390 px phone`);
    await page.goto(`${origin}/settings/security`);
    await page.getByRole("group", { name: "The least anyone may choose" }).waitFor();
    const wideSecurity = await page.evaluate(() => document.documentElement.scrollWidth);
    if (wideSecurity > 390) fail(`Security is ${wideSecurity} px wide on a 390 px phone`);

    step("passkey: both go back, so the flows after this are asked as before");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/settings/security`);
    await page
      .getByRole("group", { name: "The least anyone may choose" })
      .getByRole("radio", { name: /^Let each person choose/ })
      .check();
    await settled(page, "AT_SIGN_IN");
    await page.goto(`${origin}/profile`);
    await option(/^Once an hour/).waitFor();
    await settled(page, "HOURLY");
    await option(/^Each time/).check();
    await settled(page, "EACH_TIME");
    await page.getByText(/^Confirmed at /).waitFor({ state: "detached" });
    await page.goto(`${origin}/activity`);
    await page.getByText("Set everyone to be asked for their passkey each time", { exact: true }).first().waitFor();
    await page.getByText("Let everyone choose how often their passkey is asked for", { exact: true }).first().waitFor();
  } finally {
    page.off("request", count);
  }
}

/** After Settings, before the server pages that take a passkey, on the fake engine (flows.ts). */
export const flow: Flow = { order: 93, run: (page, run) => passkeyFlow(page, run) };
