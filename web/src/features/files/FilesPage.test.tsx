// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  FileEntrySchema,
  FileType,
  ListFilesResponseSchema,
  ReadFileResponseSchema,
  SaveFileResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { PHONE_QUERY } from "@dockwell/ui";
import { act, screen, waitFor, within } from "@testing-library/react";
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
  create(ReadFileResponseSchema, {
    path: "/var/www/.user.ini",
    file: entry(".user.ini"),
    content,
    version: "v1",
    ...more,
  });

const saved = (version = "v2") =>
  create(SaveFileResponseSchema, {
    path: "/var/www/.user.ini",
    file: entry(".user.ini", { sizeBytes: 300n }),
    version,
  });

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
    const code = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(code).toHaveValue("; PHP settings\nmemory_limit = 256M\n");
    expect(document.querySelector(".dw-code-numbers")?.textContent).toBe("1\n2\n3");
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
    const code = await screen.findByRole("textbox", { name: "Contents of index.html" });
    expect(code).toHaveValue(markup);
    expect(document.querySelector(".files-viewer script, .files-viewer img")).toBeNull();
  });

  it("closes a file", async () => {
    const { router } = await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini", {
      listFiles: () => site,
      readFile: () => text("x"),
    });
    await userEvent.click(await screen.findByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
    expect(screen.queryByRole("textbox", { name: "Contents of .user.ini" })).toBeNull();
  });

  it("says when a file isn't text, and sends nothing to show", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/logo.png", {
      listFiles: () => site,
      readFile: () => text("", { binary: true, file: entry("logo.png", { sizeBytes: 2048n }) }),
    });
    expect(await screen.findByText("This isn't a text file")).toBeVisible();
    expect(screen.getByText("It's 2 KB", { exact: false })).toBeVisible();
    expect(screen.queryByRole("textbox", { name: "Contents of logo.png" })).toBeNull();
  });

  it("says when a file is larger than it shows", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/log&file=/var/log/huge.log", {
      listFiles: () => site,
      readFile: () => text("line\n", { truncated: true, file: entry("huge.log", { sizeBytes: 2n * 1024n ** 3n }) }),
    });
    expect(await screen.findByText("Showing the first 1 MB")).toBeVisible();
    expect(screen.getByText("This file is 2 GB", { exact: false })).toBeVisible();
    const code = screen.getByRole("textbox", { name: "Contents of huge.log" });
    expect(code).toHaveAttribute("readonly");
    expect(screen.getByText("it can't be edited here", { exact: false })).toBeVisible();
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

  it("says when a path isn't answering, and doesn't try it again by itself", async () => {
    const listFiles = vi.fn(() => {
      throw refused(
        "path_not_answering",
        "/mnt/nas isn't answering, so Dockwell stopped waiting for it",
        Code.DeadlineExceeded,
      );
    });
    await renderFiles("/servers/srv_1/files?path=/mnt/nas", { listFiles });
    expect(await screen.findByText("This path isn't answering")).toBeVisible();
    expect(screen.getByText("/mnt/nas isn't answering", { exact: false })).toBeVisible();
    expect(listFiles).toHaveBeenCalledTimes(1);
  });

  it("says when a file isn't answering", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/key", {
      listFiles: () => site,
      readFile: () => {
        throw refused("path_not_answering", "/var/www/key isn't answering", Code.DeadlineExceeded);
      },
    });
    expect(await screen.findByText("This path isn't answering")).toBeVisible();
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

describe("editing a file", () => {
  const open = (files: NonNullable<Api["files"]>) =>
    renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini", { listFiles: () => site, ...files });

  async function edited(files: NonNullable<Api["files"]>) {
    const rendered = await open(files);
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    await userEvent.clear(field);
    await userEvent.type(field, "memory_limit = 512M");
    return { ...rendered, field };
  }

  const saveButton = () => screen.getByRole("button", { name: "Save" });

  it("offers a save only once something changed, and says it can't be undone", async () => {
    await open({ readFile: () => text("a\nb\n") });
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    await userEvent.type(field, "c");
    expect(screen.getByText("1 line changed")).toBeVisible();
    expect(screen.getByText("Saving replaces the file at once, and can't be undone.")).toBeVisible();
    expect(screen.getByText("Edited")).toBeVisible();
    await expectAccessible(document.body);
  });

  it("saves what you typed over the version you read, and goes on from what it gave", async () => {
    const saveFile = vi.fn(() => saved("v2"));
    const { field } = await edited({ readFile: () => text("old\n"), saveFile });
    await userEvent.click(saveButton());
    await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(1));
    expect(saveFile.mock.lastCall).toEqual([
      expect.objectContaining({
        serverId: "srv_1",
        path: "/var/www/.user.ini",
        content: "memory_limit = 512M",
        version: "v1",
      }),
      expect.anything(),
    ]);
    expect(await screen.findByText("Saved .user.ini", { selector: "p" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    await userEvent.type(field, "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
    expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ version: "v2" }), expect.anything()]);
  });

  it("saves with the keyboard", async () => {
    const saveFile = vi.fn(() => saved());
    await edited({ readFile: () => text("old\n"), saveFile });
    await userEvent.keyboard("{Control>}s{/Control}");
    await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(1));
  });

  it("sends a file's line endings back as they came", async () => {
    const saveFile = vi.fn(() => saved());
    await open({ readFile: () => text("a\r\nb\r\n"), saveFile });
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(screen.getByText("CRLF")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    await userEvent.type(field, "c");
    await userEvent.click(saveButton());
    await waitFor(() => expect(saveFile).toHaveBeenCalled());
    expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ content: "a\r\nb\r\nc" }), expect.anything()]);
  });

  it("discards what you typed on your word", async () => {
    const { field } = await edited({ readFile: () => text("old\n") });
    await userEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(field).toHaveValue("old\n");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  describe("when the file changed on the server", () => {
    const changed = () => {
      throw refused("file_changed", "/var/www/.user.ini changed since you opened it", Code.FailedPrecondition);
    };

    it("keeps what you typed, and says so", async () => {
      const { field } = await edited({ readFile: () => text("old\n"), saveFile: changed });
      await userEvent.click(saveButton());
      expect(await screen.findByText("This file changed on the server")).toBeVisible();
      expect(screen.getByText("What you typed is still here", { exact: false })).toBeVisible();
      expect(field).toHaveValue("memory_limit = 512M");
      expect(saveButton()).toBeDisabled();
    });

    it("saves over it once you say so, over what's there now", async () => {
      const saveFile = vi.fn().mockImplementationOnce(changed).mockReturnValue(saved("v4"));
      const readFile = vi
        .fn()
        .mockReturnValueOnce(text("old\n"))
        .mockReturnValue(text("theirs\n", { version: "v3" }));
      const { field } = await edited({ readFile, saveFile });
      await userEvent.click(saveButton());
      await userEvent.click(await screen.findByRole("button", { name: "Save over it" }));
      await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
      expect(saveFile.mock.lastCall).toEqual([
        expect.objectContaining({ content: "memory_limit = 512M", version: "v3" }),
        expect.anything(),
      ]);
      expect(await screen.findByText("Saved .user.ini", { selector: "p" })).toBeVisible();
      expect(screen.queryByText("This file changed on the server")).toBeNull();
      expect(field).toHaveValue("memory_limit = 512M");
    });

    it("loads the server's version in place of yours, once you say so", async () => {
      const readFile = vi
        .fn()
        .mockReturnValueOnce(text("old\n"))
        .mockReturnValue(text("theirs\n", { version: "v3" }));
      const saveFile = vi.fn().mockImplementationOnce(changed).mockReturnValue(saved("v4"));
      const { field } = await edited({ readFile, saveFile });
      await userEvent.click(saveButton());
      await userEvent.click(await screen.findByRole("button", { name: "Load the server's version" }));
      await waitFor(() => expect(field).toHaveValue("theirs\n"));
      expect(screen.queryByText("This file changed on the server")).toBeNull();
      await userEvent.type(field, "x");
      await userEvent.click(saveButton());
      await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
      expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ version: "v3" }), expect.anything()]);
    });

    it("drops the notice with what you typed, if you discard it", async () => {
      const { field } = await edited({ readFile: () => text("old\n"), saveFile: changed });
      await userEvent.click(saveButton());
      await screen.findByText("This file changed on the server");
      await userEvent.click(screen.getByRole("button", { name: "Discard" }));
      expect(field).toHaveValue("old\n");
      expect(screen.queryByText("This file changed on the server")).toBeNull();
    });

    it("won't save over a file that isn't text any more", async () => {
      const readFile = vi
        .fn()
        .mockReturnValueOnce(text("old\n"))
        .mockReturnValue(text("", { binary: true, version: "" }));
      const saveFile = vi.fn(changed);
      const { field } = await edited({ readFile, saveFile });
      await userEvent.click(saveButton());
      await userEvent.click(await screen.findByRole("button", { name: "Save over it" }));
      expect(await screen.findByText("Can't save this file")).toBeVisible();
      expect(saveFile).toHaveBeenCalledTimes(1);
      expect(field).toHaveValue("memory_limit = 512M");
    });
  });

  it("keeps what you typed through any other failure, to try again", async () => {
    const saveFile = vi
      .fn()
      .mockImplementationOnce(() => {
        throw refused(
          "server_offline",
          "hz-1's agent isn't connected, so Dockwell can't reach its files",
          Code.Unavailable,
        );
      })
      .mockReturnValue(saved());
    const { field } = await edited({ readFile: () => text("old\n"), saveFile });
    await userEvent.click(saveButton());
    expect(await screen.findByText("Can't save this file")).toBeVisible();
    expect(screen.getByText("hz-1's agent isn't connected", { exact: false })).toBeVisible();
    expect(field).toHaveValue("memory_limit = 512M");
    await userEvent.click(saveButton());
    expect(await screen.findByText("Saved .user.ini", { selector: "p" })).toBeVisible();
  });

  it("asks before leaving what you typed", async () => {
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    const { router, field } = await edited({ readFile: () => text("old\n") });
    await userEvent.click(screen.getByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(router.state.location.search).toMatchObject({ file: "/var/www/.user.ini" });
    expect(field).toHaveValue("memory_limit = 512M");
    confirm.mockReturnValue(true);
    await userEvent.click(screen.getByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
  });

  it("lets you leave what you saved", async () => {
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    const { router } = await edited({ readFile: () => text("old\n"), saveFile: () => saved() });
    await userEvent.click(saveButton());
    await screen.findByText("Saved .user.ini", { selector: "p" });
    await userEvent.click(screen.getByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("asks the folder again after a save, for its new size", async () => {
    const listFiles = vi.fn(() => site);
    const { field } = await edited({ listFiles, readFile: () => text("old\n"), saveFile: () => saved() });
    await waitFor(() => expect(listFiles).toHaveBeenCalled());
    const before = listFiles.mock.calls.length;
    await userEvent.click(saveButton());
    await screen.findByText("Saved .user.ini", { selector: "p" });
    await waitFor(() => expect(listFiles.mock.calls.length).toBeGreaterThan(before));
    expect(field).toBeVisible();
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
    expect(await screen.findByRole("textbox", { name: "Contents of .user.ini" })).toBeVisible();
    expect(screen.getByText("/var/www")).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Server hz-1" })).toBeNull();
    expect(screen.getByRole("link", { name: "Back to /var/www" })).toHaveAttribute(
      "href",
      "/servers/srv_1/files?path=%2Fvar%2Fwww",
    );
    await expectAccessible(document.body);
  });

  it("keeps what you typed as the phone turns", async () => {
    let matches = true;
    const listeners = new Set<() => void>();
    vi.stubGlobal("matchMedia", (query: string) => ({
      get matches() {
        return query === PHONE_QUERY && matches;
      },
      media: query,
      addEventListener: (_: string, fn: () => void) => listeners.add(fn),
      removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
    }));
    await renderFiles("/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini", {
      listFiles: () => site,
      readFile: () => text("old\n"),
    });
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    await userEvent.type(field, "typed");
    matches = false;
    act(() => {
      for (const fn of listeners) fn();
    });
    expect(await screen.findByRole("table")).toBeVisible();
    expect(screen.getByRole("textbox", { name: "Contents of .user.ini" })).toHaveValue("old\ntyped");
  });
});
