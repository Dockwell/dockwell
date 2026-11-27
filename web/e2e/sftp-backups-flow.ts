// SPDX-License-Identifier: AGPL-3.0-only

import { type ChildProcess, spawn } from "node:child_process";
import { once } from "node:events";
import { writeFile } from "node:fs/promises";
import { networkInterfaces } from "node:os";
import { join, resolve } from "node:path";
import readline from "node:readline";

import type { Flow } from "./flows.ts";
import { atWidth, pastTheEdge } from "./sheets.ts";

async function fixture(child: ChildProcess) {
  if (!child.stdout) throw new Error("The SFTP fixture has no stdout");
  const lines = readline.createInterface({ input: child.stdout });
  try {
    const ended = once(child, "exit").then(([code]) => {
      throw new Error(`The SFTP fixture exited before listening (${code})`);
    });
    const [line] = await Promise.race([once(lines, "line"), ended]);
    const [address, fingerprint] = String(line).split(" ");
    const port = address?.split(":").at(-1);
    if (!port || !fingerprint?.startsWith("SHA256:"))
      throw new Error("The SFTP fixture didn't report its port and key");
    return { port, fingerprint };
  } finally {
    lines.close();
  }
}

export const flow: Flow = {
  order: 98.3,
  run: async (page, { origin, step, shoot, data, bin }) => {
    // The test endpoint follows the same address rules as production. Bind only
    // to a local non-loopback interface, with only the generated key authorized.
    const host = Object.values(networkInterfaces())
      .flat()
      .find((ip) => ip?.family === "IPv4" && !ip.internal)?.address;
    if (!host) throw new Error("The SFTP flow needs a non-loopback IPv4 interface");
    const keys = join(data, "sftp-fixture-authorized-keys");
    const child = spawn(resolve(bin, "../sftpserve"), [`${host}:0`, keys], { stdio: ["ignore", "pipe", "inherit"] });
    const stopped = once(child, "close");
    const originalSize = page.viewportSize();
    try {
      const { port, fingerprint } = await fixture(child);
      step("SFTP: generate a key and verify the server's fingerprint before sign-in");
      await atWidth(page, { width: 1440, height: 1000 });
      await page.goto(`${origin}/settings/backups`);
      const destinations = page.getByRole("region", { name: "Destinations" });
      await destinations.getByRole("button", { name: /^Add( storage)?$/ }).click();
      await page
        .getByRole("dialog", { name: "Add backup storage" })
        .getByRole("button", { name: "SFTP", exact: true })
        .click();
      const form = page.getByRole("dialog", { name: "Add SFTP destination" });
      if ((await form.getByRole("textbox", { name: "Port", exact: true }).inputValue()) !== "22") {
        throw new Error("SFTP did not default to port 22");
      }
      await form.getByRole("textbox", { name: "Name", exact: true }).fill("sftp-example");
      await form.getByRole("textbox", { name: "Host", exact: true }).fill(host);
      await form.getByRole("textbox", { name: "Port", exact: true }).fill(port);
      await form.getByRole("textbox", { name: "User", exact: true }).fill("backups");
      const publicKey = await form.getByText(/^ssh-ed25519 /).textContent();
      if (!publicKey) throw new Error("No generated public key appeared");
      const server = form.getByLabel("Test from server");
      await server.locator("option").nth(1).waitFor({ state: "attached" });
      await server.selectOption({ index: 1 });
      await form.getByRole("button", { name: "Test destination" }).click();
      await form.getByText(`ssh-ed25519 ${fingerprint}`, { exact: true }).waitFor();
      await shoot(page, "sftp-destination-fingerprint-1440");

      step("SFTP: test the folder, write, read, delete and free space at phone width");
      await atWidth(page, { width: 390, height: 844 });
      await form.getByRole("button", { name: "Trust fingerprint and test" }).click();
      await form
        .getByText("The server refused the sign-in. Check the user and SSH key or password, then test again.")
        .waitFor();
      await writeFile(keys, publicKey, { mode: 0o600 });
      await form.getByRole("button", { name: "Test destination" }).click();
      await form.getByText("Destination test passed").waitFor({ timeout: 20_000 });
      for (const label of [
        "Verify host key",
        "Sign in",
        "Open folder",
        "Write test file",
        "Read test file",
        "Delete test file",
        "Free space",
      ]) {
        await form.getByText(label, { exact: true }).waitFor();
      }
      const { wider, what } = await pastTheEdge(page);
      if (wider > 0) throw new Error(`SFTP destination sheet overflows the phone by ${wider}px at ${what}`);
      await shoot(page, "sftp-destination-checks-390");
      await form.getByRole("checkbox", { name: "I've kept a copy of the passphrase" }).check();
      await form.getByRole("button", { name: "Review plan" }).click();
      const plan = page.locator(".plan-sheet");
      await plan.getByRole("button", { name: /Apply plan/ }).waitFor({ timeout: 15_000 });
      if ((await plan.innerText()).includes("BEGIN PRIVATE KEY")) throw new Error("The plan displayed the private key");
      await plan.getByRole("button", { name: /Apply plan/ }).click();
      await plan.getByRole("button", { name: "Done" }).click({ timeout: 30_000 });
      await destinations.getByText("sftp-example", { exact: true }).waitFor();
      const savedPublicKey = publicKey.split(" ").slice(0, 2).join(" ");
      await destinations.getByText(savedPublicKey, { exact: true }).waitFor();

      step("SFTP: control plane recovery offers S3-compatible destinations");
      const control = page.getByRole("region", { name: "Control plane" });
      await control.getByRole("button", { name: /Change destination|Pick a destination/ }).click();
      const recovery = page.getByRole("dialog", { name: "Off-site destination" });
      await recovery.getByText("Control plane backup recovery and retention require S3-compatible storage.").waitFor();
      if ((await recovery.getByRole("option", { name: "sftp-example" }).count()) !== 0) {
        throw new Error("The control plane destination picker listed SFTP");
      }
      await recovery.getByRole("button", { name: "Cancel" }).click();

      step("SFTP: edit the saved destination on a phone in Simplified Chinese");
      const previous = await page.evaluate(() => {
        const old = localStorage.getItem("dockwell.locale");
        localStorage.setItem("dockwell.locale", "zh-CN");
        return old;
      });
      try {
        await page.reload();
        await page.getByRole("button", { name: "编辑此 SFTP 目的地" }).click();
        const translated = page.getByRole("dialog", { name: "编辑 SFTP 目的地" });
        await translated.getByText(savedPublicKey, { exact: true }).waitFor();
        await translated.getByRole("button", { name: "复制公钥", exact: true }).waitFor();
        const selected = translated.getByLabel("发起测试的服务器");
        await selected.locator("option").nth(1).waitFor({ state: "attached" });
        await selected.selectOption({ index: 1 });
        await translated.getByRole("button", { name: "测试备份目的地" }).click();
        await translated.getByText("备份目的地测试通过", { exact: true }).waitFor();
        const { wider, what } = await pastTheEdge(page);
        if (wider > 0) throw new Error(`Translated SFTP destination sheet overflows by ${wider}px at ${what}`);
      } finally {
        await page.evaluate((old) => {
          if (old) localStorage.setItem("dockwell.locale", old);
          else localStorage.removeItem("dockwell.locale");
        }, previous);
        await page.reload();
      }
    } finally {
      child.kill("SIGTERM");
      await stopped;
      if (originalSize) await atWidth(page, originalSize);
    }
  },
};
