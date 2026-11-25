// SPDX-License-Identifier: AGPL-3.0-only

import { mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

export function toolboxTools(dir: string) {
  const write = (name: string, contents: string, mode = 0o644) => writeFileSync(join(dir, name), contents, { mode });
  for (const path of [
    "bin",
    "etc/sysctl.d",
    "etc/systemd/system",
    "proc/sys/vm",
    "var/cache/apt/archives",
    "var/lib/apt/lists",
    "var/lib/dpkg",
    "usr/bin",
    "usr/lib/systemd/system/service.d",
  ]) {
    mkdirSync(join(dir, path), { recursive: true });
  }
  write("etc/hostname", "ams-1-03\n");
  write("etc/hosts", "127.0.0.1 localhost\n192.0.2.15 cache.internal # Dockwell toolbox\n");
  write("etc/resolv.conf", "nameserver 1.1.1.1\nnameserver 9.9.9.9\n");
  write("proc/swaps", "Filename Type Size Used Priority\n/var/swap file 1048572 131072 -2\n");
  write("proc/meminfo", "MemTotal: 2048000 kB\nMemAvailable: 1024000 kB\n");
  write("proc/sys/vm/swappiness", "60\n");
  write("var/cache/apt/archives/toolbox-fixture.deb", "package fixture\n");
  write("etc/systemd/system/dockwell-toolbox-queue-worker.service", "# Managed by Dockwell toolbox\n");
  write("usr/lib/systemd/system/service.d/10-restart.conf", "[Service]\nRestartSec=3\n");
  write("bin/sh", "fixture interpreter\n", 0o755);
  for (const tool of ["sh", "flock", "cp", "mv", "mktemp", "rm"]) {
    symlinkSync(`/usr/bin/${tool}`, join(dir, "usr/bin", tool));
  }
  write(
    "usr/bin/systemd-run",
    '#!/bin/sh\ncase "$2" in --unit=dockwell-toolbox-revert-chg_*) exit 0 ;; *) exit 1 ;; esac\n',
    0o755,
  );
  write("usr/bin/toolbox-worker", "#!/bin/sh\nexit 0\n", 0o755);
  write("usr/bin/hostnamectl", "#!/bin/sh\necho ams-1-03\n", 0o755);
  write(
    "usr/bin/timedatectl",
    "#!/bin/sh\ncase \"$2\" in --property=Timezone) echo Europe/Amsterdam;; --no-pager) printf 'UTC\\nEurope/Amsterdam\\nAsia/Shanghai\\n';; *) printf 'CanNTP=yes\\nNTP=yes\\nNTPSynchronized=yes\\n';; esac\n",
    0o755,
  );
  write(
    "usr/bin/sysctl",
    `#!/bin/sh\ncase "$2" in vm.swappiness=*) printf '%s\\n' "${"$"}{2#vm.swappiness=}" > '${join(dir, "proc/sys/vm/swappiness")}' ;; *) exit 1 ;; esac\n`,
    0o755,
  );
}

export const flow: Flow = {
  order: 97,
  async run(page, { origin, step, shoot }) {
    step("host toolbox reads the server and reviews a swappiness change before applying it");
    const server = await ownServerPath(page, origin);
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}${server}/toolbox`);
    const swap = page.getByRole("region", { name: "Swap", exact: true });
    await swap.getByText("/var/swap", { exact: true }).waitFor({ timeout: 20_000 });
    await page.getByText("Synchronized", { exact: true }).waitFor();
    await shoot(page, "toolbox-overview");
    await swap.getByRole("button", { name: "Change swappiness" }).click();
    const editor = onTop(page.getByRole("dialog", { name: "Swappiness" }));
    await editor.getByRole("spinbutton", { name: "Swappiness" }).fill("35");
    await editor.getByRole("button", { name: "Review plan" }).click();
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await plan.getByText("vm.swappiness", { exact: true }).waitFor({ timeout: 20_000 });
    await plan.getByText("35", { exact: true }).waitFor();
    await plan.getByRole("button", { name: "Apply plan", exact: true }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });
    await swap.getByText("35", { exact: true }).waitFor({ timeout: 20_000 });

    await page.getByRole("button", { name: "Scan files", exact: true }).click();
    await onTop(page.getByRole("dialog", { name: "Scan host files" }))
      .getByRole("button", { name: "Scan files" })
      .click();
    await plan.getByText("/var/cache/apt/archives/toolbox-fixture.deb", { exact: true }).waitFor({ timeout: 20_000 });
    await page.keyboard.press("Escape");
    await plan.waitFor({ state: "hidden" });

    step("host toolbox keeps a hosts entry after the agent opens a fresh control connection");
    await page.getByRole("button", { name: "Add hosts entry", exact: true }).click();
    const hosts = onTop(page.getByRole("dialog", { name: "Add hosts entry" }));
    await hosts.getByLabel("Hostname", { exact: true }).fill("toolbox-check.internal");
    await hosts.getByLabel("IP address", { exact: true }).fill("192.0.2.24");
    await hosts.getByRole("button", { name: "Review plan" }).click();
    await plan.getByText("192.0.2.24", { exact: true }).waitFor({ timeout: 20_000 });
    const code = await plan.locator("#plan-confirm-why code").innerText();
    await plan.getByRole("textbox", { name: /to apply it/ }).fill(code);
    await plan.getByRole("button", { name: "Apply plan", exact: true }).click();
    await page.getByText("The agent opened a fresh connection to the control server.").waitFor({ timeout: 30_000 });
    await page.getByRole("button", { name: "Keep the change" }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });
    await page.getByText("toolbox-check.internal", { exact: true }).waitFor({ timeout: 20_000 });

    step("host toolbox shows the service unit before creating it");
    await page.getByRole("button", { name: "New service" }).click();
    const service = onTop(page.getByRole("dialog", { name: "New background service" }));
    await service.getByLabel("Name", { exact: true }).fill("toolbox-check");
    await service.getByLabel("Executable", { exact: true }).fill("/usr/bin/toolbox-worker");
    await service.getByLabel("Run as user", { exact: true }).fill("root");
    await service.getByText("Service settings", { exact: true }).click();
    const directValue = "toolbox-fixture-private-value";
    await service.getByLabel("Direct environment name", { exact: true }).fill("PATH");
    await service.getByLabel("Direct environment value", { exact: true }).fill(directValue);
    await service.getByRole("button", { name: "Add direct value", exact: true }).click();
    await service.getByRole("button", { name: "Review plan" }).click();
    await plan.getByRole("heading", { name: "Systemd unit file" }).waitFor({ timeout: 20_000 });
    await plan.getByText('ExecStart=:"/usr/bin/toolbox-worker"', { exact: false }).waitFor();
    await plan.getByText("/usr/lib/systemd/system/service.d/10-restart.conf", { exact: true }).waitFor();
    await plan.getByText("Secret · Hidden", { exact: true }).waitFor();
    if ((await plan.getByText(directValue, { exact: false }).count()) !== 0) {
      throw new Error("the toolbox plan exposed a directly entered environment value");
    }
    await plan.getByText(/^Confirm creating toolbox-check on .* as root with your passkey\.$/).waitFor();
    await shoot(page, "toolbox-service-plan");
    const serviceCode = await plan.locator("#plan-confirm-why code").innerText();
    await plan.getByRole("textbox", { name: /to apply it/ }).fill(serviceCode);
    await plan.getByRole("button", { name: "Apply plan", exact: true }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });
    await page.getByRole("link", { name: "dockwell-toolbox-toolbox-check.service" }).waitFor({ timeout: 20_000 });

    await atWidth(page, { width: 390, height: 844 });
    if ((await page.evaluate(() => document.documentElement.scrollWidth)) > 390) {
      throw new Error("the toolbox is wider than the phone viewport");
    }
    await shoot(page, "toolbox-overview-phone");
    await atWidth(page, { width: 1440, height: 900 });
  },
};
