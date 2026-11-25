// SPDX-License-Identifier: AGPL-3.0-only

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

async function applyAndKeep(
  page: Page,
  options: { confirm?: boolean; action?: RegExp; pending?: () => Promise<void> } = {},
) {
  const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
  if (options.confirm) {
    const code = await plan.locator("#plan-confirm-why code").innerText();
    await plan.getByRole("textbox", { name: /to apply it/ }).fill(code);
  }
  await plan.getByRole("button", { name: options.action ?? /Apply plan/ }).click();
  const keep = page.getByRole("button", { name: "Keep the change" });
  await keep.waitFor({ timeout: 30_000 });
  await options.pending?.();
  await keep.click();
  await keep.waitFor({ state: "detached", timeout: 15_000 });
  await page
    .getByRole("button", { name: /^(Done|Close)$/ })
    .first()
    .click();
}

export const flow: Flow = {
  order: 1115,
  async run(page, { origin, step, shoot }) {
    const path = await ownServerPath(page, origin);
    await page.goto(`${origin}${path}/firewall`);
    const rules = page.getByRole("region", { name: "Dockwell's rules" });
    step("a firewall rule can cover every port, then change in place");
    await rules.getByRole("button", { name: "Add a rule" }).click();
    const add = page.getByRole("dialog", { name: /^Add a rule to / });
    await add.getByLabel("From").fill("198.51.100.29");
    await add.getByPlaceholder("Office VPN").fill("Firewall fixture");
    await add.getByRole("button", { name: "Review plan" }).click();
    await applyAndKeep(page, { pending: () => shoot(page, "firewall-handy-keep") });
    const address = rules.getByRole("row", { name: /198\.51\.100\.29/ });
    await address.getByText("All ports", { exact: true }).waitFor();
    await address.getByRole("button", { name: "Edit the rule allow-address" }).click();
    const edit = page.getByRole("dialog", { name: "Edit the rule allow-address" });
    await edit.getByLabel("Port").fill("8196");
    await edit.getByPlaceholder("Office VPN").clear();
    await edit.getByRole("button", { name: "Review plan" }).click();
    await applyAndKeep(page, { confirm: true });
    await address.getByText("8196/tcp", { exact: true }).waitFor();
    await address.getByText("Firewall fixture").waitFor({ state: "detached" });

    step("export and import keep the rules in one reviewed file");
    const downloaded = page.waitForEvent("download");
    await rules.getByRole("button", { name: "Export", exact: true }).click();
    const exportFile = await downloaded;
    if (exportFile.suggestedFilename() !== "dockwell-firewall-rules.json") throw new Error("No firewall rule export");
    const file = rules.locator('input[type="file"]');
    await file.setInputFiles({
      name: "reserved-firewall.json",
      mimeType: "application/json",
      buffer: Buffer.from(
        JSON.stringify({ version: 1, rules: [{ name: "external-zzz-allow", rule: { port: 8197 } }] }),
      ),
    });
    await rules.getByText(/The name external-zzz-allow is reserved for database external access/).waitFor();
    if (await page.getByRole("dialog", { name: "Review plan" }).count()) {
      throw new Error("A file with a reserved name reached the plan");
    }
    await file.setInputFiles({
      name: "firewall.json",
      mimeType: "application/json",
      buffer: Buffer.from(
        JSON.stringify({
          version: 1,
          rules: [{ name: "fixture-import", rule: { port: 8197, comment: "Imported rule" } }],
        }),
      ),
    });
    const imported = rules.getByRole("row", { name: /8197\/tcp/ });
    if (await imported.count()) throw new Error("The import ran before the plan was applied");
    await applyAndKeep(page);
    await imported.waitFor();

    await shoot(page, "firewall-handy-rules");
    await atWidth(page, { width: 390, height: 844 });
    await rules.scrollIntoViewIfNeeded();
    if (await page.evaluate(() => document.documentElement.scrollWidth > 390)) {
      throw new Error("Firewall actions overflow on a phone");
    }
    await atWidth(page, { width: 1440, height: 900 });

    step("two selected firewall rules are removed in one plan and one Keep");
    await rules.getByRole("checkbox", { name: "Select the rule allow-address" }).check();
    await rules.getByRole("checkbox", { name: "Select the rule fixture-import" }).check();
    await rules.getByRole("button", { name: "Remove selected (2)" }).click();
    await applyAndKeep(page, { action: /^Delete 2 resources/ });
    await address.waitFor({ state: "detached" });
    await imported.waitFor({ state: "detached" });

    step("unban acts on the selected fail2ban address without a passkey");
    const fails = page.getByRole("region", { name: "Fail2ban" });
    const banned = fails.getByRole("listitem").filter({ hasText: "198.51.100.77" });
    await banned.getByRole("button", { name: "Unban" }).click();
    const review = page.getByRole("dialog", { name: "Unban an address" });
    await review.getByText(/Unban 198\.51\.100\.77 in the sshd jail/).waitFor();
    const done = page.waitForResponse(
      (response) => response.url().includes("/dockwell.v1.FirewallService/SetFail2banAddressBan") && response.ok(),
    );
    await review.getByRole("button", { name: "Unban", exact: true }).click();
    await done;
    await banned.waitFor({ state: "detached" });
    step("fail2ban reviews a manual ban and binds the passkey to the chosen jail and address");
    await fails.getByRole("button", { name: "Ban an address" }).click();
    const ban = page.getByRole("dialog", { name: "Ban an address" });
    await ban.getByLabel("IP address").fill("198.51.100.89");
    await ban.getByRole("button", { name: "Review plan" }).click();
    await ban.getByText(/Ban 198\.51\.100\.89 in the sshd jail/).waitFor();
    await ban.getByRole("button", { name: "Ban address", exact: true }).click();
    await fails.getByText("198.51.100.89", { exact: true }).waitFor();
    await shoot(page, "firewall-handy-fail2ban");

    const applyJail = async () => {
      const dialog = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await dialog.getByRole("button", { name: /Apply plan/ }).click();
      await page
        .getByRole("button", { name: /^(Done|Close)$/ })
        .first()
        .click({ timeout: 30_000 });
    };
    step("a managed jail can stop and start without losing its settings");
    const managed = fails.getByText("Dockwell writes this jail.");
    if (!(await managed.count())) {
      await fails.getByRole("button", { name: "Edit the sshd jail" }).click();
      await page
        .getByRole("dialog", { name: /^(The sshd jail on|Protect SSH on) / })
        .getByRole("button", { name: "Review plan" })
        .click();
      await applyJail();
      await managed.waitFor();
    }
    await fails.getByRole("button", { name: "Stop jail", exact: true }).click();
    await applyJail();
    const start = fails.getByRole("button", { name: "Start jail", exact: true });
    await start.waitFor();
    await start.click();
    await applyJail();
    await fails.getByRole("button", { name: "Stop jail", exact: true }).waitFor();
  },
};
