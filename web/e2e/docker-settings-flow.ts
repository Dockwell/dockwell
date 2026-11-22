// SPDX-License-Identifier: AGPL-3.0-only

import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import { BinaryReader, BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import type { Locator, Page, Route } from "playwright-core";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

const rpc = (name: string) => new RegExp(`/dockwell\\.v1\\.\\w+Service/${name}(\\?|$)`);
const text = WireType.LengthDelimited;
const integer = WireType.Varint;
const restored = "The Docker change could not finish. Previous settings restored";
const exampleServer = "example-server";

function settingsReply() {
  const settings = new BinaryWriter()
    .tag(2, text)
    .string("28.5.2")
    .tag(3, text)
    .string("1.47")
    .tag(4, text)
    .string("/var/run/docker.sock")
    .tag(5, text)
    .string("Rootful")
    .tag(6, text)
    .string("systemd")
    .tag(7, text)
    .string("/etc/docker/daemon.json")
    .tag(8, text)
    .string("/var/lib/docker")
    .tag(9, integer)
    .bool(true)
    .tag(10, integer)
    .bool(false)
    .tag(11, text)
    .string("Classic")
    .tag(12, text)
    .string("https://mirror.example")
    .tag(14, text)
    .fork()
    .tag(1, text)
    .string("json-file")
    .tag(2, integer)
    .uint64(10 * 1024 * 1024)
    .tag(3, integer)
    .uint32(5)
    .join()
    .tag(15, text)
    .fork()
    .tag(1, text)
    .string("192.0.2.0/24")
    .tag(2, integer)
    .uint32(28)
    .join()
    .tag(20, integer)
    .uint32(1);
  return new BinaryWriter().tag(1, text).bytes(settings.finish()).finish();
}

function inventoryReply(server: string) {
  return new BinaryWriter()
    .tag(1, text)
    .fork()
    .tag(1, text)
    .fork()
    .tag(1, text)
    .string(server)
    .tag(2, integer)
    .uint32(1)
    .tag(3, text)
    .string("b".repeat(64))
    .join()
    .tag(2, text)
    .string("example-api")
    .tag(3, integer)
    .uint32(1)
    .tag(8, integer)
    .uint32(1)
    .tag(10, text)
    .fork()
    .tag(27, integer)
    .uint32(2)
    .join()
    .join()
    .finish();
}

function impact(restart: boolean) {
  return new BinaryWriter()
    .tag(1, integer)
    .uint32(restart ? 2 : 1)
    .tag(2, integer)
    .bool(true)
    .tag(3, integer)
    .uint32(1)
    .tag(5, text)
    .string("example-api")
    .tag(6, integer)
    .bool(true)
    .tag(7, integer)
    .uint32(restart ? 2 : 1)
    .finish();
}

function changeReply(server: string, kind: "reload" | "restart" | "logging", failed = false) {
  const logging = kind === "logging";
  const resource = new BinaryWriter()
    .tag(1, text)
    .string(logging ? "container" : "docker.daemon")
    .tag(2, text)
    .string(server)
    .tag(3, text)
    .string(logging ? "example-api" : "daemon")
    .finish();
  const activation = new BinaryWriter()
    .tag(1, text)
    .bytes(impact(kind === "restart"))
    .finish();
  const edited = new BinaryWriter()
    .tag(1, text)
    .string("res_example")
    .tag(2, text)
    .bytes(resource)
    .tag(3, integer)
    .uint32(logging ? 4 : 3)
    .tag(6, text)
    .string(server);
  const step = new BinaryWriter()
    .tag(1, text)
    .string("res_example")
    .tag(2, text)
    .bytes(resource)
    .tag(3, integer)
    .uint32(logging ? 4 : 3);
  if (logging) {
    step.tag(4, integer).uint32(3);
  } else {
    edited
      .tag(8, text)
      .fork()
      .tag(1, integer)
      .uint32(400)
      .tag(2, text)
      .string(server)
      .tag(9, integer)
      .bool(true)
      .join();
    edited.tag(401, text).bytes(activation);
    step.tag(401, text).bytes(activation);
  }
  const plan = new BinaryWriter()
    .tag(1, text)
    .string("fixture-plan-fingerprint")
    .tag(2, text)
    .fork()
    .tag(logging ? 3 : 2, integer)
    .uint32(1)
    .join()
    .tag(3, text)
    .bytes(edited.finish())
    .tag(4, text)
    .fork()
    .tag(1, text)
    .string(server)
    .tag(2, text)
    .string(exampleServer)
    .tag(6, text)
    .bytes(step.finish())
    .join()
    .tag(7, integer)
    .bool(!logging);
  const change = new BinaryWriter()
    .tag(1, text)
    .string(`chg_settings_fixture_${kind}`)
    .tag(2, text)
    .string(logging ? "Rotate logs for example-api" : "Change Docker settings")
    .tag(3, integer)
    .uint32(failed ? 4 : 1)
    .tag(7, text)
    .bytes(plan.finish())
    .tag(24, integer)
    .bool(true);
  if (failed) {
    change
      .tag(14, text)
      .string(restored)
      .tag(15, text)
      .fork()
      .tag(1, text)
      .string(server)
      .tag(2, text)
      .string("res_example")
      .tag(3, text)
      .bytes(resource)
      .tag(4, integer)
      .uint32(3)
      .tag(5, integer)
      .uint32(4)
      .tag(6, text)
      .string(restored)
      .join();
  }
  return new BinaryWriter().tag(1, text).bytes(change.finish()).finish();
}

function respond(route: Route, bytes: Uint8Array) {
  return route.fulfill({ status: 200, body: Buffer.from(bytes), contentType: "application/proto" });
}

function hasProof(route: Route) {
  const data = new BinaryReader(route.request().postDataBuffer() ?? new Uint8Array());
  while (data.pos < data.len) {
    const [field, type] = data.tag();
    if (field === 3) return data.string().length > 0;
    data.skip(type);
  }
  return false;
}

async function screenshot(page: Page, dir: string, name: string, card?: Locator) {
  const dark = await page.evaluate(() => matchMedia("(prefers-color-scheme: dark)").matches);
  const edge = await pastTheEdge(page);
  if (edge.wider > 0) throw new Error(`Docker settings exceeds the viewport: ${edge.what}`);
  const options = {
    path: join(dir, `${name}.png`),
    animations: "disabled" as const,
    mask: [page.locator(".page-lead"), page.locator(".topbar-address")],
    maskColor: dark ? "#141614" : "#f1f1ee",
  };
  if (card) await card.screenshot(options);
  else await page.screenshot({ ...options, fullPage: true });
}

async function settingsFixture(page: Page, url: string, server: string, dir?: string) {
  let allow = () => {};
  const loaded = new Promise<void>((done) => {
    allow = done;
  });
  let change: "reload" | "restart" = "reload";
  let refuse = false;
  const routes: [string, (route: Route) => Promise<void>][] = [
    [
      "GetDockerSettings",
      async (route) => {
        await loaded;
        await respond(route, settingsReply());
      },
    ],
    ["ListDockerObjects", (route) => respond(route, inventoryReply(server))],
    [
      "CheckDockerSettings",
      (route) => {
        const answer = new BinaryWriter();
        if (refuse) {
          answer
            .tag(1, text)
            .fork()
            .tag(2, integer)
            .uint32(3)
            .tag(3, text)
            .string("This address pool overlaps the host route on eth0. Choose another subnet.")
            .join();
        } else {
          answer.tag(2, text).bytes(impact(change === "restart"));
          answer.tag(3, integer).uint32(1).tag(3, integer).uint32(2);
        }
        return respond(route, answer.finish());
      },
    ],
    ["ProposeChange", (route) => respond(route, changeReply(server, change))],
    ["PlanDockerContainerLogging", (route) => respond(route, changeReply(server, "logging"))],
    ["DiscardChange", (route) => respond(route, new Uint8Array())],
    [
      "ApplyChange",
      (route) =>
        hasProof(route)
          ? respond(route, changeReply(server, change, true))
          : route.fulfill({
              status: 403,
              contentType: "application/json",
              headers: { "Dockwell-Error": "needs_step_up" },
              json: { code: "permission_denied", message: "Confirm with a passkey" },
            }),
    ],
  ];
  for (const [name, handler] of routes) await page.route(rpc(name), handler);
  const shot = (name: string, card?: Locator) => (dir ? screenshot(page, dir, name, card) : Promise.resolve());
  const panel = (heading: string) =>
    page
      .locator(".docker-settings .dw-panel")
      .filter({ has: page.getByRole("heading", { name: heading, exact: true }) });
  const top = () => onTop(page.getByRole("dialog")).last();
  try {
    await page.goto(url);
    await page.getByRole("heading", { name: "Docker settings" }).waitFor();
    await page.locator(".docker-settings .dw-skeleton").first().waitFor();
    await shot("loading");
    if (dir) {
      await page.getByText("Docker is reading this server's settings", { exact: true }).waitFor();
      await shot("slow-loading");
    }
    allow();
    await page.getByText("1 running container differs", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Add mirror" }).waitFor();
    await shot("loaded");
    await shot("read-only-settings", panel("Read-only settings"));
    await shot("address-pools", panel("Default address pools"));

    await page.getByRole("button", { name: "Add mirror" }).click();
    const mirror = page.getByRole("textbox", { name: "Registry mirror URL 2" });
    await mirror.fill("http://backup-mirror.example");
    await page.getByText("Enter an HTTPS URL without credentials, a query or a fragment.").waitFor();
    const review = page.getByRole("button", { name: "Review changes", exact: true });
    if (!(await review.isDisabled())) throw new Error("an invalid mirror can be reviewed");
    await mirror.fill("https://backup-mirror.example");
    await page.getByText("Server could not reach registry", { exact: true }).waitFor();
    await shot("mirrors", panel("Registry mirrors"));
    await review.click();
    await top().getByText(`Reload Docker on ${exampleServer}`, { exact: true }).waitFor();
    await shot("reload-review");
    await page.keyboard.press("Escape");
    await top().waitFor({ state: "hidden" });

    await page.getByRole("button", { name: "Review containers", exact: true }).click();
    await page.getByRole("link", { name: "example-api" }).waitFor();
    await shot("unbounded-logs", panel("Containers with unbounded logs"));
    await page.getByRole("button", { name: "Review log rotation" }).click();
    await top().getByText("Rotate logs for example-api", { exact: true }).waitFor();
    await shot("log-fix-review");
    await page.keyboard.press("Escape");
    await top().waitFor({ state: "hidden" });
    await page.getByRole("button", { name: "Hide containers" }).click();

    await page.getByText("Change logging defaults", { exact: true }).click();
    await shot("logging", panel("Logging"));
    change = "restart";
    await page.getByRole("textbox", { name: "Maximum files" }).fill("6");
    await page.getByText("Restart Docker", { exact: true }).waitFor();
    await review.click();
    await top().getByText("Live restore is on.", { exact: false }).waitFor();
    await top()
      .getByText("If Docker fails its health check, Dockwell restarts it again with the previous settings.")
      .waitFor();
    await shot("restart-review");
    await top().getByText("Applying this asks for your passkey", { exact: true }).waitFor();
    await top()
      .getByRole("textbox", { name: /type fixture- to apply it/i })
      .fill("fixture-");
    await shot("passkey");
    const passkey = page.waitForResponse((r) => rpc("FinishStepUp").test(r.url()) && r.ok());
    await top()
      .getByRole("button", { name: /^Apply plan/ })
      .click();
    await passkey;
    await top().getByText("Previous settings restored", { exact: true }).first().waitFor();
    await shot("rollback");
    await page.keyboard.press("Escape");
    await top().waitFor({ state: "hidden" });

    refuse = true;
    await page.getByRole("textbox", { name: "Base subnet" }).fill("192.0.2.128/25");
    await page.getByText("This address pool overlaps the host route on eth0.", { exact: false }).waitFor();
    if (!(await review.isDisabled())) throw new Error("refused settings can be reviewed");
    await shot("refused-validation");
  } finally {
    allow();
    for (const [name, handler] of routes) await page.unroute(rpc(name), handler);
  }
}

export const flow: Flow = {
  order: 286,
  async run(page, { origin, step }) {
    step("Docker settings: validate drafts, review reload and restart, and show recovery");
    const path = await ownServerPath(page, origin);
    const url = `${origin}${path}/docker/settings`;
    const server = path.split("/").at(-1) ?? "";
    const shots = process.env["DOCKWELL_E2E_DOCKER_SETTINGS_SHOTS"];
    if (!shots) {
      await settingsFixture(page, url, server);
      return;
    }
    const previous = page.viewportSize();
    try {
      for (const theme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme: theme });
        for (const width of [1440, 390]) {
          const dir = join(shots, `${width}-${theme}`);
          await mkdir(dir, { recursive: true });
          await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
          await settingsFixture(page, url, server, dir);
        }
      }
    } finally {
      await page.emulateMedia({ colorScheme: null });
      if (previous) await atWidth(page, previous);
    }
  },
};
