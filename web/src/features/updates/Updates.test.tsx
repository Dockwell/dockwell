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
import { authError } from "../../test/auth";
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
    dockwell: create(DockwellReleaseSchema, {
      agent: "v0.9.4",
      controlPlane: "v0.9.4",
      newest,
      releases: "https://github.com/Dockwell/dockwell/releases",
    }),
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
    expect(lead).toHaveTextContent("One needs a reboot, and apps and websites go offline while hz-1 restarts.");
    expect(lead).toHaveTextContent(
      "unattended-upgrades installs them within about a day; the command installs every update waiting now.",
    );
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    await user.click(within(lead).getByRole("button", { name: "Copy the upgrade command" }));
    expect(await navigator.clipboard.readText()).toBe("sudo apt-get upgrade --with-new-pkgs");
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
    expect(versions).toHaveTextContent("Containers stop with it, and only those set to restart start again");
    expect(versions).toHaveTextContent("0.9.4 · agent and control plane");
    expect(within(versions).getByText("0.9.5 available")).toBeVisible();
    expect(versions).toHaveTextContent("Update it on the control server, as root, which keeps what runs:");
    expect(within(versions).getByText(/^curl /)).toHaveTextContent(
      /^curl -fsSL https:\/\/github\.com\/Dockwell\/dockwell\/releases\/download\/v0\.9\.5\/install\.sh \| sudo sh -s -- --version 0\.9\.5$/,
    );
    expect(within(versions).getByRole("button", { name: "Copy the command" })).toBeVisible();
  });

  it("names a mirror in the command that updates Dockwell", async () => {
    await renderUpdates(() => {
      const a = answer({}, "0.9.5-rc.2");
      if (a.dockwell) a.dockwell.releases = "https://mirror.example/dockwell/";
      return a;
    });
    const versions = await screen.findByRole("region", { name: "Docker and Dockwell" });
    expect(await within(versions).findByText(/^curl /)).toHaveTextContent(
      "curl -fsSL https://mirror.example/dockwell/download/v0.9.5-rc.2/install.sh | sudo sh -s -- --version 0.9.5-rc.2 --mirror https://mirror.example/dockwell",
    );
  });

  it("says nothing's read yet while the first read runs, and never that it's up to date", async () => {
    await renderUpdates(() =>
      answer({ checking: true, packages: [], readTime: undefined, listsTime: undefined, automatic: undefined }),
    );
    expect(await screen.findByText("Asking hz-1 which updates wait…")).toBeVisible();
    expect(await screen.findByRole("button", { name: "Checking" })).toHaveAttribute("aria-disabled", "true");
    for (const words of [/up to date/i, "None waiting", "Never refreshed", "Not needed", "Security updates"]) {
      expect(screen.queryByText(words)).toBeNull();
    }
    expect(screen.queryByRole("region", { name: "Automatic updates" })).toBeNull();
  });

  it("says a first read failed, with Check now, and nothing it would have found", async () => {
    await renderUpdates(() =>
      answer({
        packages: [],
        readTime: undefined,
        listsTime: undefined,
        reason: "read_failed",
        error:
          "Reading what apt would upgrade ended with status 100: E: The package lists or status file could not be parsed or opened.",
      }),
    );
    const why = await callout("Dockwell couldn't read hz-1's updates");
    expect(why).toHaveTextContent("Reading what apt would upgrade ended with status 100");
    expect(within(why).getByRole("button", { name: "Check now" })).toBeVisible();
    expect(screen.queryByText(/up to date/i)).toBeNull();
    expect(screen.queryByRole("region", { name: "Automatic updates" })).toBeNull();
  });

  it("says a role doesn't cover a server's updates as every page does", async () => {
    await renderUpdates(() => {
      throw authError(Code.PermissionDenied, "needs_role", "this needs the admin role on the install");
    });
    const why = await callout("Your role doesn't cover this");
    expect(why).toHaveTextContent("Your role doesn't let you do this here. Ask an admin to change your role.");
    expect(screen.queryByText(/couldn't ask hz-1 for its updates/)).toBeNull();
  });

  it("says a server's agent is away, before it was ever read", async () => {
    await renderUpdates(() =>
      answer({
        packages: [],
        readTime: undefined,
        listsTime: undefined,
        reason: "server_offline",
        error: "hz-1's agent isn't connected",
      }),
    );
    expect(await callout("hz-1's agent isn't connected")).toHaveTextContent(
      "Dockwell can't read its updates until it's back.",
    );
    expect(screen.queryByRole("button", { name: "Check now" })).toBeNull();
    expect(screen.queryByText(/up to date/i)).toBeNull();
  });

  it("says how to update an agent too old to read updates", async () => {
    await renderUpdates(() =>
      answer({
        packages: [],
        readTime: undefined,
        reason: "agent_outdated",
        unavailable: "hz-1's agent is older than the control plane",
      }),
    );
    const why = await callout("hz-1's agent doesn't read updates yet");
    expect(why).toHaveTextContent("Update it by running the install script on hz-1 again, which keeps what runs:");
    expect(why).toHaveTextContent(
      "curl -fsSL https://github.com/Dockwell/dockwell/releases/download/v0.9.4/install.sh | sudo sh -s -- --version 0.9.4",
    );
  });

  it("counts Docker's engine among the security fixes when it's one, and says how many need a reboot", async () => {
    await renderUpdates(() =>
      answer({
        automatic: undefined,
        packages: [
          ...waiting.filter((p) => p.name !== "docker-ce"),
          pkg({
            name: "docker.io",
            installed: "24.0.7-0ubuntu4",
            available: "24.0.7-0ubuntu4.1",
            origin: "noble-security",
            security: true,
            effect: PackageEffect.DOCKER_RESTART,
          }),
          pkg({
            name: "libc6",
            installed: "2.39-0ubuntu8.3",
            available: "2.39-0ubuntu8.4",
            origin: "noble-security",
            security: true,
            effect: PackageEffect.REBOOT,
          }),
        ],
      }),
    );
    expect(await screen.findByRole("link", { name: "Updates 5" })).toBeVisible();
    const lead = await callout("5 security updates are ready to install");
    expect(lead).toHaveTextContent("2 need a reboot, and apps and websites go offline while hz-1 restarts.");
    expect(lead).toHaveTextContent("Dockwell can't install them yet; the command installs every update waiting");
    expect(within(lead).getByRole("button", { name: "Copy the upgrade command" })).toBeVisible();
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
    const reboot = await callout("hz-1 needs a reboot");
    expect(reboot).toHaveTextContent("What linux-base changed takes effect once it restarts");
    expect(within(reboot).getByRole("button", { name: "Copy the reboot command" })).toBeVisible();
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

  it("says checking, or why not, rather than up to date, when nothing's read", async () => {
    await renderUpdates(() => answer({ checking: true, packages: [], readTime: undefined }), "/");
    const card = await screen.findByRole("region", { name: "Updates" });
    expect(await within(card).findByText("Checking for updates")).toBeVisible();
    expect(within(card).queryByText(/up to date/i)).toBeNull();
  });

  it("warns, and never says up to date, after a first read that failed", async () => {
    await renderUpdates(
      () => answer({ packages: [], readTime: undefined, reason: "read_failed", error: "status 100" }),
      "/",
    );
    const card = await screen.findByRole("region", { name: "Updates" });
    expect(await within(card).findByText("Dockwell couldn't read hz-1's updates")).toBeVisible();
    expect(within(card).getByText("The last read failed")).toBeVisible();
    expect(within(card).queryByText(/up to date/i)).toBeNull();
  });

  it("counts Alpine's updates in a neutral tone, as none is marked a security fix", async () => {
    await renderUpdates(
      () =>
        answer({
          manager: PackageManager.APK,
          marksSecurity: false,
          packages: [
            pkg({ name: "busybox", installed: "1.36.1-r29", available: "1.36.1-r30" }),
            pkg({ name: "musl", installed: "1.2.5-r0", available: "1.2.5-r1" }),
          ],
        }),
      "/",
    );
    const card = await screen.findByRole("region", { name: "Updates" });
    expect(await within(card).findByText("2 updates")).toBeVisible();
    expect(card).toHaveTextContent("apk doesn't mark security fixes. busybox, musl.");
    const row = within(card).getByText("2 updates").closest(".updates-card-row");
    expect(row?.querySelector(".updates-tone-unknown")).not.toBeNull();
    expect(row?.querySelector(".updates-tone-healthy")).toBeNull();
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
