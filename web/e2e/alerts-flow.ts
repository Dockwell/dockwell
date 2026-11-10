// SPDX-License-Identifier: AGPL-3.0-only

// Alerts' channels, as Alerts-Channels.dc.html has them: reached from the rail, a
// webhook added and tested before it's planned, its plan saying nothing of what
// signs in, a test from the list, what was sent, a change that moves it refused
// its kept secret, and the tab on a phone. The webhook is a server this flow
// starts on this machine, so nothing leaves it. Then an alert as Alerts.dc.html
// has it: an app's check fails as the flow starts, so its alert has fired by the
// end, and the card takes an acknowledgement, says who has it, and gives it back.

import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { networkInterfaces } from "node:os";

import type { Page } from "playwright-core";

import { type DockerProbe, dockerHealth } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";

interface AlertsFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  /** Says what the fake Docker Engine runs, and its socket. */
  docker: DockerProbe;
  socket: string;
}

function fail(message: string): never {
  throw new Error(message);
}

interface Hooked {
  path: string;
  signature: string;
  body: { status?: string; title?: string };
}

/** A webhook on this machine, which keeps what it's sent. */
async function hook(): Promise<{ host: string; url: string; got: Hooked[]; close: () => Promise<void> }> {
  const got: Hooked[] = [];
  const read = (req: IncomingMessage) =>
    new Promise<string>((resolve) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => resolve(body));
    });
  const server = createServer((req, res) => {
    void read(req).then((body) => {
      got.push({
        path: req.url ?? "",
        signature: String(req.headers["dockwell-signature"] ?? ""),
        body: JSON.parse(body || "{}"),
      });
      res.writeHead(202).end();
    });
  });
  // A test of a channel never reaches the control server's own addresses, so the far
  // end listens on this machine's address on its private network, as a webhook on
  // the install's own network would.
  const host = privateAddress();
  await new Promise<void>((resolve) => server.listen(0, host, resolve));
  const { port } = server.address() as AddressInfo;
  return {
    host,
    url: `http://${host}:${port}`,
    got,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

/** This machine's IPv4 address on a private network, which a test of a channel may reach. */
function privateAddress(): string {
  const all = Object.values(networkInterfaces()).flatMap((list) => list ?? []);
  const found = all.find(
    (a) =>
      a.family === "IPv4" &&
      !a.internal &&
      /^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.)/.test(a.address),
  );
  return found?.address ?? fail("this machine has no address on a private network for the flow's webhook to listen on");
}

export async function alertsFlow(page: Page, { origin, step, shoot, docker, socket }: AlertsFlowOptions) {
  const far = await hook();
  // An app whose check starts failing now: the app-down rule waits two minutes, which the steps below fill.
  const sick = (await docker()).find((c) => c.service === "checked" && c.running);
  if (!sick) fail(`Docker runs no checked app for an alert to be about: ${JSON.stringify(await docker())}`);
  await dockerHealth(socket, sick.id, "unhealthy", "the database refuses connections");
  try {
    step("alerts: the rail leads to Alerts, whose rules came with the install");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/`);
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Alerts" }).click();
    await page.getByRole("heading", { level: 1, name: "Alerts" }).waitFor();
    await page.getByRole("link", { name: /Rules/ }).click();
    const rules = page.getByRole("table", { name: "Alert rules" });
    await rules.getByRole("row", { name: /server\.connected == false/ }).waitFor();
    // A header and the eleven rules an install starts with, the restore checks' two among them (ADR 0068).
    await rules.getByRole("row", { name: /backup\.drill == failed/ }).waitFor();
    await rules.getByRole("row", { name: /backup\.drill\.passed > 2 periods ago/ }).waitFor();
    if ((await rules.getByRole("row").count()) !== 12) fail("Rules doesn't list the eleven rules an install starts with");

    step("alerts: a rule changes from its row, as a plan: disk-full says it again weekly");
    // Only how often it repeats, so nothing later in the flow fires that wouldn't have.
    await rules.getByRole("row", { name: /disk\.percent/ }).click();
    const ruleSheet = page.getByRole("dialog");
    await ruleSheet.getByRole("combobox", { name: "While it lasts, say it again" }).selectOption("168h");
    await shoot(page, "alerts-rule");
    await ruleSheet.getByRole("button", { name: "Review plan" }).click();
    const rulePlan = page.locator(".plan-sheet");
    await rulePlan.getByRole("button", { name: /Apply plan/ }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });
    await rules.getByRole("row", { name: /disk\.percent/ }).click();
    const weekly = await page
      .getByRole("dialog")
      .getByRole("combobox", { name: "While it lasts, say it again" })
      .inputValue({ timeout: 20_000 });
    if (weekly !== "168h") fail(`disk-full repeats every ${weekly} after the plan applied, not weekly`);
    await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();

    step("alerts: with nowhere to send, Channels says so, and adds one");
    await page.getByRole("link", { name: /Channels/ }).click();
    await page.getByText("Nowhere to send alerts yet", { exact: true }).waitFor();
    await shoot(page, "alerts-channels-empty");
    await page.getByRole("button", { name: "Add channel" }).click();
    const sheet = page.getByRole("dialog", { name: "Add a channel" });
    await sheet.getByRole("combobox", { name: "Sends to" }).selectOption("webhook");
    await sheet.getByRole("textbox", { name: "Name" }).fill("ops-hook");
    await sheet.getByRole("textbox", { name: "URL" }).fill(`${far.url}/hook?token=e2e-t0ken`);
    await sheet.getByLabel(/^Signing secret/).fill("e2e-s3cret");
    await shoot(page, "alerts-channel-sheet");

    step("alerts: a test sends what the form holds before anything is planned, and says what came back");
    await sheet.getByRole("button", { name: "Send a test" }).click();
    await sheet.getByText("The test arrived", { exact: true }).waitFor({ timeout: 20_000 });
    await sheet.getByText(/answered 202 Accepted/).waitFor();
    const first = far.got.at(-1);
    if (first?.body.status !== "test" || !first.signature.startsWith("t=") || !first.path.includes("e2e-t0ken")) {
      fail(`the webhook got ${JSON.stringify(far.got)}, want a signed test`);
    }

    step("alerts: its plan says nothing of what signs in, and applies");
    await sheet.getByRole("button", { name: "Review plan" }).click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 30_000 });
    const said = await plan.innerText();
    if (said.includes("e2e-t0ken") || said.includes("e2e-s3cret")) fail("the plan shows what signs in to the webhook");
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();

    step("alerts: the channel is listed with its host alone, and tested from the list");
    const table = page.getByRole("table", { name: "Alert channels" });
    const row = table.getByRole("row", { name: /ops-hook/ });
    await row.waitFor({ timeout: 15_000 });
    if ((await row.innerText()).includes("e2e-t0ken")) fail("the list shows the webhook's token");
    await row.getByText(far.host, { exact: false }).waitFor();
    await row.getByRole("button", { name: "Send a test to ops-hook" }).click();
    await page.getByText("The test reached ops-hook", { exact: true }).waitFor({ timeout: 20_000 });
    await row.getByText("Delivered", { exact: true }).waitFor({ timeout: 15_000 });
    await page.getByRole("table", { name: "Messages sent lately" }).getByText("Test message").first().waitFor();
    await page.getByRole("region", { name: "Routing" }).getByText("ops-hook").first().waitFor();
    await shoot(page, "alerts-channels");

    step("alerts: changing it keeps its secret, until it moves to another host");
    await row.click();
    const change = page.getByRole("dialog", { name: "Change ops-hook" });
    const url = change.getByRole("textbox", { name: "URL" });
    if ((await url.inputValue()) !== "" || (await url.getAttribute("placeholder")) !== "Kept, sealed") {
      fail("the form shows what the webhook keeps sealed");
    }
    // Another host on the private network: the secret stays with the one it was for.
    await url.fill(`${far.url.replace(far.host, "10.255.255.1")}/hook`);
    await change.getByRole("button", { name: "Send a test" }).click();
    await change.getByText(/needs its secret again/).waitFor({ timeout: 20_000 });
    await shoot(page, "alerts-channel-moved");
    await change.getByRole("button", { name: "Close" }).first().click();

    step("alerts: on a phone, channels are a list with a test for each");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${origin}/alerts/channels`);
    await page.getByRole("button", { name: "Send a test to ops-hook" }).waitFor();
    const wide = await page.evaluate(() => document.documentElement.scrollWidth);
    if (wide > 390) fail(`Channels is ${wide} px wide on a 390 px phone`);
    await page.setViewportSize({ width: 1440, height: 900 });

    step("alerts: what's open is a card, which takes an acknowledgement with no plan and gives it back");
    await page.goto(`${origin}/alerts`);
    const open = page.getByRole("region", { name: "Open", exact: true });
    const card = open.getByRole("article").filter({ hasText: "checked fails its health check" });
    // The rule's two minutes, an evaluation every 15 s, and a slow machine.
    await card.waitFor({ timeout: 240_000 });
    await card.getByText("Not acknowledged").waitFor();
    await card.getByRole("button", { name: /^Acknowledge: checked fails its health check/ }).click();
    await card.getByText(/^Acknowledged by you /).waitFor();
    if (await page.getByRole("dialog").count()) fail("acknowledging opened a plan, and it's no change");
    await shoot(page, "alerts-acknowledged");
    await page.reload();
    await card.getByText(/^Acknowledged by you /).waitFor();
    await page.goto(`${origin}/activity`);
    await page.getByText("Acknowledged the alert: checked fails its health check", { exact: false }).first().waitFor();
    await page.goto(`${origin}/alerts`);
    await card.getByRole("button", { name: /^Take back your acknowledgement of: checked/ }).click();
    await card.getByText("Not acknowledged").waitFor();
  } finally {
    // The app is well again for the flows after this one, and its alert resolves.
    await dockerHealth(socket, sick.id, "healthy", "ok").catch(() => {});
    await far.close();
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): after Settings. */
export const flow: Flow = { order: 92, run: (page, run) => alertsFlow(page, run) };
