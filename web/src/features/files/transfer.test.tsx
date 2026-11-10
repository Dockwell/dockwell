// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  type BeginUploadRequest,
  BeginUploadResponseSchema,
  downloadFileURL,
  FileEntrySchema,
  FileType,
  FinishUploadResponseSchema,
  ListFilesResponseSchema,
  Unpack,
  UploadSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { archiveKind, PART_BYTES, unpackedName } from "./transfer";

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

const site = create(ListFilesResponseSchema, {
  path: "/var/www",
  directory: entry("www", { type: FileType.DIRECTORY }),
  entries: [
    entry("index.php"),
    entry("assets", { type: FileType.DIRECTORY, mode: "drwxr-xr-x" }),
    entry("cache", { type: FileType.DIRECTORY, refused: true }),
    entry("current", { type: FileType.SYMLINK, linkTarget: "releases/7", linkType: FileType.DIRECTORY }),
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

/** A server's side of one upload: what it has, and what each PUT does to that. */
function anUpload(path: string, size: number) {
  const server = { received: 0, puts: [] as { offset: number; bytes: number }[] };
  /**
   * What the next PUT at offset does, before the usual: "drop" cuts it off after some
   * bytes arrive, and "busy" is the server having no room for it.
   */
  const next: ({ drop: number } | { busy: true } | undefined)[] = [];
  class FakeXHR {
    upload: { onprogress: ((event: { loaded: number }) => void) | null } = { onprogress: null };
    status = 0;
    responseText = "";
    private reason = "";
    getResponseHeader(name: string) {
      return name === "Dockwell-Error" ? this.reason : null;
    }
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    onabort: (() => void) | null = null;
    private url = "";
    open(_method: string, url: string) {
      this.url = url;
    }
    setRequestHeader() {}
    abort() {
      this.onabort?.();
    }
    send(body: Blob) {
      const offset = Number(new URL(this.url, "http://panel.example.com").searchParams.get("offset"));
      server.puts.push({ offset, bytes: body.size });
      queueMicrotask(() => {
        const then = next.shift();
        if (offset !== server.received) {
          this.status = 400;
          this.reason = "upload_offset";
          this.responseText = JSON.stringify({ code: 9, message: `Send from ${server.received}` });
          this.onload?.();
          return;
        }
        if (then && "busy" in then) {
          this.status = 429;
          this.reason = "transfers_busy";
          this.responseText = JSON.stringify({ code: 8, message: "the server is busy" });
          this.onload?.();
          return;
        }
        if (then) {
          server.received += then.drop;
          this.onerror?.();
          return;
        }
        server.received += body.size;
        this.upload.onprogress?.({ loaded: body.size });
        this.status = 200;
        this.onload?.();
      });
    }
  }
  vi.stubGlobal("XMLHttpRequest", FakeXHR);
  const state = () =>
    create(BeginUploadResponseSchema, {
      upload: create(UploadSchema, {
        uploadId: "UPLOAD0000000000000000000A",
        path,
        sizeBytes: BigInt(size),
        receivedBytes: BigInt(server.received),
        maxBytes: 16n << 30n,
      }),
    });
  const beginUpload = vi.fn((_req: BeginUploadRequest) => state());
  return { server, next, beginUpload };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Archives and links", () => {
  it("knows an archive by its name, and the folder it unpacks into", () => {
    expect(archiveKind("site.zip")).toBe("zip");
    expect(archiveKind("Site.TAR.GZ")).toBe("tarGz");
    expect(archiveKind("db.tgz")).toBe("tarGz");
    expect(archiveKind(".zip")).toBeUndefined();
    expect(archiveKind("notes.txt")).toBeUndefined();
    expect(unpackedName("theme.zip")).toBe("theme");
    expect(unpackedName("backup.2026.tar.gz")).toBe("backup.2026");
    expect(unpackedName(".zip")).toBe(".zip-unpacked");
  });

  it("downloads from the route the API document gives, with the path as it is", () => {
    expect(downloadFileURL({ serverId: "srv_1", path: "/var/www/a b&c.txt" })).toBe(
      "/v1/servers/srv_1/files:download?path=%2Fvar%2Fwww%2Fa+b%26c.txt",
    );
  });
});

describe("Uploading and downloading", () => {
  it("uploads a file in parts, goes on from where a dropped part got, and puts it in place", async () => {
    const size = PART_BYTES + 5000;
    const upload = anUpload("/var/www/backup.sql", size);
    // The second part drops after 2000 of its bytes arrive.
    upload.next.push(undefined, { drop: 2000 });
    const finishUpload = vi.fn(() =>
      create(FinishUploadResponseSchema, { path: "/var/www/backup.sql", file: entry("backup.sql") }),
    );
    const listFiles = vi.fn(() => site);
    await renderFiles({ listFiles, beginUpload: upload.beginUpload as never, finishUpload: finishUpload as never });

    await userEvent.click(screen.getByRole("button", { name: "Upload" }));
    const sheet = await screen.findByRole("dialog", { name: "Upload to www" });
    expect(within(sheet).getByText(/Up to 16 GB a file/)).toBeTruthy();
    await userEvent.upload(
      within(sheet).getByLabelText("Choose files to upload"),
      new File([new Uint8Array(size)], "backup.sql"),
    );
    const files = within(sheet).getByRole("list", { name: "Files to upload" });
    expect(within(files).getByText("backup.sql")).toBeTruthy();
    await expectAccessible(sheet);
    await userEvent.click(within(sheet).getByRole("button", { name: "Upload 1 file" }));

    await within(sheet).findByText(/^Uploaded · /, {}, { timeout: 5000 });
    expect(upload.beginUpload.mock.calls[0]?.[0]).toMatchObject({
      serverId: "srv_1",
      path: "/var/www/backup.sql",
      sizeBytes: BigInt(size),
      unpack: Unpack.UNSPECIFIED,
      proof: "",
    });
    // Once the drop, the server said where it got, and the rest went from there.
    expect(upload.beginUpload.mock.calls[1]?.[0]).toMatchObject({ uploadId: "UPLOAD0000000000000000000A" });
    expect(upload.server.puts).toEqual([
      { offset: 0, bytes: PART_BYTES },
      { offset: PART_BYTES, bytes: 5000 },
      { offset: PART_BYTES + 2000, bytes: 3000 },
    ]);
    expect(finishUpload).toHaveBeenCalledTimes(1);
    expect(listFiles.mock.calls.length).toBeGreaterThan(1);
    await userEvent.click(within(sheet).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("unpacks an archive when asked, and says so when it couldn't", async () => {
    const upload = anUpload("/var/www/theme.zip", 3);
    const finishUpload = vi
      .fn()
      .mockReturnValueOnce(
        create(FinishUploadResponseSchema, {
          path: "/var/www/theme.zip",
          unpackedTo: "/var/www/theme",
          unpackedEntries: 3n,
        }),
      )
      .mockImplementationOnce(() => {
        throw new ConnectError(
          "/var/www/fonts.tgz was uploaded, and wasn't unpacked: it holds a link. Unpack it over SSH",
          Code.FailedPrecondition,
          new Headers({ "Dockwell-Error": "unpack_refused" }),
        );
      });
    await renderFiles({ listFiles: () => site, beginUpload: upload.beginUpload as never, finishUpload });

    await userEvent.click(screen.getByRole("button", { name: "Upload" }));
    const sheet = await screen.findByRole("dialog", { name: "Upload to www" });
    await userEvent.upload(within(sheet).getByLabelText("Choose files to upload"), [
      new File(["abc"], "theme.zip"),
      new File([], "fonts.tgz"),
    ]);
    await userEvent.click(within(sheet).getByRole("switch", { name: /Unpack archives/ }));
    expect(within(sheet).getByText(/unpacks into theme$/)).toBeTruthy();
    await userEvent.click(within(sheet).getByRole("button", { name: "Upload 2 files" }));

    await within(sheet).findByText("Uploaded, and unpacked 3 items into theme");
    await within(sheet).findByText("Uploaded, and not unpacked");
    expect(within(sheet).getByText(/it holds a link/)).toBeTruthy();
    expect(upload.beginUpload.mock.calls.map((call) => call[0].unpack)).toEqual([Unpack.ZIP, Unpack.TAR_GZ]);
  });

  it("says what didn't upload, and goes on with it when asked", async () => {
    const upload = anUpload("/var/www/notes.txt", 5);
    upload.next.push({ drop: 0 }, { drop: 0 }, { drop: 0 }, { drop: 0 }, { drop: 0 }, { drop: 0 });
    await renderFiles({
      listFiles: () => site,
      beginUpload: upload.beginUpload as never,
      finishUpload: () => create(FinishUploadResponseSchema, { path: "/var/www/notes.txt" }),
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await userEvent.click(screen.getByRole("button", { name: "Upload" }));
      const sheet = await screen.findByRole("dialog", { name: "Upload to www" });
      const huge = new File([], "disk.img");
      Object.defineProperty(huge, "size", { value: 17 * 2 ** 30 });
      await userEvent.upload(within(sheet).getByLabelText("Choose files to upload"), [
        new File(["hello"], "notes.txt"),
        huge,
      ]);
      expect(within(sheet).getByText(/more than the 16 GB an upload can be/)).toBeTruthy();
      await userEvent.click(within(sheet).getByRole("button", { name: "Upload 1 file" }));
      // Five tries, a pause before each, and then it's yours to say.
      for (let i = 0; i < 6; i++) await vi.advanceTimersByTimeAsync(16_000);
      await within(sheet).findByText(/The connection dropped before notes.txt was all sent/);
      expect(upload.server.puts).toHaveLength(6);
      await userEvent.click(within(sheet).getByRole("button", { name: "Try again" }));
      await within(sheet).findByText(/^Uploaded · /);
      // The huge one never went.
      expect(upload.beginUpload.mock.calls.every((call) => call[0].path !== "/var/www/disk.img")).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it("waits for a server busy with other transfers, and says so", async () => {
    const upload = anUpload("/var/www/notes.txt", 5);
    upload.next.push({ busy: true }, { busy: true });
    await renderFiles({
      listFiles: () => site,
      beginUpload: upload.beginUpload as never,
      finishUpload: () => create(FinishUploadResponseSchema, { path: "/var/www/notes.txt" }),
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await userEvent.click(screen.getByRole("button", { name: "Upload" }));
      const sheet = await screen.findByRole("dialog", { name: "Upload to www" });
      await userEvent.upload(within(sheet).getByLabelText("Choose files to upload"), [
        new File(["hello"], "notes.txt"),
      ]);
      await userEvent.click(within(sheet).getByRole("button", { name: "Upload 1 file" }));
      await within(sheet).findByText(/Waiting for the server, which is busy/);
      for (let i = 0; i < 3; i++) await vi.advanceTimersByTimeAsync(4_000);
      await within(sheet).findByText(/^Uploaded · /);
      expect(upload.server.puts).toHaveLength(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("lets go of what reached the server once it won't be finished", async () => {
    const upload = anUpload("/var/www/notes.txt", 5);
    upload.next.push(...Array.from({ length: 12 }, () => ({ drop: 0 })));
    const cancelUpload = vi.fn((_req: { uploadId: string }) => ({}));
    await renderFiles({ listFiles: () => site, beginUpload: upload.beginUpload as never, cancelUpload });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await userEvent.click(screen.getByRole("button", { name: "Upload" }));
      const sheet = await screen.findByRole("dialog", { name: "Upload to www" });
      await userEvent.upload(within(sheet).getByLabelText("Choose files to upload"), [
        new File(["hello"], "notes.txt"),
        new File(["world"], "more.txt"),
      ]);
      await userEvent.click(within(sheet).getByRole("button", { name: "Upload 2 files" }));
      for (let i = 0; i < 12; i++) await vi.advanceTimersByTimeAsync(16_000);
      await within(sheet).findByText(/The connection dropped before more.txt was all sent/);
      expect(cancelUpload).not.toHaveBeenCalled();
      // Left out, it's stopped on the server.
      await userEvent.click(within(sheet).getByRole("button", { name: "Leave out notes.txt" }));
      await waitFor(() => expect(cancelUpload).toHaveBeenCalledTimes(1));
      expect(cancelUpload.mock.lastCall?.[0]).toMatchObject({ uploadId: "UPLOAD0000000000000000000A" });
      // Closed with one not finished, so is that.
      await userEvent.click(within(sheet).getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(cancelUpload).toHaveBeenCalledTimes(2));
    } finally {
      vi.useRealTimers();
    }
  });

  it("downloads a file, or a folder as a .zip, and not a link or what's refused", async () => {
    await renderFiles({ listFiles: () => site });
    const table = screen.getByRole("table", { name: "Files in /var/www" });
    await within(table).findByText("index.php");
    const file = within(table).getByRole("link", { name: "Download index.php" });
    expect(file.getAttribute("href")).toBe("/v1/servers/srv_1/files:download?path=%2Fvar%2Fwww%2Findex.php");
    expect(file.hasAttribute("download")).toBe(true);
    expect(within(table).getByRole("link", { name: "Download assets as a .zip" }).getAttribute("href")).toBe(
      "/v1/servers/srv_1/files:download?path=%2Fvar%2Fwww%2Fassets",
    );
    expect(within(table).queryByRole("link", { name: /Download cache/ })).toBeNull();
    expect(within(table).queryByRole("link", { name: /Download current/ })).toBeNull();
  });

  it("puts a row's actions behind one button beside an open file, so its name has room", async () => {
    async function* watchServers(_req: unknown, ctx: HandlerContext) {
      yield changed(hz1);
      yield snapshotComplete;
      await untilAborted(ctx.signal);
    }
    renderAt("/servers/srv_1/files?path=/var/www&file=/var/www/index.php", {
      servers: { watchServers },
      files: { listFiles: () => site, readFile: () => ({ path: "/var/www/index.php", content: "<?php\n" }) },
    });
    const table = await screen.findByRole("table", { name: "Files in /var/www" });
    await within(table).findByText("assets");
    await userEvent.click(within(table).getByRole("button", { name: "Actions for assets" }));
    const sheet = await screen.findByRole("dialog", { name: "assets" });
    expect(within(sheet).getByRole("link", { name: "Download as a .zip" }).getAttribute("href")).toBe(
      "/v1/servers/srv_1/files:download?path=%2Fvar%2Fwww%2Fassets",
    );
    expect(within(table).queryByRole("button", { name: "Rename assets" })).toBeNull();
    expect(screen.getByRole("link", { name: "Download index.php" })).toBeTruthy();
  });
});
