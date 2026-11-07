// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  DeletedFileSchema,
  DeleteFileResponseSchema,
  FileEntrySchema,
  FileType,
  ListDeletedFilesResponseSchema,
  ListFilesResponseSchema,
  MakeFileResponseSchema,
  MoveFileResponseSchema,
  RestoreFileResponseSchema,
  StepUpAction,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
import { type Api, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function entry(name: string, more: MessageInitShape<typeof FileEntrySchema> = {}) {
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

// The folder as the server resolved it: /srv/site is a link to /var/www.
const site = create(ListFilesResponseSchema, {
  path: "/var/www",
  directory: entry("www", { type: FileType.DIRECTORY }),
  entries: [
    entry("index.php"),
    entry("assets", { type: FileType.DIRECTORY, mode: "drwxr-xr-x" }),
    entry("cache", { type: FileType.DIRECTORY, refused: true }),
  ],
});

const kept = (path: string, id = "AAAAAAAAAAAAAAAAAAAAAAAAAA") =>
  create(DeletedFileSchema, {
    id,
    path,
    type: FileType.FILE,
    sizeBytes: 271n,
    deleteTime: timestampFromDate(new Date(Date.now() - 60_000)),
    purgeTime: timestampFromDate(new Date(Date.now() + 6.5 * 24 * 3600_000)),
  });

const refused = (reason: string, message: string, code = Code.FailedPrecondition) =>
  new ConnectError(message, code, new Headers({ "Dockwell-Error": reason }));

async function renderFiles(files: NonNullable<Api["files"]>, auth?: Api["auth"]) {
  async function* watchServers(_req: unknown, ctx: HandlerContext) {
    yield changed(hz1);
    yield snapshotComplete;
    await untilAborted(ctx.signal);
  }
  const rendered = renderAt("/servers/srv_1/files?path=/srv/site", { servers: { watchServers }, files, auth });
  await screen.findByRole("table", { name: "Files in /var/www" });
  return rendered;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Managing files", () => {
  it("makes a folder where the listing says it is, and a file that opens to edit", async () => {
    const makeFile = vi.fn((req: { path: string; type: FileType }) =>
      create(MakeFileResponseSchema, {
        path: req.path,
        file: entry(req.path.split("/").pop() ?? "", { type: req.type }),
      }),
    );
    const listFiles = vi.fn(() => site);
    const { router } = await renderFiles({
      listFiles,
      makeFile: makeFile as never,
      readFile: () => ({ path: "/var/www/new.php" }),
    });
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    const sheet = await screen.findByRole("dialog", { name: "New folder in www" });
    await expectAccessible(sheet);
    await userEvent.type(within(sheet).getByRole("textbox", { name: "Name" }), "uploads");
    await userEvent.click(within(sheet).getByRole("button", { name: "Make folder" }));
    await waitFor(() => expect(makeFile).toHaveBeenCalledTimes(1));
    expect(makeFile.mock.lastCall?.[0]).toMatchObject({
      serverId: "srv_1",
      path: "/var/www/uploads",
      type: FileType.DIRECTORY,
      proof: "",
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(listFiles.mock.calls.length).toBeGreaterThan(1);

    await userEvent.click(screen.getByRole("button", { name: "New file" }));
    const named = await screen.findByRole("dialog", { name: "New file in www" });
    await userEvent.type(within(named).getByRole("textbox", { name: "Name" }), "new.php{Enter}");
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/var/www", file: "/var/www/new.php" }));
    expect(makeFile.mock.lastCall?.[0]).toMatchObject({ path: "/var/www/new.php", type: FileType.FILE });
  });

  it("says what a name can't be, and what's there already, before and after asking", async () => {
    const makeFile = vi.fn(() => {
      throw refused("file_exists", "/var/www/assets is there already", Code.AlreadyExists);
    });
    await renderFiles({ listFiles: () => site, makeFile });
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    const sheet = await screen.findByRole("dialog");
    const name = within(sheet).getByRole("textbox", { name: "Name" });
    await userEvent.type(name, "a/b{Enter}");
    expect(await within(sheet).findByText("A name can't have a / in it", { exact: false })).toBeVisible();
    expect(makeFile).not.toHaveBeenCalled();
    await userEvent.clear(name);
    await userEvent.type(name, "assets{Enter}");
    expect(
      await within(sheet).findByText("Something called assets is here already. Choose another name."),
    ).toBeVisible();
  });

  it("renames and moves from a row, to where a folder resolves", async () => {
    const moveFile = vi.fn((req: { from: string; to: string }) =>
      create(MoveFileResponseSchema, { from: req.from, to: req.to, file: entry("x") }),
    );
    const listFiles = vi.fn((req: { path: string }) =>
      req.path === "/srv/archive" ? create(ListFilesResponseSchema, { path: "/mnt/archive" }) : site,
    );
    await renderFiles({ listFiles: listFiles as never, moveFile: moveFile as never });
    // What Dockwell doesn't open has nothing to do on its row.
    expect(screen.queryByRole("button", { name: "Delete cache" })).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: "Rename index.php" }));
    const rename = await screen.findByRole("dialog", { name: "Rename index.php" });
    const name = within(rename).getByRole("textbox", { name: "Name" });
    await userEvent.clear(name);
    await userEvent.type(name, "home.php{Enter}");
    await waitFor(() => expect(moveFile).toHaveBeenCalledTimes(1));
    expect(moveFile.mock.lastCall?.[0]).toMatchObject({ from: "/var/www/index.php", to: "/var/www/home.php" });

    await userEvent.click(screen.getByRole("button", { name: "Move assets" }));
    const move = await screen.findByRole("dialog", { name: "Move assets" });
    const to = within(move).getByRole("textbox", { name: "To the folder" });
    expect(to).toHaveValue("/var/www");
    await userEvent.clear(to);
    await userEvent.type(to, "/srv/archive/{Enter}");
    await waitFor(() => expect(moveFile).toHaveBeenCalledTimes(2));
    expect(moveFile.mock.lastCall?.[0]).toMatchObject({ from: "/var/www/assets", to: "/mnt/archive/assets" });
  });

  it("asks for a passkey for both ends of a move, as the server resolved them", async () => {
    stubPasskeys();
    const moveFile = vi.fn((req: { from: string; to: string; proof: string }) => {
      if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up", "moving it can let someone back in");
      return create(MoveFileResponseSchema, { from: req.from, to: req.to, file: entry("authorized_keys") });
    });
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    await renderFiles(
      { listFiles: () => site, moveFile: moveFile as never },
      { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
    );
    await userEvent.click(screen.getByRole("button", { name: "Rename index.php" }));
    const name = within(await screen.findByRole("dialog")).getByRole("textbox", { name: "Name" });
    await userEvent.clear(name);
    await userEvent.type(name, "authorized_keys{Enter}");
    await waitFor(() => expect(moveFile).toHaveBeenCalledTimes(2));
    expect(beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({
        action: StepUpAction.MOVE_WAY_BACK_IN,
        target: "srv_1:/var/www/index.php\n/var/www/authorized_keys",
      }),
      expect.anything(),
    );
    expect(moveFile.mock.lastCall?.[0]).toMatchObject({ proof: "proof_1" });
  });

  it("deletes to Recently deleted, and puts it back from there", async () => {
    const deleteFile = vi.fn((req: { path: string }) =>
      create(DeleteFileResponseSchema, { path: req.path, deleted: kept(req.path) }),
    );
    const restoreFile = vi.fn((_req: { id: string; proof: string }) =>
      create(RestoreFileResponseSchema, { path: "/var/www/index.php", file: entry("index.php") }),
    );
    await renderFiles({ listFiles: () => site, deleteFile: deleteFile as never, restoreFile: restoreFile as never });
    await userEvent.click(screen.getByRole("button", { name: "Delete index.php" }));
    const sheet = await screen.findByRole("dialog", { name: "Delete index.php" });
    expect(within(sheet).getByText("index.php goes to Recently deleted", { exact: false })).toBeVisible();
    await expectAccessible(sheet);
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleteFile).toHaveBeenCalledTimes(1));
    expect(deleteFile.mock.lastCall?.[0]).toMatchObject({ path: "/var/www/index.php", forGood: false });
    const notice = await screen.findByText("Deleted index.php");
    expect(screen.getByText("It's in Recently deleted for 7 days", { exact: false })).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Put back" }));
    await waitFor(() => expect(restoreFile).toHaveBeenCalledTimes(1));
    expect(restoreFile.mock.lastCall?.[0]).toMatchObject({ id: "AAAAAAAAAAAAAAAAAAAAAAAAAA", proof: "" });
    await waitFor(() => expect(notice).not.toBeInTheDocument());
  });

  it("says plainly when a delete can't be put back, and deletes for good only then", async () => {
    const deleteFile = vi.fn((req: { path: string; forGood: boolean }) => {
      if (!req.forGood) {
        throw refused(
          "delete_for_good",
          "/var/www/index.php is on another disk or mount than Dockwell's data, so a delete can't be put back, and nothing was deleted. Delete it for good to go on",
        );
      }
      return create(DeleteFileResponseSchema, { path: req.path });
    });
    await renderFiles({ listFiles: () => site, deleteFile: deleteFile as never });
    await userEvent.click(screen.getByRole("button", { name: "Delete index.php" }));
    const sheet = await screen.findByRole("dialog");
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    expect(await within(sheet).findByText("This can't be put back")).toBeVisible();
    expect(deleteFile).toHaveBeenCalledTimes(1);
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete for good" }));
    await waitFor(() => expect(deleteFile).toHaveBeenCalledTimes(2));
    expect(deleteFile.mock.lastCall?.[0]).toMatchObject({ forGood: true });
    expect(await screen.findByText("Deleted index.php for good")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Put back" })).toBeNull();
  });

  it("lists what was deleted, puts it back, and deletes it for good once you say so", async () => {
    const listed = create(ListDeletedFilesResponseSchema, {
      deleted: [kept("/var/www/old.php"), kept("/var/www/older.php", "BBBBBBBBBBBBBBBBBBBBBBBBBB")],
    });
    const restoreFile = vi.fn(() => {
      throw refused("file_exists", "/var/www/old.php is there already", Code.AlreadyExists);
    });
    const purgeDeletedFile = vi.fn((_req: { id: string }) => ({ deleted: kept("/var/www/older.php") }));
    await renderFiles({
      listFiles: () => site,
      listDeletedFiles: () => listed,
      restoreFile,
      purgeDeletedFile: purgeDeletedFile as never,
    });
    await userEvent.click(screen.getByRole("button", { name: "Recently deleted" }));
    const sheet = await screen.findByRole("dialog", { name: "Recently deleted" });
    const list = await within(sheet).findByRole("list", { name: "Recently deleted" });
    expect(within(list).getByText("old.php")).toBeVisible();
    expect(within(list).getAllByText("goes in 6 days", { exact: false })).toHaveLength(2);
    await expectAccessible(sheet);
    await userEvent.click(within(list).getAllByRole("button", { name: "Put back" })[0] as HTMLElement);
    expect(await within(sheet).findByText("Something is at /var/www/old.php now", { exact: false })).toBeVisible();
    await userEvent.click(within(list).getByRole("button", { name: "Delete older.php for good" }));
    expect(purgeDeletedFile).not.toHaveBeenCalled();
    await userEvent.click(within(list).getByRole("button", { name: "Delete for good" }));
    await waitFor(() => expect(purgeDeletedFile).toHaveBeenCalledTimes(1));
    expect(purgeDeletedFile.mock.lastCall?.[0]).toMatchObject({ id: "BBBBBBBBBBBBBBBBBBBBBBBBBB" });
  });
});

describe("Putting back and deleting for good", () => {
  it("says a folder that's gone is gone, and to make it again", async () => {
    const listed = create(ListDeletedFilesResponseSchema, { deleted: [kept("/var/www/old/a.php")] });
    const restoreFile = vi.fn(() => {
      throw refused("folder_gone", "the folder /var/www/old is gone");
    });
    await renderFiles({ listFiles: () => site, listDeletedFiles: () => listed, restoreFile });
    await userEvent.click(screen.getByRole("button", { name: "Recently deleted" }));
    const sheet = await screen.findByRole("dialog", { name: "Recently deleted" });
    await userEvent.click(await within(sheet).findByRole("button", { name: "Put back" }));
    expect(
      await within(sheet).findByText(
        "The folder /var/www/old is gone, so a.php wasn't put back, and it's still in Recently deleted. Make the folder again first, then put it back.",
      ),
    ).toBeVisible();
  });

  it("says when nothing is in Recently deleted", async () => {
    await renderFiles({ listFiles: () => site, listDeletedFiles: () => create(ListDeletedFilesResponseSchema, {}) });
    await userEvent.click(screen.getByRole("button", { name: "Recently deleted" }));
    expect(await screen.findByText("Nothing is in Recently deleted.")).toBeVisible();
  });

  it("takes one passkey for a way back in that can only be deleted for good", async () => {
    stubPasskeys();
    const deleteFile = vi.fn((req: { path: string; forGood: boolean; proof: string }) => {
      if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up", "deleting it can let someone back in");
      if (!req.forGood) throw refused("delete_for_good", "it's on another disk, so a delete can't be put back");
      return create(DeleteFileResponseSchema, { path: req.path });
    });
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    await renderFiles(
      { listFiles: () => site, deleteFile: deleteFile as never },
      { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
    );
    await userEvent.click(screen.getByRole("button", { name: "Delete index.php" }));
    const sheet = await screen.findByRole("dialog");
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    expect(await within(sheet).findByText("This can't be put back")).toBeVisible();
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete for good" }));
    expect(await screen.findByText("Deleted index.php for good")).toBeVisible();
    expect(beginStepUp).toHaveBeenCalledTimes(1);
    expect(deleteFile.mock.lastCall?.[0]).toMatchObject({ forGood: true, proof: "proof_1" });
  });
});

describe("Managing files on a phone", () => {
  it("keeps a row's actions behind one button", async () => {
    onPhone();
    const deleteFile = vi.fn((req: { path: string }) =>
      create(DeleteFileResponseSchema, { path: req.path, deleted: kept(req.path) }),
    );
    await renderFiles({ listFiles: () => site, deleteFile: deleteFile as never });
    expect(screen.queryByRole("button", { name: "Rename index.php" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Actions for index.php" }));
    const actions = await screen.findByRole("dialog", { name: "index.php" });
    await expectAccessible(actions);
    await userEvent.click(within(actions).getByRole("button", { name: "Delete" }));
    const sheet = await screen.findByRole("dialog", { name: "Delete index.php" });
    await userEvent.click(within(sheet).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleteFile).toHaveBeenCalledTimes(1));
  });
});
