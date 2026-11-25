// SPDX-License-Identifier: AGPL-3.0-only

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { atWidth } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const ISSUED = 2;

interface HandySite {
  id: string;
  domain: string;
  root?: string;
  php?: string;
  phpService?: string;
  url?: string;
  environment: string;
  service: string;
  paused?: boolean;
  daysAgo: number;
}

function wireWebsite(site: HandySite, server: { id: string; name: string }): Buffer {
  const serves =
    site.php && site.root
      ? bytes(3, Buffer.concat([bytes(1, site.root), bytes(2, site.php)]))
      : site.root
        ? bytes(2, bytes(1, site.root))
        : bytes(1, bytes(1, site.url ?? "https://backend.example.com"));
  const spec = Buffer.concat([serves, site.paused ? Buffer.concat([number(5, 1), number(6, 1)]) : Buffer.alloc(0)]);
  const seconds = Math.floor(Date.now() / 1000) - site.daysAgo * 86_400;
  return bytes(
    1,
    Buffer.concat([
      bytes(1, site.id),
      bytes(2, site.domain.replaceAll(".", "-")),
      bytes(3, site.domain),
      bytes(4, spec),
      bytes(5, site.environment),
      bytes(6, site.service),
      bytes(7, `rte_${site.id}`),
      bytes(8, number(1, seconds)),
      bytes(9, server.id),
      bytes(10, server.name),
      site.phpService ? bytes(11, site.phpService) : Buffer.alloc(0),
    ]),
  );
}

function wireDomain(site: HandySite, server: { id: string; name: string }, days: number): Buffer {
  const route = Buffer.concat([bytes(1, `rte_${site.id}`), bytes(2, "site"), bytes(4, "web"), number(5, 8080)]);
  const expireAt = Math.floor(Date.now() / 1000) + days * 86_400 - 60;
  const notBefore = Math.floor(Date.now() / 1000) - 5 * 86_400;
  const certificate = Buffer.concat([
    number(1, ISSUED),
    bytes(4, number(1, expireAt)),
    bytes(122, "Let's Encrypt E5"),
    bytes(123, site.domain),
    bytes(124, site.domain),
    bytes(125, number(1, notBefore)),
  ]);
  const status = Buffer.concat([bytes(1, site.domain), bytes(2, certificate)]);
  const on = Buffer.concat([bytes(1, server.id), bytes(2, server.name), bytes(3, status)]);
  return bytes(
    1,
    Buffer.concat([
      bytes(1, site.domain),
      bytes(2, site.environment),
      bytes(3, `https://${site.domain}`),
      bytes(4, route),
      bytes(5, on),
    ]),
  );
}

async function checkNoOverflow(page: Page, width: number, label: string) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  if (overflow > 1) {
    throw new Error(`${label} overflows ${width}px viewport by ${overflow}px`);
  }
}

export const flow: Flow = {
  order: 66,
  async run(page, { origin, step, shoot }) {
    const read = async <T>(path: string): Promise<T> =>
      (await (await page.request.get(`${origin}${path}`)).json()) as T;
    const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
    const server = servers[0] ?? { id: "srv_own", name: "own-server" };
    const sites: HandySite[] = [
      {
        id: "prj_handy_php",
        domain: "shop.example.com",
        root: "/var/www/shop",
        php: "8.3",
        phpService: "svc_handy_php",
        environment: "env_handy_php",
        service: "svc_handy_web",
        daysAgo: 2,
      },
      {
        id: "prj_handy_static",
        domain: "docs.example.com",
        root: "/var/www/docs",
        environment: "env_handy_static",
        service: "svc_handy_docs",
        paused: true,
        daysAgo: 4,
      },
    ];

    const websitesRoute = /WebsiteService\/ListWebsites/;
    const domainsRoute = /DomainService\/ListDomains/;

    await page.route(websitesRoute, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/proto",
        body: Buffer.concat(sites.map((s) => wireWebsite(s, server))),
      }),
    );
    await page.route(domainsRoute, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/proto",
        body: Buffer.concat(sites.map((s) => wireDomain(s, server, 75))),
      }),
    );

    try {
      step("websites-handy: list shows paused badge, row menu actions and batch actions");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/websites`);
      await page.getByRole("link", { name: "shop.example.com" }).first().waitFor();
      await page.getByText("Paused", { exact: true }).waitFor();

      // Open row menu for shop.example.com and verify Files, Logs, Pause, Back up now.
      await page.getByRole("button", { name: "Actions for shop.example.com" }).click();
      const menu = page.getByRole("menu");
      await menu.getByRole("menuitem", { name: "Files" }).waitFor();
      await menu.getByRole("menuitem", { name: "Logs" }).waitFor();
      await menu.getByRole("menuitem", { name: "Pause" }).waitFor();
      await menu.getByRole("menuitem", { name: "Back up now" }).waitFor();
      await page.keyboard.press("Escape");

      // Select both sites and verify batch actions bar (Pause, Resume, Switch PHP, HTTPS, Delete).
      await page.getByRole("checkbox", { name: "Select shop.example.com" }).check();
      await page.getByRole("checkbox", { name: "Select docs.example.com" }).check();
      const toolbar = page.getByRole("toolbar", { name: "Actions for the rows selected" });
      await toolbar.getByRole("button", { name: "Pause", exact: true }).waitFor();
      await toolbar.getByRole("button", { name: "Resume", exact: true }).waitFor();
      await toolbar.getByRole("button", { name: "Switch PHP", exact: true }).waitFor();
      await toolbar.getByRole("button", { name: "HTTPS", exact: true }).waitFor();
      await shoot(page, "websites-handy-list");
      await atWidth(page, { width: 1440, height: 900 });
      await checkNoOverflow(page, 1440, "websites list");
      await atWidth(page, { width: 390, height: 844 });
      await checkNoOverflow(page, 390, "websites list");

      step("websites-handy: overview shows paused banner and header pause/resume + backups card");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/websites/prj_handy_static`);
      await page.getByText("This website is paused").waitFor();
      await page.getByRole("button", { name: "Back up now" }).waitFor();
      await shoot(page, "websites-handy-paused");
      await atWidth(page, { width: 1440, height: 900 });
      await checkNoOverflow(page, 1440, "websites paused");
      await atWidth(page, { width: 390, height: 844 });
      await checkNoOverflow(page, 390, "websites paused");

      step("websites-handy: configuration rules, PHP tuning, and custom Caddy generate live Caddyfile");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/websites/prj_handy_php/configuration`);
      await page.getByRole("heading", { name: "Rules" }).waitFor();
      await page.getByRole("switch", { name: /CORS/ }).click();
      await page.getByPlaceholder("https://app.example.com").fill("https://app.example.com");
      await page.getByRole("switch", { name: /Static file caching/ }).click();
      await page
        .getByRole("region", { name: "The web server's config" })
        .getByText("Access-Control-Allow-Origin")
        .waitFor({ timeout: 15_000 });
      await shoot(page, "websites-handy-config");
      await atWidth(page, { width: 1440, height: 900 });
      await checkNoOverflow(page, 1440, "websites config");
      await atWidth(page, { width: 390, height: 844 });
      await checkNoOverflow(page, 390, "websites config");

      step("websites-handy: certificate tab offers Renew now, Upload certificate, Download certificate and key");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/websites/prj_handy_php/certificate`);
      await page.getByText("Let's Encrypt E5").waitFor();
      await page.getByRole("button", { name: "Renew now" }).waitFor();
      await page.getByRole("button", { name: "Upload certificate" }).waitFor();
      await page.getByRole("button", { name: "Download certificate" }).waitFor();
      await page.getByRole("button", { name: "Download key" }).waitFor();
      await shoot(page, "websites-handy-cert");
      await atWidth(page, { width: 1440, height: 900 });
      await checkNoOverflow(page, 1440, "websites certificate");
      await atWidth(page, { width: 390, height: 844 });
      await checkNoOverflow(page, 390, "websites certificate");
    } finally {
      await atWidth(page, { width: 1440, height: 900 });
      await page.unroute(websitesRoute);
      await page.unroute(domainsRoute);
    }
  },
};
