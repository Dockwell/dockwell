// SPDX-License-Identifier: AGPL-3.0-only

// Several apps update at once (ADR 0080): two apps on redis:7.2 are checked, both
// are picked on the Apps list, and Update 2 selected plans each as its own change,
// with a row each, applies them together, and follows each to its end. The apps go
// again at the end, so the flows after it see what they saw before.

import { execFileSync } from "node:child_process";
import { join } from "node:path";

import type { Flow } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

const APPS = ["several-a", "several-b"];

export const flow: Flow = {
  order: 98,
  run: async (page, { origin, step, shoot, bin, data, env }) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      step("update several: there's no registry stand-in on this engine, so nothing is checked");
      return;
    }
    const cli = (...args: string[]) =>
      execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
        encoding: "utf8",
        env: {
          ...process.env,
          ...env,
          USER: "mira",
          DOCKWELL_URL: "",
          DOCKWELL_TOKEN: "",
          XDG_CONFIG_HOME: join(data, "cli-config"),
        },
      });
    cli("login", "--local");
    const fleet = JSON.parse(cli("servers", "list", "--json")) as {
      servers?: { id?: string; controlServer?: boolean }[];
    };
    const control =
      fleet.servers?.find((s) => s.controlServer)?.id ?? fail(`no control server is listed: ${JSON.stringify(fleet)}`);

    step("update several: two apps on redis:7.2 are checked, and each offers 7.4");
    for (const name of APPS) {
      cli("services", "create", name, "--project", "default", "--on", control, "--app-image", "redis:7.2", "-y");
      const checked = cli("app-updates", "check", name, "--project", "default");
      if (!checked.includes("7.4 minor")) fail(`the check of ${name} found ${checked}`);
    }

    step("update several: both are picked on the Apps list, and Update 2 selected plans each, with a row each");
    await page.goto(`${origin}/apps?show=updates`);
    const table = page.getByRole("table", { name: "Apps" });
    for (const name of APPS) {
      await table.getByRole("checkbox", { name: `Select ${name}'s update` }).check({ timeout: 20_000 });
    }
    const bar = page.getByRole("region", { name: "Selected updates" });
    await bar.getByText("2 selected", { exact: true }).waitFor();
    await shoot(page, "update-several-picked");
    await bar.getByRole("button", { name: "Update 2 selected" }).click();
    const sheet = page.getByRole("dialog", { name: "Update 2 apps" });
    await sheet.waitFor({ timeout: 30_000 });
    const apply = sheet.getByRole("button", { name: "Apply 2 updates" });
    await apply.waitFor({ timeout: 30_000 });
    await page.waitForFunction(
      () =>
        [...document.querySelectorAll<HTMLButtonElement>(".update-several button")].some(
          (b) => b.textContent?.includes("Apply 2 updates") && !b.disabled,
        ),
      undefined,
      { timeout: 30_000 },
    );
    await shoot(page, "update-several-plan");

    step("update several: Apply applies them together, and each row follows its own change to its end");
    await apply.click();
    const done = page.getByRole("dialog", { name: "Updates finished" });
    await done.waitFor({ timeout: 180_000 });
    const rows = done.getByRole("list", { name: "Updates" });
    if ((await rows.getByText("Updated", { exact: true }).count()) !== 2)
      fail(`the rows say: ${await rows.innerText()}`);
    await shoot(page, "update-several-done");
    await done.getByRole("button", { name: "Done" }).click();
    await done.waitFor({ state: "detached", timeout: 10_000 });

    step("update several: the apps go again");
    for (const name of APPS) cli("services", "delete", name, "--project", "default", "-y");
  },
};
