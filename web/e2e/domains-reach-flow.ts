// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { rmSync } from "node:fs";
import { join } from "node:path";

import { flow as connectCloudflare } from "./dns-providers-flow.ts";
import type { Flow } from "./flows.ts";
import { runCloudflared } from "./publish-domain-flow.ts";
import { atWidth, LONG_HOSTNAME, onTop } from "./sheets.ts";

export const flow: Flow = {
  order: 99.25,
  run: async (page, run) => {
    const { bin, data, origin, step, shoot } = run;
    const proc = process.env["DOCKWELL_TEST_PROC"];
    if (!proc) return;
    const read = async <T>(path: string): Promise<T> =>
      (await (await page.request.get(`${origin}${path}`)).json()) as T;
    const providers = async () =>
      (await read<{ providers?: { everyProject?: boolean; projects?: string[] }[] }>("/v1/dnsProviders")).providers ??
      [];
    if ((await providers()).length === 0) await connectCloudflare.run(page, run);
    if (!(await providers())[0]?.everyProject)
      throw new Error("Cloudflare should reach every project after connecting from Integrations");
    const cli = (...args: string[]) =>
      execFileSync(bin, [...args, "--data-dir", data], {
        encoding: "utf8",
        env: {
          ...process.env,
          USER: "mira",
          DOCKWELL_URL: "",
          DOCKWELL_TOKEN: "",
          XDG_CONFIG_HOME: join(data, "cli-reach"),
        },
      });
    cli("login", "--local");
    cli("services", "create", "reach-web", "--project", "default", "--app-image", "nginx:1.27-alpine", "--yes");
    const { services } = await read<{ services: { id: string; resource: { name: string } }[] }>("/v1/services");
    const app = services.find((s) => s.resource.name === "reach-web");
    const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
    const server = servers.find((s) => s.name === LONG_HOSTNAME);
    if (!app || !server) throw new Error("the test app and server must exist");
    cli("servers", "set-public-address", server.id, "--address", "203.0.113.8", "--yes");
    const setReach = (...args: string[]) =>
      cli(
        "dns-providers",
        "update",
        "cloudflare",
        "--provider",
        "cloudflare",
        "--zones",
        "example.com",
        "--zones",
        "example.org",
        ...args,
        "--yes",
      );
    runCloudflared(proc);
    try {
      setReach();
      if (!(await providers())[0]?.everyProject)
        throw new Error("an update that leaves out --every-project should keep every-project reach");
      setReach("--every-project=false");
      step("domains reach: repair a provider that serves no project in the app's sheet");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/apps/${app.id}`);
      await page.getByRole("button", { name: "Add a domain" }).first().click();
      const add = page.getByRole("dialog", { name: "Add a domain to reach-web" });
      await add.getByRole("textbox", { name: "Domain", exact: true }).fill("blog.example.com");
      await add.getByText("Cloudflare doesn't serve this project").waitFor();
      if (await add.getByText("Make this record at your DNS provider.").count())
        throw new Error("a connected zone offered a manual DNS record");
      await add.getByRole("button", { name: "Use cloudflare here" }).click();
      const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await plan.getByRole("button", { name: /^Apply plan/ }).click();
      await page
        .getByRole("dialog", { name: /^(Applied|cloudflare is changed)$/ })
        .getByRole("button", { name: "Done" })
        .click();
      await add.getByText(/^Through the tunnel web-1 on /).waitFor();
      if ((await providers())[0]?.projects?.length !== 1) throw new Error("the repair should add only this project");
      await shoot(page, "domains-reach-app-after");
      await add.getByRole("button", { name: "Review plan" }).click();
      const publish = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await publish.getByText("Write the DNS record for blog.example.com at Cloudflare").waitFor();
      await publish.getByText("Add *.example.com to the tunnel web-1 at Cloudflare").waitFor();
      await publish.getByRole("button", { name: /Cancel/ }).click();
      await add.getByRole("button", { name: "Cancel", exact: true }).click();

      step(
        "domains reach: the Domains sheet offers the same tunnel and direct choices after selecting an app and port",
      );
      // The Domains sheet has a selector when there is more than one app.
      cli("services", "create", "reach-other", "--project", "default", "--app-image", "nginx:1.27-alpine", "--yes");
      await page.goto(`${origin}/domains`);
      await page.getByRole("button", { name: "Add a domain" }).click();
      const domains = page.getByRole("dialog", { name: "Add a domain", exact: true });
      await domains.getByRole("combobox", { name: "App", exact: true }).selectOption(app.id);
      const port = domains.getByLabel("Port in the app");
      if (await port.evaluate((el) => el.tagName === "SELECT")) await port.selectOption("80");
      else await port.fill("80");
      await domains.getByRole("textbox", { name: "Subdomain", exact: true }).fill("edge.example.com");
      await domains.getByText(/^Through the tunnel web-1 on /).waitFor();
      await shoot(page, "domains-reach-domains-after");
      await domains.getByRole("button", { name: "Change the way in" }).click();
      await domains.getByRole("radio", { name: /server's own HTTPS/i }).click();
      await domains.getByRole("button", { name: "Review plan" }).click();
      const direct = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await direct.getByText("Write the DNS record for edge.example.com at Cloudflare").waitFor();
      if (await direct.getByText("Add *.example.com to the tunnel web-1 at Cloudflare").count())
        throw new Error("direct HTTPS offered a tunnel rule");
      await direct.getByRole("button", { name: /Cancel/ }).click();
      await domains.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      setReach("--every-project");
      cli("servers", "set-public-address", server.id, "--address", "", "--yes");
      rmSync(proc, { recursive: true, force: true });
    }
  },
};
