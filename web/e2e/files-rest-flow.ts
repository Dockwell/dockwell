// SPDX-License-Identifier: AGPL-3.0-only

import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";

export const flow: Flow = {
  order: 56,
  async run(page, { origin, step, shoot }) {
    const folder = mkdtempSync("/tmp/dockwell-files-rest-");
    const nested = join(folder, "notes");
    mkdirSync(nested);
    chmodSync(nested, 0o700);
    writeFileSync(join(nested, "guide.txt"), "One line\nA lighthouse at the harbor\n");
    writeFileSync(join(folder, "script.sh"), "#!/bin/sh\n", { mode: 0o644 });
    const fixture = createServer((req, res) => {
      if (req.url === "/slow.txt") {
        res.writeHead(200, { "content-length": "4096" });
        res.write("still downloading");
      } else {
        res.end("Downloaded from the fixture\n");
      }
    });
    await new Promise<void>((resolve, reject) => {
      fixture.once("error", reject);
      fixture.listen(0, "127.0.0.1", resolve);
    });
    const address = fixture.address();
    if (!address || typeof address === "string") throw new Error("download fixture has no port");
    const source = `http://127.0.0.1:${address.port}`;
    try {
      await atWidth(page, { width: 1440, height: 900 });
      const server = await ownServerPath(page, origin);
      const files = `${origin}${server}/files?${new URLSearchParams({ path: folder })}`;
      await page.goto(files);
      const table = page.getByRole("table", { name: `Files in ${folder}` });

      step("files-rest: calculate a folder size and keep a pin across reloads");
      const notes = table.getByRole("row").filter({ hasText: "notes" });
      await notes.getByRole("button", { name: "Calculate size of notes" }).click();
      await notes.getByText("36 B", { exact: true }).waitFor();
      await notes.getByText("notes", { exact: true }).click();
      await page.locator(".files-details").getByText("36 B", { exact: true }).waitFor();
      await shoot(page, "files-rest-size");
      await page.getByRole("button", { name: "Pin folder", exact: true }).click();
      const pinned = page.getByRole("group", { name: "Pinned folders" });
      await pinned.getByRole("link", { name: /dockwell-files-rest-/ }).waitFor();
      await page.reload();
      await pinned.getByRole("link", { name: /dockwell-files-rest-/ }).waitFor();
      await shoot(page, "files-rest-pinned");

      step("files-rest: change permissions from the file's row menu");
      await table.getByText("script.sh", { exact: true }).click({ button: "right" });
      await page
        .getByRole("menu", { name: "Actions for script.sh" })
        .getByRole("menuitem", { name: "Permissions and owner" })
        .click();
      const permissions = page.getByRole("dialog", { name: "Permissions and owner" });
      await permissions.getByRole("combobox", { name: "Owner", exact: true }).waitFor();
      await permissions.getByRole("checkbox", { name: "Owner: Run" }).check();
      await permissions.getByText("0744", { exact: true }).waitFor();
      await shoot(page, "files-rest-permissions");
      await permissions.getByRole("button", { name: "Apply changes" }).click();
      await page.getByText("Permissions and ownership updated").waitFor();
      if ((statSync(join(folder, "script.sh")).mode & 0o777) !== 0o744) throw new Error("permissions did not change");

      step("files-rest: apply folder and file presets, then undo");
      await table.getByText("notes", { exact: true }).click({ button: "right" });
      await page
        .getByRole("menu", { name: "Actions for notes" })
        .getByRole("menuitem", { name: "Permissions and owner" })
        .click();
      await permissions.getByRole("checkbox", { name: "And everything inside" }).check();
      await permissions.getByRole("combobox", { name: "Folder and file presets" }).selectOption({ label: "755 / 644" });
      await permissions.getByRole("table", { name: "Folders" }).getByText("0755", { exact: true }).waitFor();
      await permissions.getByRole("table", { name: "Files" }).getByText("0644", { exact: true }).waitFor();
      await shoot(page, "files-rest-recursive");
      await permissions.getByRole("button", { name: "Apply changes" }).click();
      await page.getByRole("button", { name: "Undo", exact: true }).waitFor();
      if ((statSync(nested).mode & 0o777) !== 0o755) throw new Error("folder became non-traversable");
      if ((statSync(join(nested, "guide.txt")).mode & 0o777) !== 0o644) throw new Error("file got execute bits");
      await page.getByRole("button", { name: "Undo", exact: true }).click();
      await page.getByText("Previous access restored").waitFor();
      if ((statSync(nested).mode & 0o777) !== 0o700) throw new Error("folder permissions were not restored");

      step("files-rest: find text in a subfolder and open the matching file");
      await page.getByRole("button", { name: "Search below this folder" }).click();
      const search = page.getByRole("dialog", { name: "Search below this folder" });
      await search.getByRole("textbox", { name: "Search term" }).fill("lighthouse");
      await search.getByRole("checkbox", { name: "Search inside text files too" }).check();
      await search.getByRole("button", { name: "Search", exact: true }).click();
      const match = search.getByRole("button", { name: /guide.txt.*line 2/ });
      await match.waitFor();
      await shoot(page, "files-rest-search");
      await match.click();
      await page.getByRole("textbox", { name: "Contents of guide.txt" }).waitFor();
      if (new URL(page.url()).searchParams.get("line") !== "2") throw new Error("search result did not open at line 2");

      step("files-rest: download a public-style URL via the local fixture and cancel another");
      await page.goto(files);
      async function download(path: string) {
        await page.getByRole("button", { name: "Download from a URL" }).click();
        const sheet = page.getByRole("dialog", { name: "Download from a URL" });
        await sheet.getByRole("textbox", { name: "Download URL" }).fill(source + path);
        if (path === "/fetched.txt") await shoot(page, "files-rest-download");
        await sheet.getByRole("button", { name: "Start download" }).click();
      }
      await download("/fetched.txt");
      await page.getByText("Downloaded fetched.txt").waitFor();
      if (readFileSync(join(folder, "fetched.txt"), "utf8") !== "Downloaded from the fixture\n") {
        throw new Error("the download saved the wrong bytes");
      }
      await download("/slow.txt");
      await page.getByRole("button", { name: "Cancel download of slow.txt" }).click();
      await page.getByText("Download cancelled").waitFor();
      if (await table.getByText("slow.txt", { exact: true }).count()) throw new Error("cancel left a partial file");

      step("files-rest: open a host terminal in the selected folder");
      await table.getByText("notes", { exact: true }).click({ button: "right" });
      await page
        .getByRole("menu", { name: "Actions for notes" })
        .getByRole("menuitem", { name: "Open in terminal" })
        .click();
      await page.waitForURL(/\/terminal\?/);
      const shell = page.locator(".terminal-screen:not([hidden]) .xterm");
      await shell.click();
      await page.keyboard.type("pwd");
      await page.keyboard.press("Enter");
      await shell.locator(".xterm-rows").getByText(nested, { exact: true }).waitFor();
      await shoot(page, "files-rest-terminal");
    } finally {
      fixture.closeAllConnections();
      await new Promise<void>((resolve) => fixture.close(() => resolve()));
      rmSync(folder, { recursive: true, force: true });
    }
  },
};
