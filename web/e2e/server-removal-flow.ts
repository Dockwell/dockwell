// SPDX-License-Identifier: AGPL-3.0-only
// Removing a server, as a person does from its page (ADR 0090): the second server
// the health flow joined, with its agent started again, asks what becomes of the app
// that runs there, plans its removal, which takes the passkey and the typed code,
// and once applied it's gone from the fleet and its agent is refused. Then one that
// has just joined, with nothing on it, plans straight away and leaves the same. It
// runs just before the panel domain's flow, which restarts the control plane and runs
// last.
import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { onTop } from "./sheets.ts";

const SEEN_WITHIN = 30_000;

export async function serverRemovalFlow(
  page: Page,
  { origin, step, shoot, bin, data, env }: Pick<FlowRun, "origin" | "step" | "shoot" | "bin" | "data" | "env">,
): Promise<void> {
  step("server removal: the second server's agent starts again, and its page takes it out of the fleet");
  const clean = { ...process.env, ...env, DOCKWELL_DATA_DIR: "", DOCKWELL_URL: "", DOCKWELL_TOKEN: "" };
  const agent: ChildProcess = spawn(bin, ["run", "--data-dir", join(data, "second-agent"), "--log-format", "json"], {
    stdio: "ignore",
    env: clean,
  });
  // An app of its own on the second server, as the health flow took its own away: by the CLI, signed in locally.
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
  try {
    cli("login", "--local");
    const listed = JSON.parse(cli("servers", "list", "--json")) as {
      servers?: { id?: string; name?: string; connection?: string }[];
    };
    const second = listed.servers?.find((s) => s.name === "second")?.id;
    if (!second) throw new Error(`the second server isn't listed: ${JSON.stringify(listed)}`);
    for (let i = 0; i < 100; i++) {
      const now = JSON.parse(cli("servers", "get", second, "--json")) as { server?: { connection?: string } };
      if (now.server?.connection === "CONNECTION_CONNECTED") break;
      await page.waitForTimeout(300);
    }
    cli(
      "services",
      "create",
      "parked",
      "--project",
      "default",
      "--on",
      second,
      "--app-image",
      "nginx:1.27-alpine",
      "--yes",
    );
    await page.goto(`${origin}/servers`);
    await page
      .getByRole("table", { name: "Servers, sorted by name" })
      .getByRole("row", { name: /^second\b/ })
      .click();
    await page.getByRole("link", { name: "Open second" }).click();
    const agentPanel = page.getByRole("region", { name: "Agent" });
    // Its badge, ahead of the fact of how long it has been.
    await agentPanel.getByText("Connected", { exact: true }).first().waitFor({ timeout: SEEN_WITHIN });

    step("server removal: it asks what becomes of the apps there, and stopping them plans their containers off it");
    // It's in the server's header menu, where what acts on the server as a whole is (#800).
    await page.getByRole("button", { name: "Actions for second" }).click();
    await page.getByRole("menuitem", { name: "Remove from fleet" }).click();
    const choosing = page.getByRole("dialog", { name: "Remove second" });
    await choosing.getByText(/apps? runs? on it/).waitFor();
    if (!(await choosing.getByRole("radio", { name: /Leave them running/ }).isChecked())) {
      throw new Error("leaving the apps running isn't the default");
    }
    await choosing.getByRole("radio", { name: /Stop them/ }).check();
    await shoot(page, "server-removal-choice");
    await choosing.getByRole("button", { name: "Plan removal" }).click();

    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    const remove = plan.getByRole("button", { name: "Remove second" });
    await remove.waitFor({ timeout: SEEN_WITHIN });
    await plan.getByText("No undo").waitFor();
    await plan.getByText("Applying this asks for your passkey").waitFor();
    await plan.getByText(/second can't come back: to add it again/).waitFor();
    await plan.getByRole("switch", { name: /It may be compromised/ }).waitFor();
    await shoot(page, "server-removal-plan");

    step("server removal: applied with the passkey alone, the server leaves the fleet");
    // One confirmation (#800): stopping its apps destroys no data, so there's no code to type.
    if ((await plan.getByRole("textbox", { name: /to apply it/ }).count()) !== 0) {
      throw new Error("a removal that destroys nothing more asks for a typed code too");
    }
    await remove.click();
    // Applied, the plan says so, and Done goes back to the list, as the server's page has nothing left to show.
    const done = page.getByRole("dialog").last().getByRole("button", { name: "Done", exact: true });
    await done.waitFor({ timeout: 60_000 });
    await done.click();
    // The list, perhaps with a server it opens beside it, such as one under strain.
    await page.waitForURL(/\/servers(\?|$)/, { timeout: SEEN_WITHIN });
    const table = page.getByRole("table", { name: "Servers, sorted by name" });
    await table.getByRole("row").nth(1).waitFor();
    await table.getByRole("row", { name: /^second\b/ }).waitFor({ state: "detached", timeout: SEEN_WITHIN });
    await shoot(page, "server-removal-done");

    step("server removal: a server that has just joined, with nothing on it, plans straight away and leaves");
    const ticket = /dockwell join (\S+)/.exec(cli("token", "create"))?.[1];
    if (!ticket) throw new Error("dockwell token create printed no ticket");
    execFileSync(bin, ["join", "--data-dir", join(data, "spare-agent"), "--name", "spare", ticket], {
      encoding: "utf8",
      env: clean,
    });
    await page.goto(`${origin}/servers`);
    await table.getByRole("row", { name: /^spare\b/ }).click({ timeout: SEEN_WITHIN });
    await page.getByRole("link", { name: "Open spare" }).click();
    await page.getByRole("button", { name: "Actions for spare" }).click();
    await page.getByRole("menuitem", { name: "Remove from fleet" }).click();
    const empty = onTop(page.getByRole("dialog", { name: "Review plan" }));
    const leave = empty.getByRole("button", { name: "Remove spare" });
    await leave.waitFor({ timeout: SEEN_WITHIN });
    await empty.getByText(/spare can't come back: to add it again/).waitFor();
    await leave.click();
    const left = page.getByRole("dialog").last().getByRole("button", { name: "Done", exact: true });
    await left.waitFor({ timeout: 60_000 });
    await left.click();
    await page.waitForURL(/\/servers(\?|$)/, { timeout: SEEN_WITHIN });
    await table.getByRole("row").nth(1).waitFor();
    await table.getByRole("row", { name: /^spare\b/ }).waitFor({ state: "detached", timeout: SEEN_WITHIN });
  } finally {
    agent.kill("SIGKILL");
    await new Promise((done) => agent.once("exit", done));
  }
}

/** Where sign-in.ts runs this among the flows (flows.ts): before the panel domain's, which restarts the control plane. */
export const flow: Flow = { order: 98.5, run: (page, run) => serverRemovalFlow(page, run) };
