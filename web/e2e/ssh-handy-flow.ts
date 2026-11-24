// SPDX-License-Identifier: AGPL-3.0-only

import { BinaryReader } from "@bufbuild/protobuf/wire";
import type { Locator, Page, Route } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const message = (...fields: Buffer[]) => Buffer.concat(fields);
const rpc = (name: string) => new RegExp(`/dockwell\\.v1\\.FirewallService/${name}(\\?|$)`);
const respond = (route: Route, body: Buffer) => route.fulfill({ status: 200, contentType: "application/proto", body });
const authRpc = (name: string) => new RegExp(`/dockwell\\.v1\\.AuthService/${name}(\\?|$)`);
const addedKey =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOo8aYl+Fb2y0L53TbVo7kP1E9qnqqVxYz8SSpBBt+/G Temporary workstation";
const addedFingerprint = "SHA256:temporary";

function field(body: Uint8Array, want: number): string {
  const reader = new BinaryReader(body);
  while (reader.pos < reader.len) {
    const [tag, type] = reader.tag();
    if (tag === want) return reader.string();
    reader.skip(type);
  }
  return "";
}

async function sshPasskey(
  page: Page,
  name: string,
  action: number,
  target: string,
  proofField: number,
  click: Locator,
) {
  let submitted: string | undefined;
  const begin = page.waitForRequest((request) => authRpc("BeginStepUp").test(request.url()));
  const finish = page.waitForResponse((response) => authRpc("FinishStepUp").test(response.url()) && response.ok());
  const handler = async (route: Route) => {
    const proof = field(route.request().postDataBuffer() ?? Buffer.alloc(0), proofField);
    if (proof) {
      submitted = proof;
      await respond(route, Buffer.alloc(0));
      return;
    }
    await route.fulfill({
      status: 403,
      contentType: "application/json",
      headers: { "Dockwell-Error": "needs_step_up" },
      json: {
        code: "permission_denied",
        message: "Confirm with a passkey",
        details: [
          { type: "dockwell.v1.StepUpNeeded", value: message(number(1, action), bytes(2, target)).toString("base64") },
        ],
      },
    });
  };
  await page.route(rpc(name), handler);
  try {
    const completed = page.waitForResponse((response) => rpc(name).test(response.url()) && response.ok());
    await click.click();
    const asked = await begin;
    if (field(asked.postDataBuffer() ?? Buffer.alloc(0), 2) !== target) {
      throw new Error(`The passkey was offered for a different target than ${target}`);
    }
    const proof = field(await (await finish).body(), 1);
    await completed;
    if (!proof || submitted !== proof) throw new Error(`The passkey proof never reached ${name}`);
  } finally {
    await page.unroute(rpc(name), handler);
  }
}

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
    const server = path.split("/").at(-1);
    if (!server) throw new Error("The SSH fixture has no server ID");
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
              bytes(30, "192.0.2.44"),
            ),
          ),
        ),
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
    let keyAdded = false;
    let extraSession = false;
    const fixtureKey = bytes(
      2,
      message(
        bytes(1, "ssh-ed25519"),
        bytes(2, "Fixture workstation"),
        bytes(3, "SHA256:fixture"),
        bytes(5, 'command="true"'),
        bytes(5, 'from="192.0.2.0/24"'),
      ),
    );
    const keysFixture = (route: Route) =>
      respond(
        route,
        message(
          bytes(1, "deploy"),
          fixtureKey,
          ...(keyAdded
            ? [
                bytes(
                  2,
                  message(bytes(1, "ssh-ed25519"), bytes(2, "Temporary workstation"), bytes(3, addedFingerprint)),
                ),
              ]
            : []),
        ),
      );
    const sessionsFixture = (route: Route) =>
      respond(
        route,
        message(
          bytes(1, message(bytes(1, "session-fixture"), bytes(2, "deploy"), bytes(3, "192.0.2.44"), bytes(5, "pts/4"))),
          ...(extraSession
            ? [
                bytes(
                  1,
                  message(
                    bytes(1, "session-ready"),
                    bytes(2, "deploy"),
                    bytes(3, "192.0.2.45"),
                    bytes(5, "pts/5"),
                    number(6, 1),
                  ),
                ),
              ]
            : []),
        ),
      );
    for (const [pattern, body] of routes) await page.route(pattern, (route) => respond(route, body));
    await page.route(rpc("ListServerSshKeys"), keysFixture);
    await page.route(rpc("ListServerSshSessions"), sessionsFixture);
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
        await settings.locator('input[value="192.0.2.44"]').waitFor();
        await settings.getByRole("combobox", { name: "SSH test client address" }).fill("192.0.2.72");
        await settings.getByRole("button", { name: "Review plan" }).waitFor();
        await shoot(page, `ssh-handy-settings-${name}`);
        await settings.getByRole("button", { name: "Cancel" }).click();

        await panel.getByRole("button", { name: "Keys", exact: true }).click();
        const keys = onTop(page.getByRole("dialog", { name: /SSH keys on/ }));
        await fitsHeading(keys);
        await keys.getByRole("combobox", { name: "Account", exact: true }).selectOption("deploy");
        await keys.getByText("Fixture workstation").waitFor();
        await keys.getByText("Runs a command").waitFor();
        await keys.getByText('from="192.0.2.0/24"').waitFor();
        await keys.getByRole("textbox", { name: "OpenSSH public key" }).fill("-----BEGIN OPENSSH PRIVATE KEY-----");
        await keys.getByRole("button", { name: "Add key" }).click();
        await keys.getByText(/this is a private key/i).waitFor();
        await shoot(page, `ssh-handy-keys-${name}`);
        if (name === "desktop") {
          step("add and remove an SSH key with the browser's virtual passkey");
          await keys.getByRole("textbox", { name: "OpenSSH public key" }).fill(addedKey);
          keyAdded = true;
          await sshPasskey(
            page,
            "EditServerSshKey",
            264,
            `${server}:deploy:add:fixture`,
            5,
            keys.getByRole("button", { name: "Add key" }),
          );
          await keys.getByText("Temporary workstation").waitFor();
          const added = keys.getByRole("listitem").filter({ hasText: "Temporary workstation" });
          keyAdded = false;
          await sshPasskey(
            page,
            "EditServerSshKey",
            264,
            `${server}:deploy:remove:${addedFingerprint}`,
            5,
            added.getByRole("button", { name: "Remove key" }),
          );
          await added.waitFor({ state: "hidden" });
        }
        await keys.getByRole("button", { name: "Close", exact: true }).last().click();

        await panel.getByRole("button", { name: "Sessions", exact: true }).click();
        const sessions = onTop(page.getByRole("dialog", { name: /SSH sessions on/ }));
        await fitsHeading(sessions);
        await sessions.getByText("Protected").waitFor();
        const protectedRow = sessions.getByRole("listitem").filter({ hasText: "192.0.2.44" });
        if (await protectedRow.getByRole("button", { name: "Disconnect", exact: true }).count()) {
          throw new Error("Protected SSH connection offered Disconnect");
        }
        await sessions.getByRole("button", { name: "Close", exact: true }).last().click();
        if (name === "desktop") {
          step("disconnect an SSH session with the browser's virtual passkey");
          extraSession = true;
          await panel.getByRole("button", { name: "Sessions", exact: true }).click();
          const active = sessions.getByRole("listitem").filter({ hasText: "192.0.2.45" });
          await active.waitFor();
          extraSession = false;
          await sshPasskey(
            page,
            "DisconnectServerSshSession",
            265,
            `${server}:session-ready`,
            3,
            active.getByRole("button", { name: "Disconnect" }),
          );
          await active.waitFor({ state: "hidden" });
          await sessions.getByRole("button", { name: "Close", exact: true }).last().click();
        }

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
      await page.unroute(rpc("ListServerSshKeys"), keysFixture);
      await page.unroute(rpc("ListServerSshSessions"), sessionsFixture);
    }
  },
};
