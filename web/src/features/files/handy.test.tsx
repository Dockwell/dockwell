// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { EditorView } from "@codemirror/view";
import type { HandlerContext } from "@connectrpc/connect";
import {
  ArchiveJobKind,
  ArchiveJobSchema,
  ArchiveJobState,
  CompressFilesResponseSchema,
  DeleteFileResponseSchema,
  ExtractArchiveResponseSchema,
  FileEntrySchema,
  FileType,
  GetArchiveJobResponseSchema,
  ListArchiveJobsResponseSchema,
  ListFilesResponseSchema,
  ReadFileResponseSchema,
  SaveFileResponseSchema,
} from "@dockwell/api";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { type Api, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function entry(name: string, more: MessageInitShape<typeof FileEntrySchema> = {}) {
  return create(FileEntrySchema, {
    name,
    type: FileType.FILE,
    sizeBytes: 120n,
    mode: "-rw-r--r--",
    owner: "www-data",
    group: "www-data",
    modifyTime: timestampFromDate(new Date(2026, 9, 13)),
    ...more,
  });
}

const site = create(ListFilesResponseSchema, {
  path: "/var/www",
  directory: entry("www", { type: FileType.DIRECTORY }),
  entries: [
    entry("alpha.txt", { sizeBytes: 500n }),
    entry("beta.txt", { sizeBytes: 50n }),
    entry("icon.svg", { sizeBytes: 180n }),
    entry("bundle.zip", { sizeBytes: 2048n }),
    entry("assets", { type: FileType.DIRECTORY, mode: "drwxr-xr-x" }),
  ],
});

async function renderFiles(files: NonNullable<Api["files"]>) {
  async function* watchServers(_req: unknown, ctx: HandlerContext) {
    yield changed(hz1);
    yield snapshotComplete;
    await untilAborted(ctx.signal);
  }
  const rendered = renderAt("/servers/srv_1/files?path=/var/www", { servers: { watchServers }, files });
  await screen.findByRole("table", { name: "Files in /var/www" });
  return rendered;
}

afterEach(() => {
  vi.unstubAllGlobals();
  try {
    window.localStorage.clear();
  } catch {
    // ignore
  }
});

describe("File manager UX", () => {
  it("selects on single click, shows details with text preview, and never double-reads on edit", async () => {
    const readFile = vi.fn(() =>
      create(ReadFileResponseSchema, {
        path: "/var/www/alpha.txt",
        file: entry("alpha.txt"),
        content: "line 1\nline 2\n",
        version: "v1",
      }),
    );
    await renderFiles({ listFiles: () => site, readFile });
    await userEvent.click(screen.getByText("alpha.txt"));

    const details = await screen.findByRole("toolbar", { name: "Actions for alpha.txt" });
    expect(within(details).getByRole("button", { name: "Edit" })).toBeVisible();
    expect(within(details).getByRole("link", { name: "Download alpha.txt" })).toHaveAttribute(
      "href",
      "/v1/servers/srv_1/files:download?path=%2Fvar%2Fwww%2Falpha.txt",
    );
    expect(within(details).getByRole("button", { name: "Rename alpha.txt" })).toBeVisible();
    expect(within(details).getByRole("button", { name: "Compress alpha.txt" })).toBeVisible();
    expect(within(details).getByRole("button", { name: "More options for alpha.txt" })).toBeVisible();
    expect(await screen.findByText(/line 1/)).toBeVisible();
    expect(screen.getByText(/line 2/)).toBeVisible();
    expect(screen.getByText("/var/www/alpha.txt")).toBeVisible();
    expect(readFile).toHaveBeenCalledTimes(1);
  });

  it("renders SVG and binary image previews via readFile(allowBinary) inside <img>, never downloadFile", async () => {
    const downloadFile = vi.fn();
    const readFile = vi.fn((req: { path: string; allowBinary?: boolean }) =>
      create(ReadFileResponseSchema, {
        path: req.path,
        file: entry("icon.svg"),
        content: '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>',
        version: "v1",
      }),
    );
    await renderFiles({ listFiles: () => site, readFile: readFile as never, downloadFile: downloadFile as never });
    await userEvent.click(screen.getByText("icon.svg"));

    const img = await screen.findByRole("img", { name: "icon.svg" });
    expect(img.getAttribute("src")).toMatch(/^data:image\/svg\+xml;base64,/);
    expect(document.querySelector(".files-details script")).toBeNull();
    expect(document.querySelector(".files-details-stage svg")).toBeNull();
    expect(downloadFile).not.toHaveBeenCalled();
    expect(readFile).toHaveBeenCalledWith(
      expect.objectContaining({ serverId: "srv_1", path: "/var/www/icon.svg", allowBinary: true }),
      expect.anything(),
    );
  });

  it("supports multi-selection bar, multi-download zip URL, and multi-delete with count", async () => {
    const deleteFile = vi.fn((req: { path: string }) => create(DeleteFileResponseSchema, { path: req.path }));
    await renderFiles({ listFiles: () => site, deleteFile: deleteFile as never });

    await userEvent.click(screen.getByRole("checkbox", { name: "Select alpha.txt" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Select beta.txt" }));

    const bar = await screen.findByRole("toolbar", { name: "Selection actions" });
    expect(within(bar).getByText("2 selected")).toBeVisible();
    const dl = within(bar).getByRole("link", { name: "Download" });
    expect(dl.getAttribute("href")).toContain("paths=%2Fvar%2Fwww%2Falpha.txt");
    expect(dl.getAttribute("href")).toContain("paths=%2Fvar%2Fwww%2Fbeta.txt");

    await userEvent.click(within(bar).getByRole("button", { name: "Delete" }));
    const sheet = await screen.findByRole("dialog", { name: "Delete 2 items" });
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleteFile).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Deleted 2 items")).toBeVisible();
  });

  it("compresses selected items and extracts an archive via background jobs", async () => {
    let compPolls = 0;
    const compressFiles = vi.fn((req: { archivePath: string }) =>
      create(CompressFilesResponseSchema, {
        job: create(ArchiveJobSchema, {
          jobId: "job_comp_1",
          kind: ArchiveJobKind.COMPRESS,
          state: ArchiveJobState.RUNNING,
          path: req.archivePath,
          entries: 1n,
          bytes: 512n,
        }),
      }),
    );
    const getArchiveJob = vi.fn((req: { jobId: string }) => {
      if (req.jobId === "job_comp_1") {
        compPolls++;
        return create(GetArchiveJobResponseSchema, {
          job: create(ArchiveJobSchema, {
            jobId: "job_comp_1",
            kind: ArchiveJobKind.COMPRESS,
            state: ArchiveJobState.DONE,
            path: "/var/www/alpha.txt.zip",
            entries: 2n,
            bytes: 1024n,
            leftOut: 1n,
          }),
        });
      }
      return create(GetArchiveJobResponseSchema, {
        job: create(ArchiveJobSchema, {
          jobId: req.jobId,
          kind: ArchiveJobKind.EXTRACT,
          state: ArchiveJobState.DONE,
          path: "/var/www/bundle.zip",
          unpackedTo: "/var/www/bundle",
          entries: 4n,
          skippedLinks: 2n,
        }),
      });
    });
    const extractArchive = vi.fn((req: { path: string }) =>
      create(ExtractArchiveResponseSchema, {
        job: create(ArchiveJobSchema, {
          jobId: "job_ext_1",
          kind: ArchiveJobKind.EXTRACT,
          state: ArchiveJobState.DONE,
          path: req.path,
          unpackedTo: "/var/www/bundle",
          entries: 4n,
          skippedLinks: 2n,
        }),
      }),
    );
    await renderFiles({
      listFiles: () => site,
      listArchiveJobs: () => create(ListArchiveJobsResponseSchema, { jobs: [] }),
      compressFiles: compressFiles as never,
      extractArchive: extractArchive as never,
      getArchiveJob: getArchiveJob as never,
    });

    // Right-click alpha.txt -> Compress
    fireEvent.contextMenu(screen.getByText("alpha.txt"));
    const menu = await screen.findByRole("menu", { name: "Actions for alpha.txt" });
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Compress" }));
    const compSheet = await screen.findByRole("dialog", { name: "Compress alpha.txt" });
    await userEvent.click(within(compSheet).getByRole("button", { name: "Compress" }));
    await waitFor(() => expect(compressFiles).toHaveBeenCalledTimes(1));
    expect(compressFiles.mock.lastCall?.[0]).toMatchObject({
      serverId: "srv_1",
      paths: ["/var/www/alpha.txt"],
      archivePath: "/var/www/alpha.txt.zip",
    });
    expect(await screen.findByText("Compressing alpha.txt.zip")).toBeVisible();
    expect(await screen.findByText("Compressed alpha.txt.zip", {}, { timeout: 3000 })).toBeVisible();
    expect(screen.getByText(/1 item was left out\./)).toBeVisible();
    expect(compPolls).toBeGreaterThanOrEqual(1);

    // Right-click bundle.zip -> Extract
    fireEvent.contextMenu(screen.getByText("bundle.zip"));
    const zipMenu = await screen.findByRole("menu", { name: "Actions for bundle.zip" });
    await userEvent.click(within(zipMenu).getByRole("menuitem", { name: "Extract" }));
    await waitFor(() => expect(extractArchive).toHaveBeenCalledTimes(1));
    expect(extractArchive.mock.lastCall?.[0]).toMatchObject({
      serverId: "srv_1",
      path: "/var/www/bundle.zip",
    });
    expect(await screen.findByText("Extracted bundle.zip")).toBeVisible();
    expect(screen.getByText(/2 links weren't made\./)).toBeVisible();
  });

  it("offers Compress instead of a broken multi-download link when URL length cap is exceeded", async () => {
    const longEntries = Array.from({ length: 120 }, (_, i) =>
      entry(`very-long-filename-to-exceed-the-query-string-cap-segment-${String(i).padStart(4, "0")}.txt`),
    );
    const crowded = create(ListFilesResponseSchema, {
      path: "/var/www",
      directory: entry("www", { type: FileType.DIRECTORY }),
      entries: longEntries,
    });
    await renderFiles({ listFiles: () => crowded });
    await userEvent.click(screen.getByRole("checkbox", { name: "Select all" }));
    const bar = await screen.findByRole("toolbar", { name: "Selection actions" });
    const dlBtn = within(bar).getByRole("button", { name: "Download" });
    await userEvent.click(dlBtn);
    expect(await screen.findByText("Too many items to download at once")).toBeVisible();
  });

  it("sorts columns by clicking headers and resizes the folder tree with keyboard", async () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query.includes("min-width: 860px"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    }));
    await renderFiles({ listFiles: () => site });
    const sizeHeader = screen.getByRole("columnheader", { name: "Size" });
    await userEvent.click(within(sizeHeader).getByRole("button", { name: "Size" }));
    expect(sizeHeader).toHaveAttribute("aria-sort", "ascending");
    const names = () =>
      screen
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.querySelector(".files-entry-name")?.textContent);
    // Folders stay first, then files ordered ascending by size: beta (50), icon (180), alpha (500), bundle (2048).
    expect(names()).toEqual(["assets", "beta.txt", "icon.svg", "alpha.txt", "bundle.zip"]);

    const splitter = screen.getByRole("separator", { name: "Resize folder tree" });
    splitter.focus();
    const before = Number(splitter.getAttribute("aria-valuenow"));
    await userEvent.keyboard("{ArrowRight}");
    expect(Number(splitter.getAttribute("aria-valuenow"))).toBe(before + 16);
  });

  it("opens context menu on empty list space and on a tree folder", async () => {
    const folders: Record<string, ReturnType<typeof create<typeof ListFilesResponseSchema>>> = {
      "/": create(ListFilesResponseSchema, {
        path: "/",
        directory: entry("/", { type: FileType.DIRECTORY }),
        entries: [entry("var", { type: FileType.DIRECTORY })],
      }),
      "/var": create(ListFilesResponseSchema, {
        path: "/var",
        directory: entry("var", { type: FileType.DIRECTORY }),
        entries: [entry("www", { type: FileType.DIRECTORY })],
      }),
      "/var/www": site,
    };
    await renderFiles({ listFiles: (req: { path: string }) => folders[req.path] ?? site });
    const tableWrap = screen.getByRole("table", { name: "Files in /var/www" }).parentElement as HTMLElement;
    fireEvent.contextMenu(tableWrap);
    const folderMenu = await screen.findByRole("menu", { name: "Folder actions" });
    expect(within(folderMenu).getByRole("menuitem", { name: "New folder" })).toBeVisible();
    expect(within(folderMenu).getByRole("menuitem", { name: "Refresh" })).toBeVisible();
    await userEvent.keyboard("{Escape}");

    const treeLink = await within(screen.getByRole("navigation", { name: "Places" })).findByRole("link", {
      name: "www",
    });
    fireEvent.contextMenu(treeLink);
    const treeMenu = await screen.findByRole("menu", { name: "Actions for www" });
    expect(within(treeMenu).getByRole("menuitem", { name: "Download as a .zip" })).toBeVisible();
    expect(within(treeMenu).getByRole("menuitem", { name: "Delete" })).toBeVisible();
  });

  it("supports multiple editor tabs with unsaved dots, word wrap, find/replace, and full-window mode", async () => {
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    const saveFile = vi.fn((req: { path: string }) =>
      create(SaveFileResponseSchema, { path: req.path, file: entry("alpha.txt"), version: "v2" }),
    );
    const readFile = vi.fn((req: { path: string }) =>
      create(ReadFileResponseSchema, {
        path: req.path,
        file: entry(req.path.split("/").pop() ?? "file"),
        content: req.path.endsWith("alpha.txt") ? "alpha original\n" : "beta original\n",
        version: "v1",
      }),
    );
    await renderFiles({ listFiles: () => site, readFile: readFile as never, saveFile: saveFile as never });

    // Open alpha.txt and edit it.
    await userEvent.dblClick(screen.getByText("alpha.txt"));
    const alphaBox = await screen.findByRole("textbox", { name: "Contents of alpha.txt" });
    const alphaView = EditorView.findFromDOM(alphaBox);
    expect(alphaView).toBeTruthy();
    act(() => {
      alphaView?.dispatch({ changes: { from: 0, to: alphaView.state.doc.length, insert: "alpha changed\n" } });
    });

    // Open beta.txt as a second tab: both tabs show in tablist, and alpha keeps its unsaved dot.
    await userEvent.dblClick(screen.getByText("beta.txt"));
    await screen.findByRole("textbox", { name: "Contents of beta.txt" });
    const tablist = await screen.findByRole("tablist", { name: "Open files" });
    const alphaTab = within(tablist).getByRole("tab", { name: /alpha\.txt/ });
    expect(within(alphaTab).getByText("Unsaved changes")).toBeInTheDocument();

    // Closing dirty alpha tab asks first; cancelling keeps it open.
    await userEvent.click(within(tablist).getByRole("button", { name: "Close tab alpha.txt" }));
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(within(tablist).getByRole("tab", { name: /alpha\.txt/ })).toBeVisible();

    // Word wrap toggle persists.
    const wrapBtn = screen.getByRole("button", { name: "Word wrap" });
    expect(wrapBtn).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(wrapBtn);
    expect(wrapBtn).toHaveAttribute("aria-pressed", "true");
    const wrapKey = Object.keys(window.localStorage).find((k) => k.startsWith("dockwell:files:word-wrap:"));
    expect(wrapKey && window.localStorage.getItem(wrapKey)).toBe("1");

    // Find & replace opens the Dockwell-styled search panel with sentence-case phrases.
    await userEvent.click(screen.getByRole("button", { name: "Find and replace" }));
    const searchPanel = document.querySelector(".cm-search") as HTMLElement;
    expect(searchPanel).toBeTruthy();
    expect(within(searchPanel).getByRole("button", { name: "Match case" })).toBeVisible();
    expect(within(searchPanel).getByRole("button", { name: "Next match" })).toBeVisible();
    expect(within(searchPanel).getByRole("button", { name: "Select all" })).toBeVisible();
    expect(within(searchPanel).getByRole("button", { name: "Replace all" })).toBeVisible();

    // Full-window toggle expands editor with a primary Save button (disabled on clean beta.txt, enabled on dirty alpha.txt).
    await userEvent.click(screen.getByRole("button", { name: "Full window editor" }));
    expect(document.querySelector(".files-viewer.is-fullwindow")).toBeTruthy();
    const headerSave = screen.getByRole("button", { name: /^Save/ });
    expect(headerSave).toBeDisabled();
    await userEvent.click(alphaTab);
    expect(screen.getByRole("button", { name: /^Save/ })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Exit full window" }));
    expect(document.querySelector(".files-viewer.is-fullwindow")).toBeNull();

    // Switching back to alpha.txt preserves its unsaved edits.
    expect(
      EditorView.findFromDOM(screen.getByRole("textbox", { name: "Contents of alpha.txt" }))?.state.doc.toString(),
    ).toBe("alpha changed\n");
  });
});
