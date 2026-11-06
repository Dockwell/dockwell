// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  FileEntrySchema,
  FileType,
  ListFilesResponseSchema,
  ReadFileResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

type EntryInit = MessageInitShape<typeof FileEntrySchema>;

function entry(name: string, more: EntryInit = {}) {
  return create(FileEntrySchema, {
    name,
    type: FileType.FILE,
    sizeBytes: 271n,
    mode: "-rw-r--r--",
    owner: "www-data",
    group: "www-data",
    modifyTime: timestampFromDate(new Date(2026, 9, 13)),
    ...more,
  });
}

const folderEntry = (name: string, more: EntryInit = {}) =>
  entry(name, { type: FileType.DIRECTORY, mode: "drwxr-xr-x", ...more });

const site = create(ListFilesResponseSchema, {
  path: "/var/www",
  directory: folderEntry("www"),
  entries: [
    entry(".user.ini"),
    entry("index.php", { sizeBytes: 405n }),
    folderEntry("wp-admin"),
    entry("current", { type: FileType.SYMLINK, linkType: FileType.DIRECTORY, linkTarget: "releases/42" }),
    folderEntry("cache", { refused: true }),
  ],
});

const text = (content: string, more = {}) =>
  create(ReadFileResponseSchema, { path: "/var/www/.user.ini", file: entry(".user.ini"), content, ...more });

const refused = (reason: string, message: string, code = Code.PermissionDenied) =>
  new ConnectError(message, code, new Headers({ "Dockwell-Error": reason }));

async function renderFiles(path: string, files: NonNullable<Api["files"]>) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const rendered = renderAt(path, { servers: { watchServers: watch.stream }, files });
  await screen.findByRole("heading", { level: 1 });
  return rendered;
}

const rowNames = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => row.querySelector(".files-entry-name")?.textContent);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("FilesPage", () => {
  it("is a tab of the server's page", async () => {
    const listFiles = vi.fn(() => site);
    await renderFiles("/servers/srv_1", { listFiles });
    await userEvent.click(await screen.findByRole("link", { name: "Files" }));
    await waitFor(() => expect(listFiles).toHaveBeenCalled());
    expect(listFiles.mock.lastCall).toEqual([
      expect.objectContaining({ serverId: "srv_1", path: "/" }),
      expect.anything(),
    ]);
    expect(await screen.findByRole("link", { name: "Files" })).toHaveAttribute("aria-current", "page");
  });

  it("lists a folder, folders first, and says where you are", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles: () => site });
    await screen.findByRole("table", { name: "Files in /var/www" });
    await screen.findByText("wp-admin");
    expect(rowNames()).toEqual(["cache", "current", "wp-admin", ".user.ini", "index.php"]);
    const path = screen.getByRole("navigation", { name: "Path" });
    expect(within(path).getByRole("link", { name: "var" })).toBeVisible();
    expect(within(path).getByText("www")).toHaveAttribute("aria-current", "page");
    expect(screen.getByText("5 items", { exact: false })).toHaveTextContent("5 items · owned by www-data");
    expect(screen.getByText("→ releases/42")).toBeVisible();
    expect(screen.getByText("405 B")).toBeVisible();
    expect(screen.getAllByText("Dockwell doesn't open this")).toHaveLength(1);
    await expectAccessible(document.body);
  });

  it("opens a folder, and follows a link to one", async () => {
    const listFiles = vi.fn(() => site);
    const { router } = await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles });
    await userEvent.click(await screen.findByText("wp-admin"));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www/wp-admin" }));
    await userEvent.click(await screen.findByText("current"));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www/current" }));
  });

  it("reads a file once, as a read goes on record, with its lines numbered", async () => {
    const readFile = vi.fn(() => text("; PHP settings\nmemory_limit = 256M\n"));
    await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles: () => site, readFile });
    await userEvent.click(await screen.findByText(".user.ini"));
    const code = await screen.findByRole("region", { name: "Contents of .user.ini" });
    expect(code.querySelector(".files-text")).toHaveTextContent("memory_limit = 256M");
    expect(code.querySelector(".files-gutter")?.textContent).toBe("1\n2");
    expect(screen.getByText("2 lines")).toBeVisible();
    expect(screen.getByText("LF")).toBeVisible();
    expect(readFile).toHaveBeenCalledTimes(1);
    expect(readFile.mock.lastCall).toEqual([
      expect.objectContaining({ serverId: "srv_1", path: "/var/www/.user.ini" }),
      expect.anything(),
    ]);
    window.dispatchEvent(new Event("focus"));
    expect(readFile).toHaveBeenCalledTimes(1);
    await expectAccessible(document.body);
  });

  it("shows a file's text as text, whatever it says", async () => {
    const markup = "<script>window.ran = true</script><img src=x onerror=alert(1)>";
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/index.html", {
      listFiles: () => site,
      readFile: () => text(markup),
    });
    const code = await screen.findByRole("region", { name: "Contents of index.html" });
    expect(code.querySelector(".files-text")?.textContent).toBe(markup);
    expect(code.querySelector("script, img")).toBeNull();
  });

  it("closes a file", async () => {
    const { router } = await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini", {
      listFiles: () => site,
      readFile: () => text("x"),
    });
    await userEvent.click(await screen.findByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
    expect(screen.queryByRole("region", { name: "Contents of .user.ini" })).toBeNull();
  });

  it("says when a file isn't text, and sends nothing to show", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/logo.png", {
      listFiles: () => site,
      readFile: () => text("", { binary: true, file: entry("logo.png", { sizeBytes: 2048n }) }),
    });
    expect(await screen.findByText("This isn't a text file")).toBeVisible();
    expect(screen.getByText("It's 2 KB", { exact: false })).toBeVisible();
    expect(screen.queryByRole("region", { name: "Contents of logo.png" })).toBeNull();
  });

  it("says when a file is larger than it shows", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/log&file=/var/log/huge.log", {
      listFiles: () => site,
      readFile: () => text("line\n", { truncated: true, file: entry("huge.log", { sizeBytes: 2n * 1024n ** 3n }) }),
    });
    expect(await screen.findByText("Showing the first 1 MB")).toBeVisible();
    expect(screen.getByText("This file is 2 GB", { exact: false })).toBeVisible();
    expect(screen.getByRole("region", { name: "Contents of huge.log" })).toBeVisible();
  });

  it("says why a file is refused, in the agent's words", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/key", {
      listFiles: () => site,
      readFile: () => {
        throw refused("file_refused", "/var/lib/dockwell is Dockwell's own data, so Dockwell doesn't open it");
      },
    });
    expect(await screen.findByText("Dockwell doesn't open this file")).toBeVisible();
    expect(screen.getByText("/var/lib/dockwell is Dockwell's own data", { exact: false })).toBeVisible();
    // The folder stays, to go on from.
    expect(screen.getByText("wp-admin")).toBeVisible();
  });

  it("says why a folder can't be read, and offers a way up", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/nope", {
      listFiles: () => {
        throw refused("not_found", "there's nothing at /var/nope on this server", Code.NotFound);
      },
    });
    expect(await screen.findByText("Nothing at this path")).toBeVisible();
    expect(screen.getByText("There's nothing at /var/nope on this server", { exact: false })).toBeVisible();
    expect(screen.getByRole("link", { name: "Go up to /var" })).toHaveAttribute(
      "href",
      "/servers/srv_1/files?path=%2Fvar",
    );
  });

  it("says when the server's agent is away", async () => {
    await renderFiles("/servers/srv_1/files?path=/etc", {
      listFiles: () => {
        throw refused(
          "server_offline",
          "hz-1's agent isn't connected, so Dockwell can't reach its files",
          Code.Unavailable,
        );
      },
    });
    expect(await screen.findByText("Can't reach this server")).toBeVisible();
    expect(screen.getByText("hz-1's agent isn't connected", { exact: false })).toBeVisible();
  });

  it("searches what the folder holds, and says when nothing matches", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles: () => site });
    const search = await screen.findByRole("textbox", { name: "Search in www" });
    await screen.findByText("wp-admin");
    await userEvent.type(search, "INDEX");
    expect(rowNames()).toEqual(["index.php"]);
    await userEvent.clear(search);
    await userEvent.type(search, "nothing");
    expect(screen.getByText("Nothing here matches your search.")).toBeVisible();
  });

  it("says when a folder holds more than a listing shows", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www", {
      listFiles: () => create(ListFilesResponseSchema, { ...site, truncated: true }),
    });
    expect(await screen.findByText("This folder holds more than a listing shows")).toBeVisible();
  });

  it("doesn't open a file whose name it can't send back", async () => {
    const readFile = vi.fn(() => text("x"));
    const { router } = await renderFiles("/servers/srv_1/files?path=/var/www", {
      listFiles: () =>
        create(ListFilesResponseSchema, { path: "/var/www", entries: [entry("caf�.txt", { unreadableName: true })] }),
      readFile,
    });
    await userEvent.click(await screen.findByText("caf�.txt"));
    expect(router.state.location.search).toEqual({ path: "/var/www" });
    expect(readFile).not.toHaveBeenCalled();
  });

  it("goes to where a symlink led, whatever the URL says", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/run-link", { listFiles: () => site });
    const path = await screen.findByRole("navigation", { name: "Path" });
    expect(await within(path).findByText("www")).toHaveAttribute("aria-current", "page");
  });
});

describe("FilesPage on a phone", () => {
  it("shows what a folder holds, with only what fits", async () => {
    onPhone();
    await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles: () => site });
    await screen.findByText("wp-admin");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Name", "Size"]);
    expect(screen.getByRole("navigation", { name: "Places" })).toBeVisible();
  });

  it("gives an open file the whole screen, with a way back to its folder", async () => {
    onPhone();
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini", {
      listFiles: () => site,
      readFile: () => text("memory_limit = 256M\n"),
    });
    expect(await screen.findByRole("heading", { level: 1, name: ".user.ini" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "Contents of .user.ini" })).toBeVisible();
    expect(screen.getByText("/var/www")).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Server hz-1" })).toBeNull();
    expect(screen.getByRole("link", { name: "Back to /var/www" })).toHaveAttribute(
      "href",
      "/servers/srv_1/files?path=%2Fvar%2Fwww",
    );
    await expectAccessible(document.body);
  });
});
