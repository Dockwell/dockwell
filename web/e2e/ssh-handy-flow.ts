// SPDX-License-Identifier: AGPL-3.0-only

import type { Locator, Route } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const message = (...fields: Buffer[]) => Buffer.concat(fields);
const rpc = (name: string) => new RegExp(`/dockwell\\.v1\\.FirewallService/${name}(\\?|$)`);
const respond = (route: Route, body: Buffer) => route.fulfill({ status: 200, contentType: "application/proto", body });

async function fitsHeading(sheet: Locator) {
  const title = sheet.getByRole("heading", { level: 2 });
  if (await title.evaluate((element) => element.scrollWidth > element.clientWidth)) {
    throw new Error("The SSH sheet heading clips the server name");
  }
}

export const flow: Flow = {
  order: 1112,
  async run(page, { origin, step, shoot }) {
    const path = await ownServerPath(page, origin);
    const desktop = { width: 1440, height: 900 };
    const phone = { width: 390, height: 844 };
    const routes = [
      [
        rpc("GetServerSsh"),
        message(
          bytes(
            1,
            message(
              number(4, 1),
              number(5, 2),
              number(6, 22),
              number(7, 1),
              bytes(10, "/etc/ssh/sshd_config.d"),
              bytes(24, "deploy"),
            ),
          ),
        ),
      ],
      [
        rpc("ListServerSshKeys"),
        message(
          bytes(1, "deploy"),
          bytes(2, message(bytes(1, "ssh-ed25519"), bytes(2, "Fixture workstation"), bytes(3, "SHA256:fixture"))),
        ),
      ],
      [
        rpc("ListServerSshSessions"),
        bytes(1, message(bytes(1, "session-fixture"), bytes(2, "deploy"), bytes(3, "192.0.2.44"), bytes(5, "pts/4"))),
      ],
      [
        rpc("ListServerSshLogins"),
        message(
          bytes(1, message(bytes(2, "deploy"), bytes(3, "192.0.2.44"), bytes(4, "publickey"), number(5, 1))),
          bytes(1, message(bytes(2, "guest"), bytes(3, "198.51.100.41"), bytes(4, "password"))),
          bytes(2, message(bytes(1, "198.51.100.41"), number(2, 3))),
          number(4, 1),
        ),
      ],
    ] as const;
    for (const [pattern, body] of routes) await page.route(pattern, (route) => respond(route, body));
    try {
      step("SSH settings, key management, protected sessions and failed logins work at desktop and phone widths");
      await page.goto(`${origin}${path}/firewall`);
      const panel = page.getByRole("region", { name: "SSH", exact: true });
      for (const [name, viewport] of [
        ["desktop", desktop],
        ["phone", phone],
      ] as const) {
        await atWidth(page, viewport);
        await panel.getByRole("button", { name: "Edit settings" }).click();
        const settings = onTop(page.getByRole("dialog", { name: /SSH settings on/ }));
        await fitsHeading(settings);
        await settings.getByRole("switch", { name: "Password login" }).click();
        await settings.getByRole("combobox", { name: "Administrator to test with" }).selectOption("deploy");
        await settings.getByRole("button", { name: "Review plan" }).waitFor();
        await shoot(page, `ssh-handy-settings-${name}`);
        await settings.getByRole("button", { name: "Cancel" }).click();

        await panel.getByRole("button", { name: "Keys", exact: true }).click();
        const keys = onTop(page.getByRole("dialog", { name: /SSH keys on/ }));
        await fitsHeading(keys);
        await keys.getByRole("combobox", { name: "Account", exact: true }).selectOption("deploy");
        await keys.getByText("Fixture workstation").waitFor();
        await keys.getByRole("textbox", { name: "OpenSSH public key" }).fill("-----BEGIN OPENSSH PRIVATE KEY-----");
        await keys.getByRole("button", { name: "Add key" }).click();
        await keys.getByText(/this is a private key/i).waitFor();
        await shoot(page, `ssh-handy-keys-${name}`);
        await keys.getByRole("button", { name: "Close", exact: true }).last().click();

        await panel.getByRole("button", { name: "Sessions", exact: true }).click();
        const sessions = onTop(page.getByRole("dialog", { name: /SSH sessions on/ }));
        await fitsHeading(sessions);
        await sessions.getByText("Protected").waitFor();
        if (await sessions.getByRole("button", { name: "Disconnect", exact: true }).count()) {
          throw new Error("Protected SSH connection offered Disconnect");
        }
        await sessions.getByRole("button", { name: "Close", exact: true }).last().click();

        await panel.getByRole("button", { name: "Login log", exact: true }).click();
        const log = onTop(page.getByRole("dialog", { name: /SSH login log on/ }));
        await fitsHeading(log);
        await log.getByText("3 failed").waitFor();
        await log.getByRole("button", { name: "Ban in Fail2ban" }).waitFor();
        await log.getByRole("combobox", { name: "Result" }).selectOption("failed");
        await shoot(page, `ssh-handy-login-log-${name}`);
        await log.getByRole("button", { name: "Close", exact: true }).last().click();
      }
    } finally {
      await atWidth(page, desktop);
      for (const [pattern] of routes) await page.unroute(pattern);
    }
  },
};
