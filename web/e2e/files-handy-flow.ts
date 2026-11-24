// SPDX-License-Identifier: AGPL-3.0-only

// End-to-end flow for the upgraded file manager: deep folder tree resize and
// horizontal scroll, single-click details & image preview (<img>, never inline SVG),
// multi-selection bar with multi-download zip and multi-delete, right-click context
// menus with inline rename / compress / extract, and the lazy-loaded CodeMirror 6
// multi-tab editor in full-window mode.

import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

const PNG_1X1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64",
);

const SVG_ICON = `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64"><rect width="64" height="64" rx="12" fill="#2450d6"/><circle cx="32" cy="32" r="14" fill="#ffffff"/></svg>\n`;

async function readCode(code: ReturnType<Page["getByRole"]>): Promise<string> {
  return code.evaluate((el) => [...el.querySelectorAll(".cm-line")].map((line) => line.textContent ?? "").join("\n"));
}

async function writeCode(page: Page, code: ReturnType<Page["getByRole"]>, value: string): Promise<void> {
  await code.click();
  for (let i = 0; i < 40; i++) {
    await page.keyboard.press("ControlOrMeta+a");
    await page.keyboard.press("Backspace");
    if ((await readCode(code)) === "") break;
    await page.waitForTimeout(50);
  }
  if (!value) return;
  await code.evaluate((el, text) => {
    const dt = new DataTransfer();
    dt.setData("text/plain", text);
    el.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
  }, value);
  for (let i = 0; i < 40; i++) {
    if ((await readCode(code)) === value) return;
    await page.waitForTimeout(50);
  }
  fail(`editor did not receive pasted text: got ${JSON.stringify(await readCode(code))}`);
}

export const flow: Flow = {
  order: 55,
  async run(page, { origin, step, shoot }) {
    // Use /tmp explicitly so test files never sit inside a home directory (which requires passkey step-up).
    const root = mkdtempSync(join("/tmp", "dockwell-handy-"));
    try {
      const deep = join(root, "srv", "clusters", "production", "edge-proxy", "sites-enabled", "customer-app");
      mkdirSync(deep, { recursive: true });
      writeFileSync(join(deep, "server.ts"), 'export const port = 8080;\nconsole.log("ready");\n');
      writeFileSync(join(deep, "routes.json"), '{\n  "health": "/healthz"\n}\n');
      writeFileSync(join(deep, "badge.png"), PNG_1X1);
      writeFileSync(join(deep, "mark.svg"), SVG_ICON);
      writeFileSync(join(deep, "draft.txt"), "one\ntwo\n");
      writeFileSync(join(deep, "extra.txt"), "three\n");

      await atWidth(page, { width: 1440, height: 900 });
      const serverPath = await ownServerPath(page, origin);
      await page.goto(`${origin}${serverPath}`);
      await page
        .getByRole("navigation", { name: /^Server / })
        .getByRole("link", { name: "Files" })
        .click();
      await page.getByRole("table", { name: "Files in /" }).waitFor();
      const serverId = serverPath.split("/")[2] ?? fail(`no server in ${serverPath}`);
      const at = (path: string) => `${origin}/servers/${serverId}/files?${new URLSearchParams({ path }).toString()}`;

      step("files-handy: deep folder tree reveals current folder and resizes");
      await page.goto(at(deep));
      const table = page.getByRole("table", { name: `Files in ${deep}` });
      await table.getByText("server.ts").waitFor();
      const splitter = page.getByRole("separator", { name: "Resize folder tree" });
      await splitter.focus();
      const beforeWidth = Number(await splitter.getAttribute("aria-valuenow"));
      await page.keyboard.press("ArrowRight");
      await page.keyboard.press("ArrowRight");
      const afterWidth = Number(await splitter.getAttribute("aria-valuenow"));
      if (afterWidth <= beforeWidth) fail(`tree splitter did not widen (${beforeWidth} -> ${afterWidth})`);
      await shoot(page, "files-handy-deep-tree");

      step("files-handy: single-click image preview renders strictly as <img>");
      await table.getByText("mark.svg").click();
      const previewImg = page.getByRole("img", { name: "mark.svg" });
      await previewImg.waitFor();
      const src = await previewImg.getAttribute("src");
      if (!src?.startsWith("data:image/svg+xml;base64,")) fail(`expected data: SVG image src, got ${src}`);
      if ((await page.locator(".files-details-stage svg").count()) > 0) {
        fail("SVG was rendered inline instead of inside an <img>");
      }
      await shoot(page, "files-handy-image-preview");

      step("files-handy: multi-selection bar downloads zip and deletes with count");
      await page.keyboard.press("Escape");
      await table.getByRole("checkbox", { name: "Select draft.txt" }).click();
      await table.getByRole("checkbox", { name: "Select extra.txt" }).click();
      const bar = page.getByRole("toolbar", { name: "Selection actions" });
      await bar.getByText("2 selected").waitFor();
      await shoot(page, "files-handy-multi-select");
      const [zipDownload] = await Promise.all([
        page.waitForEvent("download"),
        bar.getByRole("link", { name: "Download" }).click(),
      ]);
      const zipHead = readFileSync(await zipDownload.path())
        .subarray(0, 2)
        .toString();
      if (zipHead !== "PK") fail(`multi-selection download isn't a zip (${zipHead})`);
      await bar.getByRole("button", { name: "Delete" }).click();
      const delSheet = page.getByRole("dialog", { name: "Delete 2 items" });
      await delSheet.getByRole("button", { name: "Delete", exact: true }).click();
      await page.getByText("Deleted 2 items").waitFor();
      if (existsSync(join(deep, "draft.txt")) || existsSync(join(deep, "extra.txt"))) {
        fail("multi-delete left files on disk");
      }

      step("files-handy: context menu compresses and extracts an archive, and renames inline");
      await table.getByText("routes.json", { exact: true }).click({ button: "right" });
      const rowMenu = page.getByRole("menu", { name: "Actions for routes.json" });
      await rowMenu.waitFor();
      await shoot(page, "files-handy-context-menu");
      if (!(await rowMenu.isVisible())) {
        await table.getByText("routes.json", { exact: true }).click({ button: "right" });
        await rowMenu.waitFor();
      }
      await rowMenu.getByRole("menuitem", { name: "Compress" }).click();
      const compSheet = page.getByRole("dialog", { name: "Compress routes.json" });
      await compSheet.getByRole("textbox", { name: "Archive name" }).fill("routes-bundle.zip");
      await compSheet.getByRole("button", { name: "Compress" }).click();
      await page.getByText("Compressed routes-bundle.zip").waitFor();
      if (!existsSync(join(deep, "routes-bundle.zip"))) fail("compress did not create routes-bundle.zip");

      await table.getByText("routes-bundle.zip", { exact: true }).click({ button: "right" });
      await page
        .getByRole("menu", { name: "Actions for routes-bundle.zip" })
        .getByRole("menuitem", { name: "Extract" })
        .click();
      await page.getByText("Extracted routes-bundle.zip").waitFor();
      if (!existsSync(join(deep, "routes-bundle", "routes.json"))) fail("extract did not unpack routes.json");

      await table.getByText("routes-bundle", { exact: true }).click({ button: "right" });
      await page
        .getByRole("menu", { name: "Actions for routes-bundle" })
        .getByRole("menuitem", { name: /Rename/ })
        .click();
      const inlineRename = page.getByRole("textbox", { name: "New name for routes-bundle" });
      await inlineRename.fill("routes-unpacked");
      await inlineRename.press("Enter");
      await table.getByText("routes-unpacked", { exact: true }).waitFor();
      if (!existsSync(join(deep, "routes-unpacked", "routes.json"))) fail("inline rename did not rename folder");

      step("files-handy: CodeMirror 6 editor with two tabs in full-window mode");
      await table.getByText("server.ts", { exact: true }).dblclick();
      const tsBox = page.getByRole("textbox", { name: "Contents of server.ts" });
      await tsBox.waitFor();
      await writeCode(page, tsBox, 'export const port = 9090;\nconsole.log("updated");\n');
      await table.getByText("routes.json", { exact: true }).dblclick();
      await page.getByRole("textbox", { name: "Contents of routes.json" }).waitFor();
      const tablist = page.getByRole("tablist", { name: "Open files" });
      await tablist.getByRole("tab", { name: /server\.ts/ }).waitFor();
      await tablist.getByRole("tab", { name: /routes\.json/ }).waitFor();
      await page.getByRole("button", { name: "Word wrap" }).click();
      await page.getByRole("button", { name: "Find and replace" }).click();
      await page.locator(".cm-search").waitFor();
      await page.getByRole("button", { name: "Full window editor" }).click();
      await page.locator(".files-viewer.is-fullwindow").waitFor();
      await shoot(page, "files-handy-editor-fullwindow");

      // Switch back to server.ts and save via keyboard shortcut Cmd/Ctrl-S.
      await tablist.getByRole("tab", { name: /server\.ts/ }).click();
      await tsBox.click();
      await page.keyboard.press("ControlOrMeta+s");
      await page.getByText("Saved server.ts").first().waitFor();
      if (!readFileSync(join(deep, "server.ts"), "utf8").includes("9090")) {
        fail("Cmd/Ctrl-S did not save active tab");
      }
      await page.getByRole("button", { name: "Exit full window" }).click();
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  },
};
