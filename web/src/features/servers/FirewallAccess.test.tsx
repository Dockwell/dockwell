// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  Fail2banState,
  type GetServerFail2banResponseSchema,
  type GetServerSshResponseSchema,
  ListenerReach,
  SshAnswer,
  SshLoaded,
  SshRootLogin,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError } from "../../test/auth";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

type Ssh = MessageInitShape<typeof GetServerSshResponseSchema>["ssh"];
type Fail2ban = MessageInitShape<typeof GetServerFail2banResponseSchema>["fail2ban"];

/** SSH as an Ubuntu cloud image with its port moved has it, with a Match block. */
const ubuntu: Ssh = {
  version: "OpenSSH_9.6p1 Ubuntu-3ubuntu13.5",
  passwordLogin: SshAnswer.YES,
  keyLogin: SshAnswer.YES,
  rootLogin: SshRootLogin.KEYS_ONLY,
  ports: [2222],
  setIn: [
    { keyword: "PasswordAuthentication", file: "/etc/ssh/sshd_config.d/50-cloud-init.conf", line: 1, value: "yes" },
    { keyword: "Port", file: "/etc/ssh/sshd_config.d/60-port.conf", line: 2, value: "2222" },
    { keyword: "KbdInteractiveAuthentication", file: "/etc/ssh/sshd_config", line: 7, value: "no" },
  ],
  matches: [{ file: "/etc/ssh/sshd_config", line: 12, criteria: "User deploy", sets: ["PasswordAuthentication"] }],
  dropInFolder: "/etc/ssh/sshd_config.d",
  socketUnit: "ssh.socket",
};

const minutesAgo = (n: number) => timestampFromDate(new Date(Date.now() - n * 60_000));

/** fail2ban with two jails, one of them banning for good, three addresses banned now. */
const running: Fail2ban = {
  state: Fail2banState.RUNNING,
  version: "1.0.2",
  jails: [
    {
      name: "nginx-http-auth",
      maxRetry: 3,
      findTime: durationFromMs(60_000),
      bansForGood: true,
      bannedNow: 1,
      bannedTotal: 1,
      bans: [{ address: "203.0.113.50" }],
    },
    {
      name: "sshd",
      maxRetry: 5,
      findTime: durationFromMs(600_000),
      banTime: durationFromMs(3_600_000),
      bannedNow: 2,
      bannedTotal: 5,
      bans: [
        { address: "198.51.100.77", since: minutesAgo(6) },
        { address: "192.0.2.211", since: minutesAgo(36) },
      ],
    },
  ],
};

async function renderAccess({
  ssh = ubuntu,
  fail2ban = running,
  firewall = true,
}: {
  ssh?: Ssh | Error;
  fail2ban?: Fail2ban | Error;
  /** False makes the firewall's own read fail, which leaves SSH and fail2ban to say what they read. */
  firewall?: boolean;
}) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const getServerSsh = vi.fn(() => {
    if (ssh instanceof Error) throw ssh;
    return { ssh: { time: timestampFromDate(new Date()), ...ssh } };
  });
  const getServerFail2ban = vi.fn(() => {
    if (fail2ban instanceof Error) throw fail2ban;
    return { fail2ban: { time: timestampFromDate(new Date()), ...fail2ban } };
  });
  const rendered = renderAt("/servers/srv_1/firewall", {
    servers: { watchServers: watch.stream },
    firewall: {
      getServerFirewall: () => {
        if (!firewall) throw authError(Code.Unavailable, "unavailable", "hz-1 can't say what filters its traffic now");
        return {
          firewall: {
            time: timestampFromDate(new Date()),
            listening: [
              {
                address: "0.0.0.0",
                port: 2222,
                protocol: "tcp",
                reach: ListenerReach.SOME,
                sources: ["203.0.113.0/24"],
              },
              { address: "0.0.0.0", port: 80, protocol: "tcp", reach: ListenerReach.OPEN },
            ],
          },
        };
      },
      getServerSsh,
      getServerFail2ban,
    },
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return { ...rendered, getServerSsh, getServerFail2ban };
}

/** A fact of a card's list: its term, and what follows it, in order. */
function fact(card: HTMLElement, term: string): string[] {
  const dt = within(card).getByText(term, { selector: "dt" });
  return [...(dt.parentElement?.querySelectorAll("dd") ?? [])].map((dd) => dd.textContent ?? "");
}

afterEach(() => {
  activateLocale("en");
});

/** What the SSH card says of its own values, in every state. */
const lead = "These are what sshd's files say; sshd uses what it read when it last started or reloaded.";

/** Words that would read as a confirmation that the sshd that listens has read its files, which the card never says. */
const confirming = [/has read/, /read them since/, /in effect\b(?! until)/, /after each last changed/, /confirmed/i];

/** Opens the card's list of Match blocks, and returns its lines. */
async function matchBlocks(card: HTMLElement): Promise<string[]> {
  const user = userEvent.setup();
  await user.click(within(card).getByText(/^Defaults for every connection:/));
  return within(card)
    .getAllByRole("listitem")
    .map((li) => li.textContent ?? "")
    .filter((line) => line.startsWith("Match "));
}

describe("SSH on a server's Firewall tab", () => {
  it("says what sshd's files say, where each value is set, and never that sshd has read them", async () => {
    await renderAccess({});
    const card = await screen.findByRole("region", { name: "SSH" });
    await within(card).findByText("Password login");
    expect(fact(card, "Password login")).toEqual(["On", "50-cloud-init.conf, line 1"]);
    expect(fact(card, "Root login")).toEqual(["Keys only", "sshd's default"]);
    expect(fact(card, "Port")).toEqual(["2222", "60-port.conf, line 2"]);
    expect(await within(card).findByText("203.0.113.0/24")).toBeVisible();
    expect(within(card).getByText("OpenSSH_9.6p1 Ubuntu-3ubuntu13.5")).toBeVisible();
    expect(card).toHaveTextContent("systemd holds SSH's port, through ssh.socket.");
    expect(card).not.toHaveTextContent("includes no drop-in folder");
    // The lead line, always; nothing to warn of is no confirmation, and nothing is marked.
    expect(card).toHaveTextContent(lead);
    for (const never of confirming) expect(card).not.toHaveTextContent(never);
    expect(within(card).queryByRole("heading", { name: /sshd hasn't read|may not use/ })).toBeNull();
    expect(card).not.toHaveTextContent("not in effect until sshd reloads");
  });

  it("warns, naming the files and the command that reloads sshd, when they changed after it last read them", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await renderAccess({
      ssh: {
        ...ubuntu,
        passwordLogin: SshAnswer.NO,
        loaded: SshLoaded.STALE,
        changedFiles: ["/etc/ssh/sshd_config.d/50-cloud-init.conf"],
        reloadCommand: "systemctl reload ssh.service",
        setIn: [
          {
            keyword: "PasswordAuthentication",
            file: "/etc/ssh/sshd_config.d/50-cloud-init.conf",
            line: 1,
            value: "no",
            changed: true,
          },
          { keyword: "Port", file: "/etc/ssh/sshd_config.d/60-port.conf", line: 2, value: "2222" },
        ],
      },
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    expect(await within(card).findByText("sshd hasn't read 50-cloud-init.conf since it changed")).toBeVisible();
    expect(card).toHaveTextContent(lead);
    expect(card).toHaveTextContent("Until it reloads, it may still accept what this says is off.");
    expect(within(card).getByText("systemctl reload ssh.service")).toBeVisible();
    await user.click(within(card).getByRole("button", { name: "Copy systemctl reload ssh.service" }));
    expect(writeText).toHaveBeenCalledWith("systemctl reload ssh.service");
    // Only a value from a file that changed is marked, and only in this state.
    expect(fact(card, "Password login")).toEqual([
      "Off",
      "50-cloud-init.conf, line 1 · not in effect until sshd reloads",
    ]);
    expect(fact(card, "Port")).toEqual(["2222", "60-port.conf, line 2"]);
  });

  it("says can't tell for each value sshd -T didn't say, never Off, and only the lead line when nothing can be read", async () => {
    await renderAccess({
      ssh: {
        version: "OpenSSH_9.6p1",
        ports: [],
        loadedWhy: "No sshd that listens was found among the server's processes.",
        dropInFolder: "/etc/ssh/sshd_config.d",
      },
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    await within(card).findByText("Password login");
    // A value that can't be told names no line, and isn't called sshd's default.
    expect(fact(card, "Password login")).toEqual(["Can't tell", ""]);
    expect(fact(card, "Root login")).toEqual(["Can't tell", ""]);
    expect(fact(card, "Ports")).toEqual(["Can't tell", ""]);
    // What couldn't be read is no warning: the lead line, and nothing more.
    expect(card).toHaveTextContent(lead);
    expect(card).not.toHaveTextContent("No sshd that listens");
    expect(within(card).queryByRole("heading", { name: /sshd/ })).toBeNull();
    for (const never of confirming) expect(card).not.toHaveTextContent(never);
  });

  it("warns when the sshd that listens isn't run from these files, in the agent's words", async () => {
    await renderAccess({
      ssh: {
        ...ubuntu,
        loaded: SshLoaded.APART,
        loadedWhy: "It's /usr/local/sbin/sshd, not the /usr/sbin/sshd these were read with.",
        setIn: [
          { keyword: "PasswordAuthentication", file: "/etc/ssh/sshd_config", line: 3, value: "no", changed: false },
        ],
      },
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    expect(await within(card).findByText("The sshd that listens may not use these files")).toBeVisible();
    expect(card).toHaveTextContent("It's /usr/local/sbin/sshd, not the /usr/sbin/sshd these were read with.");
    expect(card).toHaveTextContent(lead);
    expect(card).not.toHaveTextContent("not in effect until sshd reloads");
  });

  it("lists every Match block, whatever it sets, behind a line that says the values are only the defaults", async () => {
    await renderAccess({
      ssh: {
        ...ubuntu,
        matches: [
          { file: "/etc/ssh/sshd_config", line: 12, criteria: "User deploy", sets: ["PasswordAuthentication"] },
          { file: "/etc/ssh/sshd_config.d/90-office.conf", line: 1, criteria: "Address 198.51.100.0/24", sets: [] },
        ],
      },
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    await within(card).findByText("Password login");
    expect(
      within(card).getByText("Defaults for every connection: 2 Match blocks can set them otherwise"),
    ).toBeVisible();
    expect(await matchBlocks(card)).toEqual([
      "Match User deploy · sshd_config, line 12 · sets PasswordAuthentication",
      "Match Address 198.51.100.0/24 · 90-office.conf, line 1",
    ]);
  });

  it("says when sshd_config includes no drop-in folder, who may sign in, and ways in Dockwell can't check", async () => {
    await renderAccess({
      ssh: {
        passwordLogin: SshAnswer.NO,
        rootLogin: SshRootLogin.NO,
        ports: [22, 2200],
        setIn: [{ keyword: "PasswordAuthentication", file: "/etc/ssh/sshd_config", line: 60, value: "no" }],
        accessLists: ["allowusers deploy"],
        unchecked: ["AuthorizedKeysCommand"],
      },
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    await within(card).findByText("Password login");
    expect(fact(card, "Password login")).toEqual(["Off", "sshd_config, line 60"]);
    expect(fact(card, "Root login")[0]).toBe("No");
    expect(fact(card, "Ports")[0]).toBe("22, 2200");
    expect(card).toHaveTextContent("sshd_config includes no drop-in folder, so Dockwell won't change SSH here.");
    expect(card).toHaveTextContent("sshd lets in only who these allow: allowusers deploy.");
    expect(card).toHaveTextContent("AuthorizedKeysCommand also let people in, which Dockwell can't check.");
    expect(within(card).queryByText(/^Defaults for every connection/)).toBeNull();
  });

  it("says why SSH can't be read, and still shows SSH and fail2ban when the firewall can't be read", async () => {
    await renderAccess({
      ssh: { unreadable: "This server has no OpenSSH sshd, so Dockwell reads nothing of SSH here." },
      firewall: false,
    });
    const card = await screen.findByRole("region", { name: "SSH" });
    expect(
      await within(card).findByText("This server has no OpenSSH sshd, so Dockwell reads nothing of SSH here."),
    ).toBeVisible();
    expect(await screen.findByText("hz-1 can't say what filters its traffic now.")).toBeVisible();
    expect(await screen.findByRole("region", { name: "Fail2ban" })).toBeVisible();
  });

  it("says how to update an agent too old to read SSH or fail2ban", async () => {
    const old = authError(Code.FailedPrecondition, "agent_outdated", "too old");
    await renderAccess({ ssh: old, fail2ban: old });
    expect(
      await screen.findByText(
        "The agent on hz-1 is older than the control plane, so it can't read SSH. Run the install script on hz-1 again to update it.",
      ),
    ).toBeVisible();
    expect(
      await screen.findByText(
        "The agent on hz-1 is older than the control plane, so it can't read fail2ban. Run the install script on hz-1 again to update it.",
      ),
    ).toBeVisible();
  });
});

describe("fail2ban on a server's Firewall tab", () => {
  it("says it runs, how many it bans now, each jail's rule, and whom it bans, the latest first", async () => {
    await renderAccess({});
    const card = await screen.findByRole("region", { name: "Fail2ban" });
    expect(await within(card).findByText("Running")).toBeVisible();
    expect(card).toHaveTextContent("Banned now3addresses");
    expect(card).toHaveTextContent("6 bans since fail2ban started");
    const jails = within(within(card).getByRole("region", { name: "Jails" })).getAllByRole("listitem");
    expect(jails.map((j) => j.textContent)).toEqual([
      "nginx-http-auth3 failed sign-ins in 1 min · bans for good1",
      "sshd5 failed sign-ins in 10 min · bans for 1 hour2",
    ]);
    const bans = within(within(card).getByRole("region", { name: "Recent bans" })).getAllByRole("listitem");
    expect(bans.map((b) => b.textContent)).toEqual([
      "198.51.100.77sshd · 6 min ago",
      "192.0.2.211sshd · 36 min ago",
      "203.0.113.50nginx-http-auth",
    ]);
  });

  it("says when it isn't installed, or isn't running, and why", async () => {
    const { unmount } = await renderAccess({ fail2ban: { state: Fail2banState.NOT_INSTALLED } });
    let card = await screen.findByRole("region", { name: "Fail2ban" });
    expect(
      await within(card).findByText(
        "fail2ban isn't installed on hz-1, so nothing bans an address that keeps failing to sign in.",
      ),
    ).toBeVisible();
    expect(within(card).getByText("Not installed")).toBeVisible();
    unmount();
    await renderAccess({
      fail2ban: { state: Fail2banState.STOPPED, why: "Failed to access socket path: /var/run/fail2ban/fail2ban.sock." },
    });
    card = await screen.findByRole("region", { name: "Fail2ban" });
    expect(await within(card).findByText("Stopped")).toBeVisible();
    expect(card).toHaveTextContent(
      "fail2ban isn't running on hz-1, so it bans nobody: Failed to access socket path: /var/run/fail2ban/fail2ban.sock.",
    );
  });

  it("passes axe, and speaks Simplified Chinese", async () => {
    const { container, unmount } = await renderAccess({});
    await screen.findByText("Recent bans", { selector: "h3" });
    await screen.findByText("Password login");
    await expectAccessible(container);
    unmount();
    await activateLocale("zh-CN");
    await renderAccess({});
    expect(await screen.findByText("密码登录")).toBeVisible();
    expect(await screen.findByText("正在运行")).toBeVisible();
  });
});
