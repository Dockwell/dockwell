// SPDX-License-Identifier: AGPL-3.0-only

import { create, fromBinary, type MessageInitShape, toBinary } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  FileEntrySchema,
  FileType,
  ListFilesResponseSchema,
  ReadFileResponseSchema,
  Role,
  SaveFileResponseSchema,
  StepUpAction,
  UserSchema,
} from "@dockwell/api";
import { PHONE_QUERY } from "@dockwell/ui";
import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
import { type Api, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { dropDrafts } from "./drafts";

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

async function renderFiles(path: string, files: NonNullable<Api["files"]>, auth?: Api["auth"]) {
  // As the control plane does, for each page that opens it, however many come.
  async function* watchServers(_req: unknown, ctx: HandlerContext) {
    yield changed(hz1);
    yield snapshotComplete;
    await untilAborted(ctx.signal);
  }
  const rendered = renderAt(path, { servers: { watchServers }, files, auth });
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
  focusManager.setFocused(undefined);
  dropDrafts();
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

  it("groups its places as Server-Files does: the common ones, then the server's folders, with yours marked", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/www/site", { listFiles: () => site });
    const places = await screen.findByRole("navigation", { name: "Places" });
    const common = within(places).getByRole("group", { name: "Common places" });
    expect(
      within(common)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual(["Websites", "Logs", "Docker volumes"]);
    const server = within(places).getByRole("group", { name: "Server" });
    // The server's own folders are a tree, from its root.
    expect(within(server).getAllByRole("link")[0]).toHaveTextContent(/^\/$/);
    // A folder inside a place is in that place, not in the root that holds them all.
    expect(within(common).getByRole("link", { name: "Websites" })).toHaveAttribute("aria-current", "location");
    expect(within(server).getAllByRole("link")[0]).not.toHaveAttribute("aria-current");
    expect(places.querySelector(".files-places-rule")).toHaveAttribute("aria-hidden", "true");
  });

  it("shows the server's folders as a tree, open down to where you are, and opens another in place", async () => {
    const at = (path: string, ...names: EntryInit[]) =>
      create(ListFilesResponseSchema, {
        path,
        directory: folderEntry(path),
        entries: names.map((more) => folderEntry(String(more.name), more)),
      });
    const folders: Record<string, ReturnType<typeof at>> = {
      "/": at("/", { name: "etc" }, { name: "var" }),
      "/var": at("/var", { name: "lib", refused: true }, { name: "www" }),
      "/var/www": create(ListFilesResponseSchema, {
        path: "/var/www",
        directory: folderEntry("www"),
        entries: [entry("index.php"), folderEntry("blog")],
      }),
      "/etc": at("/etc", { name: "nginx" }),
    };
    const listFiles = vi.fn((req: { path: string; lookup: boolean }) => folders[req.path] ?? at(req.path));
    await renderFiles("/servers/srv_1/files?path=/var/www", { listFiles });
    const server = within(await screen.findByRole("navigation", { name: "Places" })).getByRole("group", {
      name: "Server",
    });
    // Folders only, each level under the one that holds it; what's beside the way stays shut.
    await within(server).findByRole("link", { name: "blog" });
    expect(
      within(server)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual(["/", "etc", "var", "lib", "www", "blog"]);
    expect(within(server).getByRole("link", { name: "www" })).toHaveAttribute("aria-current", "page");
    expect(within(server).getByRole("link", { name: "www" })).toHaveAttribute(
      "href",
      "/servers/srv_1/files?path=%2Fvar%2Fwww",
    );
    // The folder you're in is the page's own listing. The ones above are lookups, which go on no Activity.
    const asked = () => listFiles.mock.calls.map(([req]) => `${req.path}${req.lookup ? " lookup" : ""}`).sort();
    expect(asked()).toEqual(["/ lookup", "/var lookup", "/var/www"]);
    // What Dockwell doesn't open has nothing to show.
    expect(within(server).queryByRole("button", { name: "Show the folders in lib" })).toBeNull();

    await userEvent.click(within(server).getByRole("button", { name: "Show the folders in etc" }));
    expect(await within(server).findByRole("link", { name: "nginx" })).toBeVisible();
    expect(within(server).getByRole("button", { name: "Hide the folders in etc" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    // And the way to where you are closes too, without leaving the folder.
    await userEvent.click(within(server).getByRole("button", { name: "Hide the folders in var" }));
    expect(within(server).queryByRole("link", { name: "www" })).toBeNull();
    expect(screen.getByRole("table", { name: "Files in /var/www" })).toBeVisible();
  });

  it("keeps the way to where you are in the tree, past what one folder shows of its own", async () => {
    const many = Array.from({ length: 70 }, (_, i) => folderEntry(`pkg-${String(i).padStart(2, "0")}`));
    const listFiles = (req: { path: string }) =>
      create(ListFilesResponseSchema, {
        path: req.path,
        directory: folderEntry(req.path),
        entries: req.path === "/" ? many : [],
      });
    await renderFiles("/servers/srv_1/files?path=/pkg-68", { listFiles });
    const server = within(await screen.findByRole("navigation", { name: "Places" })).getByRole("group", {
      name: "Server",
    });
    expect(await within(server).findByRole("link", { name: "pkg-68" })).toHaveAttribute("aria-current", "page");
    expect(within(server).queryByRole("link", { name: "pkg-69" })).toBeNull();
    expect(within(server).getByText("and 9 more, in its list")).toBeVisible();
  });

  it("marks the place you're on as yours, and only it", async () => {
    await renderFiles("/servers/srv_1/files?path=/var/log", { listFiles: () => site });
    const places = await screen.findByRole("navigation", { name: "Places" });
    // On the place itself the link is the page you're on, as the router says.
    expect(within(places).getByRole("link", { name: "Logs" })).toHaveAttribute("aria-current", "page");
    expect(within(places).getByRole("link", { name: "Logs" })).toHaveClass("is-current");
    expect(places.querySelectorAll("[aria-current]")).toHaveLength(1);
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
    const list = () => screen.findByRole("table");
    await userEvent.click(await within(await list()).findByText("wp-admin"));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www/wp-admin" }));
    await userEvent.click(await within(await list()).findByText("current"));
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
    expect(screen.getByRole("link", { name: "Download huge.log" })).toBeVisible();
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
    // A download would be refused the same way, and a browser would say only "Failed".
    expect(screen.queryByRole("link", { name: "Download key" })).toBeNull();
    // The folder stays, to go on from.
    expect(within(screen.getByRole("table")).getByText("wp-admin")).toBeVisible();
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
    // Once for the path itself: the tree asks for the folders above it, never for it again.
    const asked = (listFiles.mock.calls as unknown as [{ path: string }][]).map(([req]) => req.path);
    expect(asked.filter((path) => path === "/mnt/nas")).toHaveLength(1);
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
  const open = (files: NonNullable<Api["files"]>, auth?: Api["auth"]) =>
    renderFiles(
      "/servers/srv_1/files?path=/var/www&file=/var/www/.user.ini",
      { listFiles: () => site, ...files },
      auth,
    );

  async function edited(files: NonNullable<Api["files"]>, auth?: Api["auth"]) {
    const rendered = await open(files, auth);
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

  it.each([
    ["one stray CRLF", "#!/bin/sh\necho a\r\necho b\n", "#!/bin/sh\necho a\r\necho b\nx"],
    ["a lone CR", "a\rb\n", "a\rb\nx"],
  ])("edits a file with %s, and saves each line with the ending it had", async (_, content, sent) => {
    const saveFile = vi.fn(() => saved());
    await open({ readFile: () => text(content), saveFile });
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(field).not.toHaveAttribute("readonly");
    expect(screen.getByText("Mixed line endings")).toBeVisible();
    await userEvent.type(field, "x");
    await userEvent.click(saveButton());
    await waitFor(() => expect(saveFile).toHaveBeenCalled());
    expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ content: sent }), expect.anything()]);
  });

  it("takes the server's version with mixed line endings in place of yours, and keeps them on the next save", async () => {
    const readFile = vi
      .fn()
      .mockReturnValueOnce(text("old\n"))
      .mockReturnValue(text("a\r\nb\n", { version: "v3" }));
    let calls = 0;
    const saveFile = vi.fn(() => {
      calls++;
      if (calls === 1) throw refused("file_changed", "it changed", Code.FailedPrecondition);
      return saved();
    });
    const { field } = await edited({ readFile, saveFile });
    await userEvent.click(saveButton());
    await userEvent.click(await screen.findByRole("button", { name: "Load the server's version" }));
    await waitFor(() => expect(field).toHaveValue("a\nb\n"));
    expect(screen.getByText("Mixed line endings")).toBeVisible();
    await userEvent.type(field, "c");
    await userEvent.click(saveButton());
    await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
    expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ content: "a\r\nb\nc" }), expect.anything()]);
  });

  it("reads a file from an agent too old to save, and says what to do", async () => {
    const saveFile = vi.fn(() => saved());
    await open({ readFile: () => text("old\n", { version: "" }), saveFile });
    const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(field).toHaveAttribute("readonly");
    expect(screen.getByText("This server's agent can't save files")).toBeVisible();
    expect(screen.getByText("Update the agent on this server", { exact: false })).toBeVisible();
    await userEvent.type(field, "x");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("says a save may still land, not that it failed", async () => {
    const saveFile = vi.fn(() => {
      throw refused(
        "save_may_land",
        "/var/www/.user.ini stopped answering as the agent began to write it, so this save may still land when it answers",
        Code.DeadlineExceeded,
      );
    });
    const { field } = await edited({ readFile: () => text("old\n"), saveFile });
    await userEvent.click(saveButton());
    expect(await screen.findByText("This save may still land")).toBeVisible();
    expect(screen.queryByText("Can't save this file")).toBeNull();
    expect(field).toHaveValue("memory_limit = 512M");
  });

  it("says which file a path leads to, as that's where a save goes", async () => {
    await open({ readFile: () => text("old\n", { path: "/srv/releases/42/.user.ini" }) });
    await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(screen.getByText("/srv/releases/42/.user.ini")).toBeVisible();
    expect(screen.getByText("Leads to", { exact: false })).toBeVisible();
  });

  // protobuf-es decodes text with a TextDecoder, which drops this mark. The panel can't
  // see it, so what keeps it in the file is the agent (TestASaveKeepsTheMarkAFileStartsWith).
  it("doesn't get a file's byte order mark in production's format, but does in development's", () => {
    const sent = text("\uFEFFold\n");
    expect(fromBinary(ReadFileResponseSchema, toBinary(ReadFileResponseSchema, sent)).content).toBe("old\n");
    expect(sent.content.startsWith("\uFEFF")).toBe(true);
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

    it("stops if the path leads somewhere else now, and names it", async () => {
      const keys = "/root/.ssh/authorized_keys";
      const readFile = vi
        .fn()
        .mockReturnValueOnce(text("old\n"))
        .mockReturnValue(text("ssh-ed25519 AAAA admin\n", { path: keys, version: "v9" }));
      const saveFile = vi.fn(changed);
      const { field } = await edited({ readFile, saveFile });
      await userEvent.click(saveButton());
      await userEvent.click(await screen.findByRole("button", { name: "Save over it" }));
      expect(await screen.findByText("This isn't the file you opened")).toBeVisible();
      expect(screen.getByText(keys)).toBeVisible();
      expect(saveFile).toHaveBeenCalledTimes(1);
      expect(field).toHaveValue("memory_limit = 512M");
      expect(screen.queryByRole("button", { name: "Save over it" })).toBeNull();
      expect(saveButton()).toBeDisabled();
      field.focus();
      await userEvent.keyboard("{Control>}s{/Control}");
      expect(saveFile).toHaveBeenCalledTimes(1);
      // With it in front of you, you can go on.
      await userEvent.click(screen.getByRole("button", { name: "Load the server's version" }));
      await waitFor(() => expect(field).toHaveValue("ssh-ed25519 AAAA admin\n"));
      expect(screen.queryByText("This isn't the file you opened")).toBeNull();
      expect(screen.getByText(keys)).toBeVisible();
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

  describe("with a file that's a way back onto the server", () => {
    const keys = "/root/.ssh/authorized_keys";
    const wayIn = () => text("ssh-ed25519 AAAA admin\n", { path: keys, wayBackIn: true });
    const asks = () => {
      throw authError(Code.PermissionDenied, "needs_step_up", `${keys} can let someone back onto this server`);
    };

    it("says it asks, confirms that path on that server, and saves with the proof", async () => {
      const passkeys = stubPasskeys();
      const saveFile = vi.fn((req: { proof: string }) => (req.proof ? saved() : asks()));
      const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
      await edited(
        { readFile: wayIn, saveFile: saveFile as never },
        { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
      );
      expect(screen.getByText("It asks for your passkey", { exact: false })).toBeVisible();
      await userEvent.click(saveButton());
      await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
      expect(saveFile.mock.calls[0]?.[0]).toMatchObject({ proof: "", confirmedPath: keys });
      expect(saveFile.mock.lastCall?.[0]).toMatchObject({ proof: "proof_1", confirmedPath: keys, version: "v1" });
      expect(beginStepUp).toHaveBeenCalledWith(
        expect.objectContaining({ action: StepUpAction.SAVE_WAY_BACK_IN, target: `srv_1:${keys}` }),
        expect.anything(),
      );
      expect(passkeys.get).toHaveBeenCalledTimes(1);
      expect(await screen.findByText("Saved .user.ini", { selector: "p" })).toBeVisible();
    });

    it("points to SSH with no passkey for this address, and keeps what you typed", async () => {
      stubPasskeys();
      const { field } = await edited(
        { readFile: wayIn, saveFile: asks },
        {
          getAuthState: () => authState(),
          beginStepUp: () => {
            throw authError(Code.FailedPrecondition, "no_passkey_here", "you have none here");
          },
        },
      );
      await userEvent.click(saveButton());
      expect(await screen.findByText("Connect to the server over SSH to change it", { exact: false })).toBeVisible();
      expect(screen.getByText(`Saving ${keys} takes a passkey`, { exact: false })).toBeVisible();
      expect(field).toHaveValue("memory_limit = 512M");
    });

    it("says so in words if you close the passkey prompt, and keeps what you typed", async () => {
      stubPasskeys({ get: "NotAllowedError" });
      const { field } = await edited(
        { readFile: wayIn, saveFile: asks },
        {
          getAuthState: () => authState(),
          beginStepUp: () => ({ ceremonyId: "cer_1", optionsJson: requestOptions }),
        },
      );
      await userEvent.click(saveButton());
      expect(
        await screen.findByText("The passkey prompt closed, so nothing was saved. Save again when you're ready.", {
          exact: false,
        }),
      ).toBeVisible();
      expect(field).toHaveValue("memory_limit = 512M");
    });

    it("says nothing of a passkey for any other file", async () => {
      await edited({ readFile: () => text("old\n") });
      expect(screen.queryByText("It asks for your passkey", { exact: false })).toBeNull();
    });
  });

  describe("when a session ends", () => {
    const signedIn = () => {
      let user = authState().user;
      const set = (next: typeof user) => {
        user = next;
      };
      return { auth: { getAuthState: () => authState({ user }) }, set };
    };
    const ended = () => refused("session_ended", "your session ended", Code.Unauthenticated);
    /** What a browser does when someone comes back to the tab, and what signing in comes to. */
    const refocus = () =>
      act(() => {
        focusManager.setFocused(false);
        focusManager.setFocused(true);
      });
    const signIn = () => screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" });

    it("offers what you typed back after you sign in, over the version you read", async () => {
      const session = signedIn();
      const saveFile = vi
        .fn()
        .mockImplementationOnce(() => {
          session.set(undefined);
          throw ended();
        })
        .mockReturnValue(saved());
      await edited({ readFile: () => text("old\n"), saveFile }, session.auth);
      await userEvent.click(saveButton());
      await signIn();

      session.set(authState().user);
      refocus();
      expect(await screen.findByText("You have unsaved changes to .user.ini")).toBeVisible();
      const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
      expect(field).toHaveValue("old\n");
      expect(field).toHaveAttribute("readonly");
      await userEvent.click(screen.getByRole("button", { name: "Bring them back" }));
      expect(field).toHaveValue("memory_limit = 512M");
      expect(screen.getByText("Edited")).toBeVisible();
      await userEvent.click(saveButton());
      await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(2));
      expect(saveFile.mock.lastCall).toEqual([
        expect.objectContaining({ content: "memory_limit = 512M", version: "v1" }),
        expect.anything(),
      ]);
    });

    it("does so when something in the background found it, with no save at all", async () => {
      const session = signedIn();
      await edited({ readFile: () => text("old\n") }, session.auth);
      session.set(undefined);
      refocus();
      await signIn();
      session.set(authState().user);
      refocus();
      expect(await screen.findByText("You have unsaved changes to .user.ini")).toBeVisible();
      await userEvent.click(screen.getByRole("button", { name: "Discard them" }));
      const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
      expect(field).toHaveValue("old\n");
      expect(field).not.toHaveAttribute("readonly");
      expect(screen.queryByText("You have unsaved changes to .user.ini")).toBeNull();
    });

    it("says so if the file changed meanwhile, and a save then asks", async () => {
      const session = signedIn();
      const readFile = vi
        .fn()
        .mockReturnValueOnce(text("old\n"))
        .mockReturnValue(text("theirs\n", { version: "v3" }));
      const saveFile = vi.fn(() => {
        throw refused("file_changed", "it changed", Code.FailedPrecondition);
      });
      await edited({ readFile, saveFile }, session.auth);
      session.set(undefined);
      refocus();
      await signIn();
      session.set(authState().user);
      refocus();
      expect(await screen.findByText("the file has changed on the server since", { exact: false })).toBeVisible();
      await userEvent.click(screen.getByRole("button", { name: "Bring them back" }));
      await userEvent.click(saveButton());
      await waitFor(() => expect(saveFile).toHaveBeenCalledTimes(1));
      expect(saveFile.mock.lastCall).toEqual([expect.objectContaining({ version: "v1" }), expect.anything()]);
      expect(await screen.findByText("This file changed on the server")).toBeVisible();
    });

    it("never shows one person what another typed", async () => {
      const session = signedIn();
      await edited({ readFile: () => text("old\n") }, session.auth);
      session.set(undefined);
      refocus();
      await signIn();
      session.set(create(UserSchema, { id: "usr_someone_else", name: "Sam Okoye", role: Role.OWNER }));
      refocus();
      const field = await screen.findByRole("textbox", { name: "Contents of .user.ini" });
      expect(field).toHaveValue("old\n");
      expect(screen.queryByText("You have unsaved changes to .user.ini")).toBeNull();
    });
  });

  it("offers nothing back once you've said to leave it", async () => {
    vi.stubGlobal("confirm", () => true);
    const { router } = await edited({ readFile: () => text("old\n") });
    await userEvent.click(screen.getByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
    await act(() =>
      router.navigate({
        to: "/servers/$serverId/files",
        params: { serverId: "srv_1" },
        search: { path: "/var/www", file: "/var/www/.user.ini" },
      }),
    );
    await screen.findByRole("textbox", { name: "Contents of .user.ini" });
    expect(screen.queryByText("You have unsaved changes to .user.ini")).toBeNull();
  });

  it("closes up the columns of facts beside an open file, so a name keeps its room", async () => {
    vi.stubGlobal("confirm", () => true);
    const { router } = await edited({ readFile: () => text("old\n") });
    const width = (name: string) => screen.getByRole("columnheader", { name }).style.width;
    expect([width("Size"), width("Modified"), width("Permissions"), width("Owner")]).toEqual([
      "68px",
      "78px",
      "100px",
      "80px",
    ]);
    await userEvent.click(screen.getByRole("button", { name: "Close .user.ini" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www" }));
    expect([width("Size"), width("Modified"), width("Permissions"), width("Owner")]).toEqual([
      "84px",
      "92px",
      "116px",
      "96px",
    ]);
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
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Name", "Size", "Actions"]);
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
