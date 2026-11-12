// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  AutomaticUpdatesSchema,
  BuildInfoSchema,
  type Change,
  ChangeSchema,
  ChangeState,
  DockwellReleaseSchema,
  GetServerUpdatesResponseSchema,
  PackageEffect,
  PackageManager,
  PackageUpdateSchema,
  PlanAction,
  type PlanInstallUpdatesRequest,
  ServerInventorySchema,
  ServerUpdatesSchema,
  StepState,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError } from "../../test/auth";
import { type Api, feed, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { lastLine, packageLine } from "./install";
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

async function renderUpdates(
  get: () => ReturnType<typeof answer>,
  path = "/servers/srv_1/updates",
  check = vi.fn(),
  more: Api = {},
) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const getServerUpdates = vi.fn(get);
  const rendered = renderAt(path, {
    ...more,
    servers: { watchServers: watch.stream },
    updates: { getServerUpdates, checkServerUpdates: check },
  });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, getServerUpdates, check };
}

/** The plan for hz-1's security fixes, as the server's agent simulated it: openssl, and what it pulls in. */
function installPlan(): Change {
  const host = { kind: "server.host", scope: "srv_1", name: "host" };
  const install = {
    packages: [
      { name: "openssl", installed: "3.0.13-0ubuntu3.4", available: "3.0.13-0ubuntu3.5", security: true },
      { name: "libssl-extra", available: "3.0.13-0ubuntu3.5", security: true },
    ],
    dockerRestarts: true,
  };
  return create(ChangeSchema, {
    id: "chg_install",
    summary: "Install security updates on hz-1",
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(NOW),
    plan: {
      fingerprint: "f0e1d2c3b4a5968778695a4b3c2d1e0f",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], install }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, install }],
        },
      ],
    },
  });
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

  it("reads what an install's lines say of each package, and why one failed", () => {
    expect(packageLine("Unpacking libssl3t64:amd64 (3.0.13-0ubuntu3.5) over (3.0.13-0ubuntu3.4) ...")).toEqual({
      name: "libssl3t64",
      phase: "installing",
    });
    expect(packageLine("Preparing to unpack .../03-openssl_3.0.13-0ubuntu3.5_amd64.deb ...")).toEqual({
      name: "openssl",
      phase: "installing",
    });
    expect(packageLine("Setting up openssl (3.0.13-0ubuntu3.5) ...")).toEqual({ name: "openssl", phase: "installed" });
    expect(packageLine("Downloading the packages")).toBeUndefined();
    expect(
      lastLine(
        "the install ended with status 100, and 1 package didn't reach the planned version: dpkg: error processing package openssl (--configure):; installed openssl package post-installation script subprocess returned error exit status 1; E: Sub-process /usr/bin/dpkg returned an error code (1)",
      ),
    ).toBe("E: Sub-process /usr/bin/dpkg returned an error code (1)");
    expect(lastLine("web-1 hasn't come back after its reboot. Check its console.")).toBe(
      "web-1 hasn't come back after its reboot. Check its console.",
    );
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
    expect(lead).toHaveTextContent("unattended-upgrades installs them within about a day.");
    expect(within(lead).getByRole("button", { name: "Install security updates" })).toBeEnabled();
    expect(within(lead).getByRole("button", { name: "Install every update" })).toBeEnabled();
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
    expect(why).toHaveTextContent("Run the install script on hz-1 again; what runs keeps running:");
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
    // The plan says the rest: what it installs, and that it takes a passkey.
    expect(lead).not.toHaveTextContent("Review the plan");
  });

  it("installs the security fixes as a plan: what the server simulated, what it pulls in, and what follows", async () => {
    const user = userEvent.setup();
    const planInstallUpdates = vi.fn((_req: PlanInstallUpdatesRequest) => ({ change: installPlan() }));
    await renderUpdates(() => answer(), "/servers/srv_1/updates", vi.fn(), {
      changes: { planInstallUpdates, watchChange: feed<WatchChangeResponse>().stream },
    });
    const lead = await callout("3 security updates are ready to install");
    await user.click(within(lead).getByRole("button", { name: "Install security updates" }));
    await waitFor(() => expect(planInstallUpdates).toHaveBeenCalledTimes(1));
    expect(planInstallUpdates.mock.calls[0]?.[0]).toMatchObject({ serverIds: ["srv_1"], all: false });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByText(/Install 2 packages on/)).toBeVisible();
    const packages = within(sheet).getByRole("list", { name: "Packages it installs on hz-1" });
    const rows = within(packages).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "openssl 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5Security fix",
      "libssl-extra 3.0.13-0ubuntu3.5Pulled inSecurity fix",
    ]);
    expect(sheet).toHaveTextContent("Docker restarts: every app and website on hz-1 stops until it's back");
    // What restarts beyond Docker can't be told ahead, so it says they may.
    expect(sheet).toHaveTextContent("Services these packages run may restart as they're installed.");
    expect(sheet).not.toHaveTextContent("Nothing else restarts");
    // As good as root, with no undo, and Docker's restart is the server's downtime: it stops what runs
    // there, so the code is typed as well as the passkey.
    expect(within(sheet).getByText("Applying this asks for your passkey")).toBeVisible();
    expect(within(sheet).getByRole("textbox", { name: /to apply it/ })).toBeVisible();
    expect(within(sheet).getByText("No undo")).toBeVisible();
    expect(within(sheet).queryByText(/Undo for/)).toBeNull();
    expect(within(sheet).getByText(/apps and websites stop while Docker restarts/)).toBeVisible();
    expect(within(sheet).queryByText(/no downtime expected/)).toBeNull();

    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await user.click(within(lead).getByRole("button", { name: "Install every update" }));
    await waitFor(() => expect(planInstallUpdates).toHaveBeenCalledTimes(2));
    expect(planInstallUpdates.mock.calls[1]?.[0]).toMatchObject({ serverIds: ["srv_1"], all: true });
  });

  it("offers every update in one step when none is a security fix", async () => {
    await renderUpdates(() => answer({ packages: waiting.filter((p) => !p.security && p.name !== "docker-ce") }));
    const lead = await callout("2 updates are ready to install");
    expect(within(lead).getByRole("button", { name: "Install every update" })).toBeEnabled();
    expect(within(lead).queryByRole("button", { name: "Install security updates" })).toBeNull();
  });

  it("points at no command where it has none to give", async () => {
    await renderUpdates(() => answer({ manager: PackageManager.UNSPECIFIED }));
    const lead = await callout("3 security updates are ready to install");
    expect(lead).toHaveTextContent("unattended-upgrades installs them within about a day.");
    expect(lead).not.toHaveTextContent("the command");
    expect(within(lead).queryByRole("button", { name: "Copy the upgrade command" })).toBeNull();
  });

  it("gives the command on a server that doesn't install with apt, and offers no install", async () => {
    await renderUpdates(() => answer({ manager: PackageManager.DNF, automatic: undefined }));
    const lead = await callout("3 security updates are ready to install");
    expect(lead).toHaveTextContent(
      "Dockwell installs updates with apt for now; the command installs every update waiting, not only these.",
    );
    expect(within(lead).getByRole("button", { name: "Copy the upgrade command" })).toBeVisible();
    expect(within(lead).queryByRole("button", { name: /Install/ })).toBeNull();
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
    expect(rows[0]).toHaveTextContent("0.10.6-2ubuntu0.1from 0.10.6-2build2");
    expect(within(rows[0] as HTMLElement).getByRole("button", { name: "Update libssh-4" })).toBeVisible();
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

/**
 * A change's watch that every watcher hears, the page and the sheet alike, each
 * from the change as it last stood.
 */
function watched() {
  const subs = new Set<(c: Change) => void>();
  let last: Change | undefined;
  return {
    push(c: Change) {
      last = c;
      for (const sub of subs) sub(c);
    },
    async *watchChange(_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchChangeResponse> {
      const queue: Change[] = last ? [last] : [];
      let wake = () => {};
      const sub = (c: Change) => {
        queue.push(c);
        wake();
      };
      subs.add(sub);
      try {
        for (;;) {
          const next = queue.shift();
          if (next) {
            yield {
              $typeName: "dockwell.v1.WatchChangeResponse",
              event: { case: "change", value: next },
            } as WatchChangeResponse;
            continue;
          }
          if (ctx.signal.aborted) return;
          await Promise.race([new Promise<void>((resolve) => (wake = resolve)), untilAborted(ctx.signal)]);
        }
      } finally {
        subs.delete(sub);
      }
    },
  };
}

/** A plan to update openssl on hz-1, which pulls in libssl3t64, as it stands at state with its step. */
function opensslUpdate(state: ChangeState, step?: { state: StepState; activity?: string; detail?: string }): Change {
  const host = { kind: "server.host", scope: "srv_1", name: "host" };
  const install = {
    packages: [
      { name: "openssl", installed: "3.0.13-0ubuntu3.4", available: "3.0.13-0ubuntu3.5", security: true },
      { name: "libssl3t64", installed: "3.0.13-0ubuntu3.4", available: "3.0.13-0ubuntu3.5", security: true },
    ],
    asked: ["openssl"],
  };
  return create(ChangeSchema, {
    id: "chg_openssl",
    summary: "Update openssl on hz-1",
    state,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(NOW),
    error: state === ChangeState.FAILED ? "Installing updates on hz-1 failed" : "",
    plan: {
      fingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], install }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, install }],
        },
      ],
    },
    steps: step
      ? [{ serverId: "srv_1", resourceId: "res_host", resource: host, action: PlanAction.UPDATE, ...step }]
      : [],
  });
}

/** What hz-1 has waiting, with libssl3t64 among the security fixes. */
function withLibssl(updates: MessageInitShape<typeof ServerUpdatesSchema> = {}) {
  return answer({
    packages: [
      ...waiting,
      pkg({
        name: "libssl3t64",
        summary: "Secure Sockets Layer toolkit - shared libraries",
        installed: "3.0.13-0ubuntu3.4",
        available: "3.0.13-0ubuntu3.5",
        origin: "noble-security",
        security: true,
      }),
    ].sort((a, b) => Number(b.security) - Number(a.security) || a.name.localeCompare(b.name)),
    ...updates,
  });
}

function rowOf(table: HTMLElement, name: string): HTMLElement {
  const cell = within(table).getByText(name, { selector: ".updates-name" });
  const row = cell.closest("tr");
  if (!row) throw new Error(`no row for ${name}`);
  return row;
}

describe("updating packages one by one", () => {
  it("plans one package from its own row, and its row says so until the plan is up", async () => {
    const user = userEvent.setup();
    let answerPlan: (c: { change: Change }) => void = () => {};
    const planInstallUpdates = vi.fn(
      (_req: PlanInstallUpdatesRequest) => new Promise<{ change: Change }>((resolve) => (answerPlan = resolve)),
    );
    const watch = watched();
    await renderUpdates(() => withLibssl(), "/servers/srv_1/updates", vi.fn(), {
      changes: { planInstallUpdates, watchChange: watch.watchChange },
    });
    const table = await screen.findByRole("table", { name: "Security updates for hz-1" });
    const update = within(rowOf(table, "openssl")).getByRole("button", { name: "Update openssl" });
    await user.click(update);
    expect(planInstallUpdates.mock.calls[0]?.[0]).toMatchObject({
      serverIds: ["srv_1"],
      packages: ["openssl"],
      all: false,
    });
    // Planning shows on the row's own button; the others wait for it, and nothing moves.
    expect(update).toHaveAttribute("aria-busy", "true");
    expect(within(rowOf(table, "libssh-4")).getByRole("button", { name: "Update libssh-4" })).toBeDisabled();
    watch.push(opensslUpdate(ChangeState.PLANNED));
    act(() => answerPlan({ change: opensslUpdate(ChangeState.PLANNED) }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const packages = within(sheet).getByRole("list", { name: "Packages it installs on hz-1" });
    // What was picked comes first, then what it pulls in, each with what changes marked.
    expect(
      within(packages)
        .getAllByRole("listitem")
        .map((r) => r.textContent),
    ).toEqual([
      "openssl 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5Security fix",
      "libssl3t64 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5Pulled inSecurity fix",
    ]);
    expect(packages.querySelector(".plan-install-changed")).toHaveTextContent(/^5$/);
    // It restarts nothing, so the passkey is enough: no code to type, and an ordinary Apply (ADR 0064).
    expect(within(sheet).getByText("Applying this asks for your passkey")).toBeVisible();
    expect(within(sheet).getByText("No undo")).toBeVisible();
    expect(within(sheet).queryByRole("textbox", { name: /to apply it/ })).toBeNull();
    expect(within(sheet).getByRole("button", { name: /^Apply plan/ })).not.toHaveClass("dw-btn-danger");
    expect(await within(rowOf(table, "openssl")).findByText("In review")).toBeInTheDocument();
    expect(within(rowOf(table, "libssl3t64")).getByText("In review")).toBeInTheDocument();
  });

  it("picks a range with shift, and updates what's picked in one plan", async () => {
    const user = userEvent.setup();
    const planInstallUpdates = vi.fn((_req: PlanInstallUpdatesRequest) => ({
      change: opensslUpdate(ChangeState.PLANNED),
    }));
    await renderUpdates(() => withLibssl(), "/servers/srv_1/updates", vi.fn(), {
      changes: { planInstallUpdates, watchChange: watched().watchChange },
    });
    const table = await screen.findByRole("table", { name: "Security updates for hz-1" });
    expect(screen.queryByRole("region", { name: "Selected updates" })).toBeNull();
    await user.click(within(table).getByRole("checkbox", { name: "Select libssh-4" }));
    await user.keyboard("{Shift>}");
    await user.click(within(table).getByRole("checkbox", { name: "Select linux-image-6.8.0-49-generic" }));
    await user.keyboard("{/Shift}");
    const bar = screen.getByRole("region", { name: "Selected updates" });
    expect(bar).toHaveTextContent("3 selected");
    expect(within(table).getByRole("checkbox", { name: "Select libssl3t64" })).toBeChecked();
    expect(within(table).getByRole("checkbox", { name: "Select openssl" })).not.toBeChecked();
    // The column's box says some are picked; Clear lets go of them, and the box picks every one.
    const all = within(table).getByRole("checkbox", { name: /^Select every package in/ });
    expect((all as HTMLInputElement).indeterminate).toBe(true);
    await user.click(within(bar).getByRole("button", { name: "Clear" }));
    expect(screen.queryByRole("region", { name: "Selected updates" })).toBeNull();
    await user.click(all);
    const again = screen.getByRole("region", { name: "Selected updates" });
    await user.click(within(again).getByRole("button", { name: "Update 4 selected" }));
    await waitFor(() => expect(planInstallUpdates).toHaveBeenCalledTimes(1));
    expect(planInstallUpdates.mock.calls[0]?.[0]).toMatchObject({
      packages: ["libssh-4", "libssl3t64", "linux-image-6.8.0-49-generic", "openssl"],
    });
  });

  it("follows an install on its rows, then says what each went to before it leaves", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    let installed = false;
    const watch = watched();
    const { getServerUpdates } = await renderUpdates(
      () =>
        installed
          ? answer({
              packages: waiting.filter((p) => p.name !== "openssl"),
              readTime: timestampFromDate(new Date(NOW.getTime() + 60_000)),
            })
          : withLibssl({ installChangeId: "chg_openssl" }),
      "/servers/srv_1/updates",
      vi.fn(),
      { changes: { watchChange: watch.watchChange } },
    );
    // An install someone started elsewhere is followed as well: the others wait for it, with why.
    watch.push(opensslUpdate(ChangeState.APPLYING, { state: StepState.RUNNING, activity: "Downloading the packages" }));
    const lead = await callout("Installing 2 packages on hz-1");
    expect(lead).toHaveTextContent("Downloading the packages");
    const table = screen.getByRole("table", { name: "Security updates for hz-1" });
    expect(await within(rowOf(table, "openssl")).findByText("Downloading")).toBeVisible();
    const other = within(rowOf(table, "libssh-4")).getByRole("button", { name: "Update libssh-4" });
    expect(other).toHaveAttribute("aria-disabled", "true");
    expect(other).toHaveAccessibleDescription("An install is running on hz-1. This one can start once it ends.");
    expect(screen.getByRole("button", { name: "Check now" })).toHaveAttribute("aria-disabled", "true");

    watch.push(
      opensslUpdate(ChangeState.APPLYING, {
        state: StepState.RUNNING,
        activity: "Unpacking openssl (3.0.13-0ubuntu3.5) over (3.0.13-0ubuntu3.4) ...",
      }),
    );
    expect(await within(rowOf(table, "openssl")).findByText("Installing")).toBeVisible();
    expect(rowOf(table, "openssl")).toHaveTextContent("Unpacking openssl (3.0.13-0ubuntu3.5)");
    watch.push(
      opensslUpdate(ChangeState.APPLYING, {
        state: StepState.RUNNING,
        activity: "Setting up openssl (3.0.13-0ubuntu3.5) ...",
      }),
    );
    expect(await within(rowOf(table, "openssl")).findByText("Installed")).toBeVisible();
    expect(within(rowOf(table, "libssl3t64")).getByText("Installing")).toBeVisible();

    installed = true;
    const asked = getServerUpdates.mock.calls.length;
    watch.push(opensslUpdate(ChangeState.APPLIED, { state: StepState.DONE }));
    // Once it applies, it's read again, and each row says what it went to, in place.
    await waitFor(() => expect(getServerUpdates.mock.calls.length).toBeGreaterThan(asked));
    expect(await within(rowOf(table, "openssl")).findByText("Updated")).toBeVisible();
    expect(rowOf(table, "openssl")).toHaveTextContent("Updated to 3.0.13-0ubuntu3.5");
    expect(rowOf(table, "libssl3t64")).toHaveTextContent("Updated to 3.0.13-0ubuntu3.5");
    expect(screen.queryByText("Installing 2 packages on hz-1")).toBeNull();
    // Then they leave, and the counts follow, with no reload.
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    await waitFor(() => expect(within(table).queryByText("openssl", { selector: ".updates-name" })).toBeNull());
    expect(within(table).queryByText("libssl3t64", { selector: ".updates-name" })).toBeNull();
    expect(screen.getByRole("link", { name: "Updates 2" })).toBeVisible();
  });

  it("says on the row that an install failed, with its last line, the change, and Retry", async () => {
    const user = userEvent.setup();
    const watch = watched();
    const planInstallUpdates = vi.fn((_req: PlanInstallUpdatesRequest) => ({
      change: opensslUpdate(ChangeState.PLANNED),
    }));
    await renderUpdates(() => withLibssl({ installChangeId: "chg_openssl" }), "/servers/srv_1/updates", vi.fn(), {
      changes: {
        planInstallUpdates,
        watchChange: watch.watchChange,
        getChange: () => ({ change: opensslUpdate(ChangeState.FAILED) }),
      },
    });
    watch.push(opensslUpdate(ChangeState.APPLYING, { state: StepState.RUNNING, activity: "Installing 2 packages" }));
    const table = await screen.findByRole("table", { name: "Security updates for hz-1" });
    await within(rowOf(table, "openssl")).findByText("Installing");
    watch.push(
      opensslUpdate(ChangeState.FAILED, {
        state: StepState.FAILED,
        message: "Installing updates on hz-1 failed",
        detail:
          "the install ended with status 100, and 1 package didn't reach the planned version: dpkg: error processing package openssl (--configure): E: Sub-process /usr/bin/dpkg returned an error code (1)",
      } as never),
    );
    const row = rowOf(table, "openssl");
    expect(await within(row).findByText("Failed")).toBeVisible();
    expect(row).toHaveTextContent("E: Sub-process /usr/bin/dpkg returned an error code (1)");
    expect(within(row).getByRole("button", { name: "Details" })).toBeVisible();
    await user.click(within(row).getByRole("button", { name: "Retry updating openssl" }));
    await waitFor(() => expect(planInstallUpdates).toHaveBeenCalledTimes(1));
    expect(planInstallUpdates.mock.calls[0]?.[0]).toMatchObject({ packages: ["openssl"] });
  });

  it("updates Docker from its card, with what else restarts it", async () => {
    const user = userEvent.setup();
    const planInstallUpdates = vi.fn((_req: PlanInstallUpdatesRequest) => ({
      change: opensslUpdate(ChangeState.PLANNED),
    }));
    await renderUpdates(
      () =>
        answer({
          packages: [
            ...waiting,
            pkg({
              name: "containerd.io",
              installed: "1.7.22-1",
              available: "1.7.24-1",
              effect: PackageEffect.DOCKER_RESTART,
            }),
          ],
        }),
      "/servers/srv_1/updates",
      vi.fn(),
      { changes: { planInstallUpdates, watchChange: watched().watchChange } },
    );
    const versions = await screen.findByRole("region", { name: "Docker and Dockwell" });
    // The card is the engine's: containerd's version isn't Docker's.
    expect(versions).toHaveTextContent("27.3.1 → 27.4.0 available");
    await user.click(within(versions).getByRole("button", { name: "Update Docker" }));
    await waitFor(() => expect(planInstallUpdates).toHaveBeenCalledTimes(1));
    expect(planInstallUpdates.mock.calls[0]?.[0]).toMatchObject({ packages: ["containerd.io", "docker-ce"] });
  });

  it("offers no Update where Dockwell doesn't install, and nothing to pick", async () => {
    await renderUpdates(() => answer({ manager: PackageManager.DNF, automatic: undefined }));
    const table = await screen.findByRole("table", { name: "Security updates for hz-1" });
    expect(within(table).queryByRole("button", { name: /^Update / })).toBeNull();
    expect(within(table).queryByRole("checkbox")).toBeNull();
  });
});
