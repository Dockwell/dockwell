// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  AutomaticUpdatesSchema,
  BuildInfoSchema,
  DockwellReleaseSchema,
  GetServerUpdatesResponseSchema,
  PackageEffect,
  PackageManager,
  PackageUpdateSchema,
  ServerInventorySchema,
  ServerUpdatesSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { releaseStanding, splitVersion, summarize } from "./updates";

const NOW = new Date(2026, 10, 7, 12, 0);

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  inventory: create(ServerInventorySchema, { hostname: "hz-1", osName: "Ubuntu 24.04.1 LTS", dockerVersion: "27.3.1" }),
  agentBuild: create(BuildInfoSchema, { version: "v0.9.4" }),
});

const pkg = (p: MessageInitShape<typeof PackageUpdateSchema>) =>
  create(PackageUpdateSchema, { architecture: "amd64", ...p });

const waiting = [
  pkg({
    name: "libssh-4",
    summary: "tiny C SSH library",
    installed: "0.10.6-2build2",
    available: "0.10.6-2ubuntu0.1",
    origin: "noble-security",
    security: true,
  }),
  pkg({
    name: "linux-image-6.8.0-49-generic",
    summary: "Signed kernel image generic",
    available: "6.8.0-49.49",
    origin: "noble-security",
    security: true,
    effect: PackageEffect.REBOOT,
  }),
  pkg({
    name: "openssl",
    summary: "Secure Sockets Layer toolkit",
    installed: "3.0.13-0ubuntu3.4",
    available: "3.0.13-0ubuntu3.5",
    origin: "noble-security",
    security: true,
  }),
  pkg({
    name: "docker-ce",
    installed: "5:27.3.1-1~ubuntu.24.04~noble",
    available: "5:27.4.0-1~ubuntu.24.04~noble",
    origin: "noble",
    effect: PackageEffect.DOCKER_RESTART,
  }),
  pkg({ name: "git", installed: "1:2.43.0-1ubuntu7.1", available: "1:2.43.0-1ubuntu7.2", origin: "noble-updates" }),
  pkg({
    name: "systemd",
    installed: "255.4-1ubuntu8.4",
    available: "255.4-1ubuntu8.5",
    origin: "noble-updates",
    effect: PackageEffect.REBOOT,
  }),
];

function answer(updates: MessageInitShape<typeof ServerUpdatesSchema> = {}, newest = "0.9.4") {
  return create(GetServerUpdatesResponseSchema, {
    updates: create(ServerUpdatesSchema, {
      serverId: "srv_1",
      manager: PackageManager.APT,
      marksSecurity: true,
      packages: waiting,
      held: ["python3-distupgrade"],
      listsTime: timestampFromDate(new Date(NOW.getTime() - 20 * 60_000)),
      readTime: timestampFromDate(NOW),
      automatic: create(AutomaticUpdatesSchema, { tool: "unattended-upgrades", on: true }),
      ...updates,
    }),
    dockwell: create(DockwellReleaseSchema, { agent: "v0.9.4", controlPlane: "v0.9.4", newest }),
  });
}

async function renderUpdates(get: () => ReturnType<typeof answer>, path = "/servers/srv_1/updates", check = vi.fn()) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const getServerUpdates = vi.fn(get);
  const rendered = renderAt(path, {
    servers: { watchServers: watch.stream },
    updates: { getServerUpdates, checkServerUpdates: check },
  });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, getServerUpdates, check };
}

afterEach(() => {
  vi.useRealTimers();
  activateLocale("en");
});

/** The callout whose words hold text. */
async function callout(text: string | RegExp) {
  const found = (await screen.findByText(text, { exact: false, selector: ".dw-callout *" })).closest(".dw-callout");
  if (!(found instanceof HTMLElement)) throw new Error(`no callout says ${String(text)}`);
  return found;
}

describe("summarize", () => {
  it("sorts what waits into security fixes, the rest, and Docker's engine", () => {
    const s = summarize(answer().updates);
    expect(s.security.map((p) => p.name)).toEqual(["libssh-4", "linux-image-6.8.0-49-generic", "openssl"]);
    expect(s.other.map((p) => p.name)).toEqual(["git", "systemd"]);
    expect(s.docker?.name).toBe("docker-ce");
    expect(s.total).toBe(6);
    expect(s.rebooting.map((p) => p.name)).toEqual(["linux-image-6.8.0-49-generic", "systemd"]);
    expect(s.securityFrom).toEqual(["noble-security"]);
    expect(s.otherFrom).toEqual(["noble-updates"]);
  });

  it("marks where a version starts to change", () => {
    expect(splitVersion("3.0.13-0ubuntu3.4", "3.0.13-0ubuntu3.5")).toEqual(["3.0.13-0ubuntu3.", "5"]);
    expect(splitVersion("0.10.6-2build2", "0.10.6-2ubuntu0.1")).toEqual(["0.10.6-", "2ubuntu0.1"]);
    expect(splitVersion("", "6.8.0-49.49")).toEqual(["", "6.8.0-49.49"]);
  });

  it("says whether Dockwell is its newest release", () => {
    const r = (controlPlane: string, newest: string) =>
      releaseStanding(create(DockwellReleaseSchema, { controlPlane, newest }));
    expect(r("v0.9.4", "0.9.4")).toBe("current");
    expect(r("0.9.4", "0.9.5")).toBe("behind");
    expect(r("0.10.0", "0.9.5")).toBe("current");
    expect(r("dev", "0.9.5")).toBe("unknown");
    expect(r("0.9.4", "")).toBe("unknown");
  });
});

describe("UpdatesPage", () => {
  it("leads with the security fixes, then the rest, as Server-Updates has them", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    const { container } = await renderUpdates(() => answer());
    const lead = await callout("3 security updates are ready to install");
    expect(lead).toHaveAttribute("role", "status");
    expect(lead).toHaveTextContent("One of them needs a reboot to take effect");
    expect(lead).toHaveTextContent("run sudo apt-get upgrade on hz-1");
    expect(screen.getByText("3 from noble-security · checked 20 min ago")).toBeVisible();
    const table = screen.getByRole("table", { name: "Security updates for hz-1" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector(".updates-name")?.textContent)).toEqual([
      "libssh-4",
      "linux-image-6.8.0-49-generic",
      "openssl",
    ]);
    expect(rows[1]).toHaveTextContent("New");
    expect(rows[1]).toHaveTextContent("Needs a reboot");
    expect(rows[2]?.querySelector(".updates-changed")).toHaveTextContent(/^5$/);
    // The tab says how many security fixes wait, as a warning.
    const tab = screen.getByRole("link", { name: "Updates 3" });
    expect(tab).toHaveAttribute("aria-current", "page");
    expect(tab.querySelector(".dw-tab-count")).toHaveClass("dw-tone-warning");
    await expectAccessible(container);
  });

  it("folds the other updates away until asked", async () => {
    const user = userEvent.setup();
    await renderUpdates(() => answer());
    const toggle = await screen.findByRole("button", { name: /Other updates/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent("From noble-updates, 1 needs a reboot · git, systemd");
    expect(screen.queryByRole("table", { name: "Other updates for hz-1" })).toBeNull();
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("table", { name: "Other updates for hz-1" })).toBeVisible();
    expect(screen.getByText(/1 more is held back: python3-distupgrade/)).toBeVisible();
  });

  it("says how the server updates itself, and Docker's and Dockwell's versions", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderUpdates(() => answer({ dockerLiveRestore: false }, "0.9.5"));
    const auto = await screen.findByRole("region", { name: "Automatic updates" });
    expect(auto).toHaveTextContent("unattended-upgrades installs updates");
    expect(auto).toHaveTextContent("Refreshed 20 min ago");
    const versions = screen.getByRole("region", { name: "Docker and Dockwell" });
    expect(versions).toHaveTextContent("27.3.1 → 27.4.0 available");
    expect(versions).toHaveTextContent("Every container restarts with it");
    expect(versions).toHaveTextContent("0.9.4 · agent and control plane");
    expect(within(versions).getByText("0.9.5 available")).toBeVisible();
    expect(versions).toHaveTextContent("Update with the install script");
  });

  it("refreshes the lists when asked, and asks again while it checks", async () => {
    const user = userEvent.setup();
    let checking = false;
    const check = vi.fn(() => {
      checking = true;
      return { updates: answer({ checking: true }).updates };
    });
    const { getServerUpdates } = await renderUpdates(
      () => answer({ checking, refreshing: checking }),
      undefined,
      check,
    );
    await user.click(await screen.findByRole("button", { name: "Check now" }));
    expect(check).toHaveBeenCalledWith(expect.objectContaining({ id: "srv_1" }), expect.anything());
    expect(await screen.findByRole("button", { name: "Refreshing lists" })).toHaveAttribute("aria-disabled", "true");
    const asked = getServerUpdates.mock.calls.length;
    checking = false;
    await waitFor(() => expect(getServerUpdates.mock.calls.length).toBeGreaterThan(asked), { timeout: 4000 });
    expect(await screen.findByRole("button", { name: "Check now" })).not.toHaveAttribute("aria-disabled");
  });

  it("says why a server's updates can't be read", async () => {
    await renderUpdates(() =>
      create(GetServerUpdatesResponseSchema, {
        updates: create(ServerUpdatesSchema, {
          serverId: "srv_1",
          unavailable: "Dockwell reads updates with apt, dnf or apk, and Arch Linux uses none of them",
        }),
      }),
    );
    expect(await callout("Dockwell can't read updates on hz-1")).toHaveTextContent("Arch Linux uses none of them");
    expect(screen.getByRole("link", { name: "Updates" })).toBeVisible();
  });

  it("marks nothing where the distribution doesn't, as Alpine", async () => {
    await renderUpdates(() =>
      answer({
        manager: PackageManager.APK,
        marksSecurity: false,
        packages: [pkg({ name: "busybox", installed: "1.36.1-r29", available: "1.36.1-r30" })],
        held: [],
      }),
    );
    expect(
      await screen.findByText("apk doesn't say which updates fix security issues, so none is marked here."),
    ).toBeVisible();
    expect(screen.getByRole("table", { name: "Updates for hz-1" })).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Security updates" })).toBeNull();
    expect(screen.getByRole("link", { name: "Updates 1" }).querySelector(".dw-tab-count")).not.toHaveClass(
      "dw-tone-warning",
    );
  });

  it("asks for a reboot the server needs", async () => {
    await renderUpdates(() => answer({ rebootRequired: true, rebootPackages: ["linux-base"], packages: [] }));
    expect(await callout("hz-1 needs a reboot")).toHaveTextContent(
      "What linux-base changed takes effect only once hz-1 restarts",
    );
    expect(screen.getByText("hz-1 is up to date, as far as its lists say.")).toBeVisible();
  });

  it("lists a package a row on a phone, and scrolls nothing sideways", async () => {
    onPhone();
    const { container } = await renderUpdates(() => answer());
    const list = await screen.findByRole("list", { name: "Security updates for hz-1" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("0.10.6-2build2→0.10.6-2ubuntu0.1");
    await expectAccessible(container);
  });

  it("speaks Chinese", async () => {
    activateLocale("zh-CN");
    await renderUpdates(() => answer());
    expect(await screen.findByRole("heading", { name: "安全更新" })).toBeVisible();
    expect(await callout("3 个安全更新可以安装")).toBeVisible();
  });
});

describe("the overview's Updates card", () => {
  it("names the security fixes, and Dockwell's version", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderUpdates(() => answer(), "/");
    const card = await screen.findByRole("region", { name: "Updates" });
    expect(card).toHaveTextContent("Checked 20 min ago");
    expect(card).toHaveTextContent("3 security updates for Ubuntu");
    expect(card).toHaveTextContent("libssh-4, linux-image-6.8.0-49-generic, openssl. Needs a reboot.");
    expect(card).toHaveTextContent("Dockwell 0.9.4 is current");
    expect(within(card).getByRole("link", { name: "Review updates" })).toHaveAttribute(
      "href",
      "/servers/srv_1/updates",
    );
  });

  it("is one link on a phone", async () => {
    onPhone();
    await renderUpdates(() => answer(), "/");
    const link = await screen.findByRole("link", { name: /3 security updates for Ubuntu/ });
    expect(link).toHaveAttribute("href", "/servers/srv_1/updates");
    expect(link).toHaveTextContent("Review");
  });

  it("stays away while a server can't be asked", async () => {
    const { getServerUpdates } = await renderUpdates(() => {
      throw new ConnectError("the control plane is restarting", Code.Unavailable);
    }, "/");
    await waitFor(() => expect(getServerUpdates).toHaveBeenCalled());
    await act(() => Promise.resolve());
    expect(screen.queryByRole("region", { name: "Updates" })).toBeNull();
  });
});
