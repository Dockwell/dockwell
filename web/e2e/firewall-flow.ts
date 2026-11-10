// SPDX-License-Identifier: AGPL-3.0-only

// A server's Firewall tab, read-only: what filters its traffic, and the ports
// Docker publishes. Under DOCKWELL_TEST the agent runs none of the machine's own
// tools, only the stand-ins hostTools writes: a ufw that answers as Ubuntu's does
// after ufw allow OpenSSH and ufw allow "Nginx Full", and an nft with nothing in
// its ruleset. Nothing is changed by looking (ADR 0052).

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

/** ufw status verbose as ufw prints it, its rows "%-26s %-12s%-26s", app profiles named after their ports. */
const ufwStatus = [
  "Status: active",
  "Logging: on (low)",
  "Default: deny (incoming), allow (outgoing), disabled (routed)",
  "New profiles: skip",
  "",
  "To                         Action      From",
  "--                         ------      ----",
  "22/tcp (OpenSSH)           ALLOW IN    Anywhere",
  "80,443/tcp (Nginx Full)    ALLOW IN    Anywhere",
  "22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)",
  "80,443/tcp (Nginx Full (v6)) ALLOW IN    Anywhere (v6)",
  "",
].join("\n");

/** sshd -T on an Ubuntu 24.04 cloud image whose cloud-init turned passwords off, cut to what the agent reads. */
const sshdT = [
  "port 22",
  "permitrootlogin without-password",
  "pubkeyauthentication yes",
  "passwordauthentication no",
  "kbdinteractiveauthentication no",
  "authorizedkeyscommand none",
  "gssapiauthentication no",
  "",
].join("\n");

/** fail2ban-client 1.0.2 with an sshd jail that reads the journal and bans two addresses. */
const fail2banClient = `case "$*" in
version) echo 1.0.2 ;;
ping) echo 'Server replied: pong' ;;
status) printf 'Status\\n|- Number of jail:\\t1\\n\`- Jail list:\\tsshd\\n' ;;
"status sshd") printf 'Status for the jail: sshd\\n|- Filter\\n|  |- Currently failed:\\t1\\n|  |- Total failed:\\t27\\n|  \`- Journal matches:\\t_SYSTEMD_UNIT=sshd.service + _COMM=sshd\\n\`- Actions\\n   |- Currently banned:\\t2\\n   |- Total banned:\\t5\\n   \`- Banned IP list:\\t198.51.100.77 192.0.2.211\\n' ;;
"get sshd maxretry") echo 5 ;;
"get sshd findtime") echo 600 ;;
"get sshd bantime") echo 3600 ;;
-t) echo "OK: configuration test is successful" ;;
reload) echo OK ;;
"get sshd banip --with-time") printf '198.51.100.77 \\t2026-11-08 03:10:00 + 3600 = 2026-11-08 04:10:00\\n192.0.2.211 \\t2026-11-08 02:40:00 + 3600 = 2026-11-08 03:40:00\\n' ;;
*) echo "Invalid command ($*)" >&2; exit 255 ;;
esac
`;

/**
 * Writes stand-ins for the server's own tools into dir, laid out as the paths
 * they stand in for, for DOCKWELL_TEST_TOOLS, and returns dir: ufw and nft;
 * sshd, with the files it reads under etc/ssh; and fail2ban-client.
 */
export function hostTools(dir: string): string {
  const sbin = join(dir, "usr", "sbin");
  const bin = join(dir, "usr", "bin");
  const ssh = join(dir, "etc", "ssh");
  for (const d of [sbin, bin, join(ssh, "sshd_config.d"), join(dir, "etc", "fail2ban", "jail.d")]) {
    mkdirSync(d, { recursive: true });
  }
  writeFileSync(join(sbin, "ufw"), `#!/bin/sh\ncat <<'EOF'\n${ufwStatus}EOF\n`, { mode: 0o755 });
  writeFileSync(join(sbin, "nft"), `#!/bin/sh\necho '{"nftables": []}'\n`, { mode: 0o755 });
  writeFileSync(
    join(sbin, "sshd"),
    `#!/bin/sh\ncase "$1" in\n-T) cat <<'EOF'\n${sshdT}EOF\n;;\n-V) echo 'OpenSSH_9.6p1 Ubuntu-3ubuntu13.5, OpenSSL 3.0.13 30 Jan 2024' >&2 ;;\nesac\n`,
    { mode: 0o755 },
  );
  writeFileSync(
    join(ssh, "sshd_config"),
    "Include /etc/ssh/sshd_config.d/*.conf\nKbdInteractiveAuthentication no\nUsePAM yes\n",
  );
  writeFileSync(join(ssh, "sshd_config.d", "50-cloud-init.conf"), "PasswordAuthentication no\n");
  writeFileSync(join(bin, "fail2ban-client"), `#!/bin/sh\n${fail2banClient}`, { mode: 0o755 });
  return dir;
}

interface FirewallFlowOptions {
  /** The environment the binary runs with, which names the stand-ins' folder. */
  env?: NodeJS.ProcessEnv;
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function firewallFlow(page: Page, { origin, step, shoot, env }: FirewallFlowOptions) {
  step("a server's Firewall tab says what filters its traffic, read-only, and what Docker publishes past it");
  // The run has a second server by now, with no agent connected: read the control server's.
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  await page
    .getByRole("navigation", { name: /^Server / })
    .getByRole("link", { name: "Firewall" })
    .click();
  await page.waitForFunction((to) => window.location.pathname === to, `${link}/firewall`);
  // ufw's app profiles read as the ports they allow, as ufw status verbose says them.
  const ufw = page.getByRole("region", { name: "ufw" });
  await ufw.waitFor({ timeout: 15_000 });
  await ufw
    .getByRole("row", { name: /22\/tcp \(OpenSSH\)/ })
    .first()
    .waitFor();
  await ufw
    .getByRole("row", { name: /80,443\/tcp \(Nginx Full\)/ })
    .first()
    .waitFor();
  if ((await page.getByText(/Nothing filters what comes in/).count()) > 0) {
    fail("the Firewall tab says nothing filters, beside ufw");
  }
  if ((await page.getByRole("alert").count()) > 0) {
    fail(`the Firewall tab says something went wrong: ${await page.getByRole("alert").first().innerText()}`);
  }
  await shoot(page, "server-firewall");

  step("SSH and fail2ban say how people sign in and whom fail2ban bans, as each runs, read-only");
  const ssh = page.getByRole("region", { name: "SSH" });
  await ssh.getByText("Password login").waitFor({ timeout: 15_000 });
  const sshText = await ssh.innerText();
  // The card says whose word its values are, and never that the sshd that listens has read them.
  for (const want of [
    "Off",
    "50-cloud-init.conf, line 1",
    "Keys only",
    "sshd's default",
    "OpenSSH_9.6p1",
    "These are what sshd's files say; sshd uses what it read",
  ]) {
    if (!sshText.includes(want)) fail(`the SSH card doesn't say ${want}: ${sshText}`);
  }
  const f2b = page.getByRole("region", { name: "Fail2ban" });
  await f2b.getByText("198.51.100.77").waitFor({ timeout: 15_000 });
  const f2bText = await f2b.innerText();
  for (const want of ["Running", "sshd", "5 failed sign-ins in 10 min · bans for 1 hour", "192.0.2.211"]) {
    if (!f2bText.includes(want)) fail(`the fail2ban card doesn't say ${want}: ${f2bText}`);
  }
  await shoot(page, "server-firewall-access");

  step("the SSH jail is set from the Fail2ban card, with a passkey, and written to Dockwell's own file");
  await f2b.getByRole("button", { name: "Edit the sshd jail" }).click();
  const jail = page.getByRole("dialog", { name: /^(The sshd jail on|Protect SSH on) / });
  const tries = jail.getByLabel("Failed sign-ins that ban an address");
  await tries.fill("3");
  await jail.getByRole("button", { name: "Plan it" }).click();
  const jailPlan = page.getByRole("dialog", { name: "Review plan" });
  await jailPlan.getByRole("button", { name: /Apply plan/ }).click();
  // The browser's virtual authenticator gives the passkey a jail takes; no timer follows.
  await page
    .getByRole("button", { name: /^(Done|Close)$/ })
    .first()
    .click({ timeout: 30_000 });
  const tools = env?.["DOCKWELL_TEST_TOOLS"];
  if (tools) {
    const written = readFileSync(join(tools, "etc", "fail2ban", "jail.d", "dockwell.conf"), "utf8");
    if (!written.includes("[sshd]") || !written.includes("maxretry = 3")) fail(`Dockwell's jail file reads ${written}`);
  }
  await f2b.getByText("Dockwell writes this jail.").waitFor({ timeout: 15_000 });
  await page.setViewportSize({ width: 390, height: 844 });
  await f2b.scrollIntoViewIfNeeded();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 390) fail(`the Firewall tab is ${wide} px wide on a 390 px phone`);
  await shoot(page, "server-firewall-access-phone");
  await page.setViewportSize({ width: 1440, height: 900 });

  step(
    "a rule of Dockwell's own applies with a passkey, waits for you to say you can still reach the server, and stays",
  );
  const rules = page.getByRole("region", { name: "Dockwell's rules" });
  await rules.getByRole("button", { name: "Add a rule" }).click();
  const add = page.getByRole("dialog", { name: /^Add a rule to / });
  await add.getByLabel("Port").fill("8443");
  await add.getByRole("button", { name: "Plan it" }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  // The browser's virtual authenticator gives the passkey the change takes. Then the server waits.
  const keep = page.getByRole("button", { name: "Keep the change" });
  await keep.waitFor({ timeout: 30_000 });
  // The test names SSH's port where the server has an SSH server, and none where it has none, as here.
  const test = await page.getByText(/^Open a new SSH session /).innerText();
  if (!test.includes("with your key")) fail(`the confirmation's test says ${test}`);
  await shoot(page, "server-firewall-confirm");
  await keep.click();
  await page.getByRole("button", { name: "Keep the change" }).waitFor({ state: "detached", timeout: 15_000 });
  await page
    .getByRole("button", { name: /^(Done|Close)$/ })
    .first()
    .click();
  await rules.getByRole("row", { name: /8443\/tcp/ }).waitFor({ timeout: 15_000 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 88, run: (page, run) => firewallFlow(page, run) };
