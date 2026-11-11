// SPDX-License-Identifier: AGPL-3.0-only

// The Domains section end to end, on the fake Docker Engine of scripts/fakedocker: the
// rail to the list, a first domain through its form and its plan, then a list with
// every state there is, what needs a look first. The fake engine can't run the ingress,
// so ListDomains is answered by hand for those, for real apps and a real server. It
// searches and filters, says what's wrong with the fix, opens an app from a row, fits
// the narrowest phone, stops saying where a domain stands once the panel can't read it,
// and says it's checking, not that anything failed, coming back to it and to Apps.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { bytes, number } from "./wire.ts";

export interface DomainsFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

const PENDING = 1;
const ISSUED = 2;
const FAILED = 3;
const NONE = 4;

interface Answer {
  name: string;
  service: string;
  state: number;
  message?: string;
  days?: number;
  tls?: string;
  stale?: boolean;
}

/** A domain in ListDomains' wire format: one route to service, on one server, saying what's asked. */
function aDomain(environment: string, server: { id: string; name: string }, answer: Answer): Buffer {
  const { name, service, state, message = "", days, tls = "", stale = false } = answer;
  const route = Buffer.concat([
    bytes(1, `rte_${name}`),
    bytes(2, name.replaceAll(".", "-")),
    bytes(4, service),
    number(5, 80),
    tls ? bytes(7, tls) : Buffer.alloc(0),
  ]);
  // Leave a minute inside the intended day for the panel's clock estimate.
  const seconds = days === undefined ? undefined : Math.floor(Date.now() / 1000) + days * 86_400 - 60;
  const certificate = Buffer.concat([
    number(1, state),
    message ? bytes(3, message) : Buffer.alloc(0),
    seconds === undefined ? Buffer.alloc(0) : bytes(4, number(1, seconds)),
  ]);
  const status = Buffer.concat([bytes(1, name), bytes(2, certificate)]);
  const on = Buffer.concat([
    bytes(1, server.id),
    bytes(2, server.name),
    bytes(3, status),
    stale ? number(4, 1) : Buffer.alloc(0),
  ]);
  return bytes(
    1,
    Buffer.concat([bytes(1, name), bytes(2, environment), bytes(3, `https://${name}`), bytes(4, route), bytes(5, on)]),
  );
}

/** What's on the page beyond its width, and what's clipped of what a row says. */
function howItFits(page: Page) {
  return page.evaluate(() => ({
    page: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    clipped: [...document.querySelectorAll(".domains-at, .domains-target, .domains-phone-target, .domains-where")]
      .filter((name) => name.scrollWidth > name.clientWidth)
      .map((name) => name.textContent),
  }));
}

export async function domainsFlow(page: Page, { origin, step, shoot }: DomainsFlowOptions) {
  const rail = page.getByRole("navigation", { name: "Main" });
  const heading = page.getByRole("heading", { level: 1, name: "Domains" });

  step("domains: the rail leads to it, and with no domain it offers the first");
  await page.goto(`${origin}/apps`);
  await rail.getByRole("link", { name: "Domains" }).click();
  await heading.waitFor();
  if ((await rail.getByRole("link", { name: "Domains" }).getAttribute("aria-current")) !== "page") {
    fail("the rail doesn't mark Domains as where you are");
  }
  await page.getByRole("heading", { name: "No domains yet" }).waitFor();
  await shoot(page, "domains-empty");

  step("domains: adding one asks which app, and reaches the plan through the form an app's page has");
  await page.getByRole("button", { name: "Add a domain" }).click();
  const form = page.getByRole("dialog", { name: "Add a domain", exact: true });
  await form.waitFor();
  const app = form.getByRole("combobox", { name: "App" });
  const named = (await app.getByRole("option").allTextContents()).find((label) => label.startsWith("adminer"));
  if (!named) fail("the form offers no adminer to send a domain to");
  await form.getByLabel("Domain").fill("first.example.net");
  await form.getByRole("button", { name: /Review plan/ }).click();
  await form.getByText("Choose the app this domain reaches.").waitFor();
  await app.selectOption({ label: named });
  await shoot(page, "domains-add");
  await form.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("first.example.net").first().waitFor();
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await form.waitFor();
  if ((await form.getByLabel("Domain").inputValue()) !== "first.example.net") fail("cancelling loses the domain");
  await form.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("heading", { name: "No domains yet" }).waitFor();

  const read = async <T>(path: string): Promise<T> => (await (await page.request.get(`${origin}${path}`)).json()) as T;
  const { services } = await read<{ services: { id: string; resource: { name: string; scope: string } }[] }>(
    "/v1/services",
  );
  const adminer = services.find((service) => service.resource.name === "adminer") ?? fail("adminer isn't there");
  const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
  const server = servers[0] ?? fail("no server is there");

  const failing =
    "A certificate authority can't reach bad.example.com. If a tunnel or a proxy ends TLS in front of this server, set tls to off on its routes.";
  const long = "a-tunnel-with-a-name-long-enough-to-wrap.e2e-dockwell.example.net";
  const longApp = "a-service-with-a-name-long-enough-to-wrap-on-a-phone";
  const answers: Answer[] = [
    { name: "shop.example.com", service: "adminer", state: ISSUED, days: 47 },
    { name: "bad.example.com", service: "adminer", state: FAILED, message: failing },
    { name: "new.example.com", service: "adminer", state: PENDING },
    { name: "tunnel.example.com", service: "adminer", state: NONE, tls: "off" },
    { name: "gone.example.com", service: "adminer", state: ISSUED, days: 30, stale: true },
    { name: "old.example.com", service: "adminer", state: ISSUED, days: -2 },
    { name: "nowhere.example.com", service: longApp, state: ISSUED, days: 20 },
    { name: long, service: "adminer", state: ISSUED, days: 88 },
  ];
  const stubbed = /DomainService\/ListDomains/;
  const answer = (route: Parameters<Parameters<Page["route"]>[1]>[0]) =>
    route.fulfill({
      status: 200,
      contentType: "application/proto",
      body: Buffer.concat(answers.map((a) => aDomain(adminer.resource.scope, server, a))),
    });
  await page.route(stubbed, answer);

  step("domains: every state, in an app page's words, with what reaches it and what needs a look");
  const row = (name: string) => page.getByRole("row", { name: new RegExp(`^${name.replaceAll(".", "\\.")}`) });
  await row("shop.example.com").getByText("Valid for 47 days").waitFor({ timeout: 15_000 });
  await row("shop.example.com").getByRole("link", { name: "adminer" }).waitFor();
  await row("bad.example.com").getByText("Failed", { exact: true }).waitFor();
  await row("new.example.com").getByText("Issuing").waitFor();
  await row("tunnel.example.com").getByText("TLS ends in front").waitFor();
  await row("gone.example.com").getByText("Not connected").waitFor();
  await row("old.example.com").getByText("Expired").waitFor();
  await row("old.example.com")
    .getByText(/so browsers refuse this domain/)
    .waitFor();
  await page.getByText("8 domains · 3 need a look").waitFor();
  const first = await page.getByRole("row").nth(1).locator(".domains-at").textContent();
  const second = await page.getByRole("row").nth(2).locator(".domains-at").textContent();
  const third = await page.getByRole("row").nth(3).locator(".domains-at").textContent();
  if ([first, second, third].join() !== "bad.example.com,gone.example.com,old.example.com") {
    fail(`what needs a look isn't first: ${[first, second, third].join(", ")}`);
  }
  if (
    (await row("old.example.com")
      .getByRole("link", { name: /^Open / })
      .count()) !== 0
  ) {
    fail("a domain whose certificate has run out offers to open as if it serves");
  }
  if ((await row("nowhere.example.com").getAttribute("tabindex")) !== null) {
    fail("a row that leads nowhere is a keyboard stop");
  }
  const visit = await row("tunnel.example.com")
    .getByRole("link", { name: "Open tunnel.example.com" })
    .getAttribute("href");
  if (visit !== "https://tunnel.example.com") fail(`a tunnel's link goes to ${visit}`);
  if (
    (await row("gone.example.com")
      .getByRole("link", { name: /^Open / })
      .count()) !== 0
  ) {
    fail("a domain whose server isn't connected offers to open as if it serves");
  }
  await shoot(page, "domains");

  step("domains: a word finds a row, a state narrows them, and the URL keeps both");
  const filter = page.getByRole("searchbox", { name: "Filter domains" });
  await filter.fill("tunnel.example");
  await row("shop.example.com").waitFor({ state: "hidden" });
  await row("tunnel.example.com").waitFor();
  await page.waitForURL(/[?&]q=tunnel\.example/);
  await filter.fill("no-such-domain");
  await page.getByRole("heading", { name: "Nothing matches" }).waitFor();
  await page.getByRole("button", { name: "Clear the filters" }).click();
  await row("shop.example.com").waitFor();
  const state = page.getByRole("combobox", { name: "State" });
  await state.selectOption({ label: "Failing" });
  await row("shop.example.com").waitFor({ state: "hidden" });
  await page.waitForURL(/[?&]state=failing/);
  await page.reload();
  await row("bad.example.com").waitFor();
  if ((await row("shop.example.com").count()) !== 0) fail("reading the page again loses the filter");

  step("domains: what's wrong comes with the fix, and the way to it");
  const bad = row("bad.example.com");
  await bad.getByText(`turn on "A tunnel or a proxy in front handles HTTPS" for this domain`).waitFor();
  await bad.getByRole("button", { name: "It's behind a tunnel or a proxy" }).click();
  const moving = page.getByRole("dialog", { name: "Move bad.example.com behind what's in front" });
  await moving.waitFor();
  await moving.getByRole("button", { name: "Cancel" }).click();
  await state.selectOption({ label: "All states" });

  step("domains: a row opens its app, and a link in a row goes where it says");
  await row("shop.example.com").click({ position: { x: 300, y: 12 } });
  await page.waitForURL(new RegExp(`/apps/${adminer.id}$`));
  await page.getByRole("heading", { level: 1, name: "adminer", exact: true }).waitFor();
  await page.goBack();
  await heading.waitFor();
  await row("gone.example.com").getByRole("link", { name: "Check its page" }).click();
  await page.waitForURL(new RegExp(`/servers/${server.id}$`));
  await page.goBack();

  step("domains: at 800 px what a row reaches, and where, can be read whole");
  await page.setViewportSize({ width: 800, height: 844 });
  await row("shop.example.com").waitFor();
  const narrow = await howItFits(page);
  if (narrow.page > 0) fail(`at 800 px the page is ${narrow.page} px too wide`);
  if (narrow.clipped.length > 0) fail(`at 800 px these are clipped: ${narrow.clipped.join(", ")}`);

  step("domains: the narrowest phone fits it all, and reaches it from More");
  await page.setViewportSize({ width: 360, height: 780 });
  await page.goto(`${origin}/more`);
  await page.getByRole("link", { name: "Domains" }).click();
  await heading.waitFor();
  const name = page.getByRole("link", { name: new RegExp(`^${long.replaceAll(".", "\\.")}`) });
  await name.waitFor();
  const fits = await howItFits(page);
  if (fits.page > 0) fail(`at 360 px the page is ${fits.page} px too wide`);
  if (fits.clipped.length > 0) fail(`at 360 px these are clipped: ${fits.clipped.join(", ")}`);
  const [field, select] = await Promise.all([filter.boundingBox(), state.boundingBox()]);
  if (!field || !select || select.y <= field.y) fail("at 360 px the filters don't stack");
  await page.getByText(/turn on "A tunnel or a proxy in front handles HTTPS"/).waitFor();
  await shoot(page, "domains-phone");
  await state.selectOption({ label: "Behind a tunnel or a proxy" });
  await page.getByRole("link", { name: /^tunnel\.example\.com/ }).click();
  await page.waitForURL(new RegExp(`/apps/${adminer.id}$`));
  await page.setViewportSize({ width: 1440, height: 900 });

  step("domains: once the panel can't read them it says so, and nothing reads as serving");
  await page.goto(`${origin}/domains`);
  await row("shop.example.com").getByText("Valid for 47 days").waitFor();
  await page.unroute(stubbed);
  await page.route(stubbed, (route) => route.abort());
  await row("shop.example.com").getByText("Unknown").waitFor({ timeout: 45_000 });
  await page.getByText(/so your domains read Unknown/).waitFor();
  if ((await page.getByText("need a look").count()) !== 0) fail("what can't be read still counts as needing a look");
  if ((await page.getByText("Valid for 47 days").count()) !== 0) fail("what can't be read still reads as serving");
  if (!(await state.isDisabled())) fail("the state filter stays on with no state to go by");
  await shoot(page, "domains-unread");
  await page.unroute(stubbed);
  await page.route(stubbed, answer);
  await row("shop.example.com").getByText("Valid for 47 days").waitFor({ timeout: 30_000 });

  step("domains: what goes wrong under someone's eyes moves nothing, and offers to come first");
  const listTop = async () => (await page.locator(".dw-table, .domains-phone").first().boundingBox())?.y ?? Number.NaN;
  const quietly = answers.find((answer) => answer.name === "tunnel.example.com") ?? fail("the tunnel isn't listed");
  for (const width of [1440, 360]) {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(`${origin}/domains`);
    await page.getByRole("link", { name: "Open tunnel.example.com" }).waitFor();
    const before = await listTop();
    const firstAt = await page.locator(".domains-at").first().textContent();
    Object.assign(quietly, { state: FAILED, tls: "", message: "tunnel.example.com went wrong." });
    const showFirst = page.getByRole("button", { name: "Show them first" }).first();
    await showFirst.waitFor({ timeout: 15_000 });
    const moved = Math.abs((await listTop()) - before);
    if (moved >= 1) fail(`at ${width} px the list moves ${moved} px as something goes wrong`);
    if ((await page.locator(".domains-at").first().textContent()) !== firstAt) fail("the list reorders under someone");
    await shoot(page, `domains-show-first-${width}`);
    await showFirst.click();
    await showFirst.waitFor({ state: "hidden" });
    Object.assign(quietly, { state: NONE, tls: "off", message: "" });
  }
  await page.setViewportSize({ width: 1440, height: 900 });

  step("domains: coming back after a while it says it's checking, keeps its filter, and so does Apps");
  await page.goto(`${origin}/domains?state=failing`);
  await row("bad.example.com").getByText("Failed", { exact: true }).waitFor();
  await rail.getByRole("link", { name: "Apps" }).click();
  await page.getByRole("heading", { level: 1, name: "Apps" }).waitFor();
  await page
    .getByRole("row", { name: /adminer/ })
    .first()
    .waitFor();
  await rail.getByRole("link", { name: "Activity" }).click();
  await page.getByRole("heading", { level: 1, name: "Activity" }).waitFor();
  // Longer than a reading stands, and then reads slow enough to see what's said while they're on their way.
  await page.waitForTimeout(31_000);
  const health = /HealthService\/ListHealth/;
  const wait = (ms: number) => new Promise((done) => setTimeout(done, ms));
  await page.route(health, async (route) => {
    const read = await route.fetch();
    // Apps holds its rows' place for 3 s while it waits, so a read has to outlast that to show them.
    await wait(4_000);
    await route.fulfill({ response: read });
  });
  await page.unroute(stubbed);
  await page.route(stubbed, async (route) => {
    await wait(2_000);
    await answer(route);
  });
  const says = async (where: string) => {
    if ((await page.getByText(/can't reach the control plane/).count()) !== 0) {
      fail(`back on ${where}, it says it can't reach the control plane, with nothing having failed`);
    }
  };
  // Back through history, as the panel's own clocks are what's on trial, and a page load would clear them.
  await page.goBack();
  await page.getByRole("heading", { level: 1, name: "Apps" }).waitFor();
  await says("Apps");
  await page
    .getByRole("row", { name: /adminer/ })
    .first()
    .getByText("Checking")
    .waitFor({ timeout: 4_000 });
  await says("Apps");
  await shoot(page, "apps-back");
  await page
    .getByRole("row", { name: /adminer/ })
    .first()
    .getByText("Checking")
    .waitFor({ state: "hidden" });
  await says("Apps");
  await page.goBack();
  await heading.waitFor();
  await row("bad.example.com").getByText("Checking").waitFor({ timeout: 2_000 });
  await says("the list");
  if ((await row("shop.example.com").count()) !== 0) fail("back on the list, it widens before it narrows");
  if (await state.isDisabled()) fail("back on the list, the state filter turns off for a read under way");
  await shoot(page, "domains-back");
  await row("bad.example.com").getByText("Failed", { exact: true }).waitFor({ timeout: 10_000 });
  await says("the list");
  await page.unroute(health);
  await page.unroute(stubbed);
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 60, run: (page, run) => domainsFlow(page, run) };
