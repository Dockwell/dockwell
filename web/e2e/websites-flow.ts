// SPDX-License-Identifier: AGPL-3.0-only

// The Websites section end to end, on the fake Docker Engine of scripts/fakedocker: the
// rail to the list, a first website to a URL through its sheet and its plan, and one
// to a port on the server, which a person approves. The fake engine can't run the
// ingress, so a website can't apply here, and ListWebsites and ListDomains are answered
// by hand for a list of them; what their pages show of a web server's config comes from
// the control plane itself. It edits that config and sees the line that changes, fits
// a phone, and reads in Chinese. scripts/docker-e2e.sh applies websites on real Docker.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { bytes, number } from "./wire.ts";

export interface WebsitesFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

const ISSUED = 2;
const NONE = 4;

interface Site {
  id: string;
  domain: string;
  root?: string;
  url?: string;
  port?: number;
  environment: string;
  service?: string;
  daysAgo: number;
}

/** A website in ListWebsites' wire format. */
function aWebsite(site: Site, server: { id: string; name: string }): Buffer {
  const proxy = Buffer.concat([
    site.url ? bytes(1, site.url) : Buffer.alloc(0),
    site.port ? number(2, site.port) : Buffer.alloc(0),
  ]);
  const seconds = Math.floor(Date.now() / 1000) - site.daysAgo * 86_400;
  return bytes(
    1,
    Buffer.concat([
      bytes(1, site.id),
      bytes(2, site.domain.replaceAll(".", "-")),
      bytes(3, site.domain),
      bytes(4, site.root ? bytes(2, bytes(1, site.root)) : bytes(1, proxy)),
      bytes(5, site.environment),
      site.service ? bytes(6, site.service) : Buffer.alloc(0),
      bytes(7, `rte_${site.id}`),
      bytes(8, number(1, seconds)),
      bytes(9, server.id),
      bytes(10, server.name),
    ]),
  );
}

/** Its domain in ListDomains' wire format, with a certificate for so many days, or none behind a tunnel. */
function itsDomain(site: Site, server: { id: string; name: string }, days?: number): Buffer {
  const tunnel = days === undefined;
  const route = Buffer.concat([
    bytes(1, `rte_${site.id}`),
    bytes(2, "site"),
    site.port ? Buffer.alloc(0) : bytes(4, "web"),
    site.port ? Buffer.alloc(0) : number(5, 8080),
    tunnel ? bytes(7, "off") : Buffer.alloc(0),
    site.port ? number(8, site.port) : Buffer.alloc(0),
  ]);
  const certificate = tunnel
    ? number(1, NONE)
    : Buffer.concat([number(1, ISSUED), bytes(4, number(1, Math.floor(Date.now() / 1000) + days * 86_400))]);
  const status = Buffer.concat([bytes(1, site.domain), bytes(2, certificate)]);
  const on = Buffer.concat([bytes(1, server.id), bytes(2, server.name), bytes(3, status)]);
  const scheme = tunnel ? "http" : "https";
  return bytes(
    1,
    Buffer.concat([
      bytes(1, site.domain),
      bytes(2, site.environment),
      bytes(3, `${scheme}://${site.domain}`),
      bytes(4, route),
      bytes(5, on),
    ]),
  );
}

/** What's on the page beyond its width, and what's clipped of what a row says. */
function howItFits(page: Page) {
  return page.evaluate(() => ({
    page: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    clipped: [...document.querySelectorAll(".websites-domain")]
      .filter((name) => name.scrollWidth > name.clientWidth + 1)
      .map((name) => name.textContent),
  }));
}

export async function websitesFlow(page: Page, { origin, step, shoot }: WebsitesFlowOptions) {
  const rail = page.getByRole("navigation", { name: "Main" });
  const heading = page.getByRole("heading", { level: 1, name: "Websites" });

  step("websites: the rail leads to it, and with none it offers the first");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/apps`);
  await rail.getByRole("link", { name: "Websites" }).click();
  await heading.waitFor();
  if ((await rail.getByRole("link", { name: "Websites" }).getAttribute("aria-current")) !== "page") {
    fail("the rail doesn't mark Websites as where you are");
  }
  await page.getByRole("heading", { name: "No websites yet" }).waitFor();
  await shoot(page, "websites-empty");

  step("websites: a folder follows the domain, and its plan names the folder for a person to approve");
  await page.getByRole("link", { name: "New website" }).click();
  const sheet = page.getByRole("dialog", { name: "New website" });
  await sheet.waitFor();
  if (!(await sheet.getByRole("radio", { name: /^Files in a folder/ }).isChecked())) {
    fail("a new website doesn't start from a folder");
  }
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  await sheet.getByText("Name the domain, such as blog.example.com.").waitFor();
  await sheet.getByText("Give the folder that holds its files, such as /var/www/blog.").waitFor();
  await sheet.getByRole("textbox", { name: "Domain", exact: true }).fill("blog.example.net");
  const folder = sheet.getByRole("textbox", { name: "Folder", exact: true });
  if ((await folder.inputValue()) !== "/var/www/blog.example.net") {
    fail(`the folder doesn't follow the domain: ${await folder.inputValue()}`);
  }
  // With several servers, the sheet asks which, as a website runs on one.
  const where = sheet.getByRole("combobox", { name: "Server" });
  if ((await where.count()) > 0) {
    await sheet.getByText("Choose the server it runs on.").waitFor();
    await where.selectOption({ index: 1 });
  }
  // What's at the folder is read on its server, which with several is the one chosen.
  await sheet.getByText(/Nothing is at/).waitFor({ timeout: 15_000 });
  await shoot(page, "website-new");
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("/var/www/blog.example.net").first().waitFor();
  await shoot(page, "website-plan-folder");
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await sheet.waitFor();

  step("websites: one to a URL reaches its plan, with its web server and its route, and needs no person");
  await sheet.getByRole("radio", { name: /^A URL/ }).check();
  await sheet.getByRole("textbox", { name: "URL", exact: true }).fill("https://backend.example.net/blog");
  await shoot(page, "website-new-url");
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("blog.example.net").first().waitFor();
  await plan.getByText("web", { exact: true }).first().waitFor();
  if ((await plan.getByText(/approve|a person/i).count()) > 0 && (await plan.getByText("127.0.0.1").count()) > 0) {
    fail("a website to a URL asks a person to approve a port on the server");
  }
  await shoot(page, "website-plan");
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await sheet.waitFor();
  if (
    (await sheet.getByRole("textbox", { name: "URL", exact: true }).inputValue()) !== "https://backend.example.net/blog"
  ) {
    fail("cancelling the plan loses the URL");
  }

  step("websites: one to a port on the server behind a tunnel, which the plan says a person approves");
  await sheet.getByRole("radio", { name: /A port on this server/ }).check();
  await sheet.getByRole("textbox", { name: "Port on the server", exact: true }).fill("3000");
  await sheet.getByRole("switch", { name: /tunnel/i }).check();
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan
    .getByText(/127\.0\.0\.1:3000/)
    .first()
    .waitFor();
  await shoot(page, "website-plan-port");
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await sheet.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("heading", { name: "No websites yet" }).waitFor();

  const read = async <T>(path: string): Promise<T> => (await (await page.request.get(`${origin}${path}`)).json()) as T;
  const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
  const server = servers[0] ?? fail("no server is there");
  const sites: Site[] = [
    {
      id: "prj_blog",
      domain: "blog.example.net",
      url: "https://backend.example.net/blog",
      environment: "env_blog",
      service: "svc_blogweb",
      daysAgo: 6,
    },
    { id: "prj_tools", domain: "tools.example.net", port: 3000, environment: "env_tools", daysAgo: 9 },
    {
      id: "prj_docs",
      domain: "docs.example.net",
      root: "/var/www/docs",
      environment: "env_docs",
      service: "svc_docsweb",
      daysAgo: 12,
    },
    {
      id: "prj_long",
      domain: "a-website-with-a-name-long-enough-to-wrap.e2e-dockwell.example.net",
      url: "http://10.0.0.5:8080",
      environment: "env_long",
      service: "svc_longweb",
      daysAgo: 24,
    },
  ];
  const days: Record<string, number | undefined> = { prj_blog: 61, prj_tools: undefined, prj_long: 88, prj_docs: 40 };
  const websites = /WebsiteService\/ListWebsites/;
  const domains = /DomainService\/ListDomains/;
  await page.route(websites, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/proto",
      body: Buffer.concat(sites.map((site) => aWebsite(site, server))),
    }),
  );
  await page.route(domains, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/proto",
      body: Buffer.concat(sites.map((site) => itsDomain(site, server, days[site.id]))),
    }),
  );

  step("websites: each with what it sends to, what it is, its certificate and when it changed");
  await page.reload();
  const row = (name: string) => page.getByRole("row", { name: new RegExp(`^${name.replaceAll(".", "\\.")}`) });
  await row("blog.example.net").getByText("https://backend.example.net/blog").waitFor();
  await row("blog.example.net").getByText("Valid").waitFor({ timeout: 15_000 });
  await row("blog.example.net").getByText("· 61 days left").waitFor();
  await row("tools.example.net").getByText("Port 3000 on the server").waitFor();
  await row("tools.example.net").getByText("TLS ends in front").waitFor();
  await page
    .getByText(/^4 sites( on |$)/)
    .first()
    .waitFor();
  await shoot(page, "websites");

  step("websites: at 1440 px, 800 px and on a phone it fits, with every domain whole");
  for (const width of [1440, 800, 390]) {
    await page.setViewportSize({ width, height: 844 });
    await page
      .getByText(sites[2]?.domain ?? "")
      .first()
      .waitFor();
    const fits = await howItFits(page);
    if (fits.page > 0) fail(`at ${width} px the page is ${fits.page} px too wide`);
    if (fits.clipped.length > 0) fail(`at ${width} px these domains are cut: ${fits.clipped.join(", ")}`);
  }
  await page.setViewportSize({ width: 1440, height: 900 });

  step(
    "websites: a website's page shows what its web server runs, from the control plane, and marks what an edit changes",
  );
  await row("blog.example.net").click({ position: { x: 300, y: 12 } });
  await page.waitForURL(/\/websites\/prj_blog\/configuration$/);
  await page.getByRole("heading", { level: 1, name: "blog.example.net" }).waitFor();
  const code = page.getByRole("region", { name: "The web server's config" });
  await code.getByText("reverse_proxy https://backend.example.net").waitFor({ timeout: 15_000 });
  await code.getByText("rewrite * /blog{uri}").waitFor();
  if ((await code.locator(".is-changed").count()) !== 0) fail("an unedited config marks lines as changed");
  const visit = await page.getByRole("link", { name: "Visit site" }).getAttribute("href");
  if (visit !== "https://blog.example.net") fail(`Visit site goes to ${visit}`);
  await shoot(page, "website-configuration");
  await page.getByRole("textbox", { name: "URL", exact: true }).fill("https://backend.example.net/news");
  await code.getByText("rewrite * /news{uri}").waitFor({ timeout: 15_000 });
  if ((await code.locator(".is-changed").count()) !== 1)
    fail("an edit of the path doesn't mark the one line it changes");
  await page.getByText("1 change", { exact: true }).waitFor();
  await page.getByText("Was https://backend.example.net/blog").waitFor();
  await shoot(page, "website-configuration-edited");
  await page.getByRole("textbox", { name: "URL", exact: true }).fill("http://localhost");
  await page.getByText("Not valid").waitFor({ timeout: 15_000 });
  await page
    .getByText(/is the web server's own container/)
    .first()
    .waitFor();
  if (!(await page.getByRole("button", { name: /Review plan/ }).isDisabled()))
    fail("a URL that won't do can be planned");
  await page.getByRole("button", { name: "Discard" }).click();
  await code.getByText("rewrite * /blog{uri}").waitFor();

  step("websites: a folder's page shows its file server, from the control plane, and leads to its files");
  await page.goto(`${origin}/websites/prj_docs`);
  await page.waitForURL(/\/websites\/prj_docs\/configuration$/);
  await page.getByText("Static files from /var/www/docs").first().waitFor();
  await code.getByText("root * /srv").waitFor({ timeout: 15_000 });
  await code.getByText("file_server {").waitFor();
  await shoot(page, "website-folder");
  const edit = await page.getByRole("link", { name: "Edit files" }).getAttribute("href");
  if (edit !== `/servers/${server.id}/files?path=%2Fvar%2Fwww%2Fdocs`) fail(`Edit files goes to ${edit}`);

  step("websites: one to a port says how the ingress reaches it, and that nothing of its own logs");
  await page.goto(`${origin}/websites/prj_tools`);
  await page.waitForURL(/\/websites\/prj_tools\/configuration$/);
  await page.getByRole("heading", { level: 2, name: "Route" }).waitFor();
  await page.getByText("127.0.0.1:3000").waitFor();
  await shoot(page, "website-port");
  await page.getByRole("link", { name: "Logs" }).click();
  await page.getByRole("heading", { name: "Nothing here writes logs" }).waitFor();

  step("websites: in Chinese, the list and a website's page read whole");
  const browser = page.context().browser() ?? fail("the page has no browser to open another context in");
  const zh = await browser.newContext({ locale: "zh-CN", storageState: await page.context().storageState() });
  const chinese = await zh.newPage();
  try {
    await chinese.route(websites, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/proto",
        body: Buffer.concat(sites.map((site) => aWebsite(site, server))),
      }),
    );
    await chinese.route(domains, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/proto",
        body: Buffer.concat(sites.map((site) => itsDomain(site, server, days[site.id]))),
      }),
    );
    await chinese.goto(`${origin}/websites`);
    await chinese.getByRole("heading", { level: 1, name: "网站" }).waitFor();
    await chinese.getByText("反向代理").first().waitFor();
    await shoot(chinese, "websites-zh");
    await chinese.goto(`${origin}/websites/prj_blog/configuration`);
    await chinese
      .getByRole("region", { name: "Web 服务器的配置" })
      .getByText("reverse_proxy")
      .waitFor({ timeout: 15_000 });
    await shoot(chinese, "website-configuration-zh");
  } finally {
    await zh.close();
  }
  await page.unroute(websites);
  await page.unroute(domains);
  step("  websites: listed, planned, configured and read, on a desktop, a phone and in Chinese");
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 65, run: (page, run) => websitesFlow(page, run) };
