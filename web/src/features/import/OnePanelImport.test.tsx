// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  ImportAction,
  ImportKind,
  ImportOnePanelResponseSchema,
  ListContainersResponseSchema,
  ServerContainerSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, ...more };
}

const found = () =>
  create(ImportOnePanelResponseSchema, {
    found: { version: "v2.4.2", baseDir: "/opt", records: "/opt/1panel/db/agent.db", knownVersion: true },
    steps: [
      {
        kind: ImportKind.APP,
        name: "wordpress",
        summary: "WordPress 6.7.1 from 1Panel's store, running",
        action: ImportAction.TAKE_ON,
        becomes: "the project wordpress, taken on as it runs, with no restart",
      },
      {
        kind: ImportKind.WEBSITE,
        name: "blog.example.com",
        summary: "a static site",
        action: ImportAction.MAKE,
        becomes: "a website serving /opt/1panel/www/sites/blog/index, in place",
        waitsFor: "#206",
      },
      {
        kind: ImportKind.APP,
        name: "openresty",
        summary: "OpenResty 1.27.1.2 from 1Panel's store, running",
        action: ImportAction.LEAVE,
        reason: "It serves 1Panel's websites on ports 80 and 443.",
      },
    ],
    change: create(ChangeSchema, {
      id: "chg_import",
      summary: "Take on what 1Panel runs on hz-1",
      state: ChangeState.PLANNED,
      plan: { fingerprint: "feedfacefeedfacefeedfacefeedface", summary: { change: 1 } },
    }),
  });

describe("Moving in from 1Panel", () => {
  it("is offered on a server's Overview where 1Panel's containers run, and nowhere else", async () => {
    const containers = (names: string[]) =>
      create(ListContainersResponseSchema, {
        containers: names.map((name) => create(ServerContainerSchema, { name })),
      });
    renderAt("/servers/srv_1", api({ containers: { listContainers: () => containers(["1Panel-mysql-AbCd", "web"]) } }));
    const offer = await screen.findByText("1Panel runs on this server");
    expect(offer).toBeTruthy();
    expect(screen.getByRole("link", { name: "Move in from 1Panel" }).getAttribute("href")).toBe(
      "/servers/srv_1/import",
    );
  });

  it("isn't offered where nothing of 1Panel's runs", async () => {
    const listContainers = vi.fn(() =>
      create(ListContainersResponseSchema, { containers: [create(ServerContainerSchema, { name: "web" })] }),
    );
    renderAt("/servers/srv_1", api({ containers: { listContainers } }));
    await waitFor(() => expect(listContainers).toHaveBeenCalled());
    await screen.findByText("web");
    expect(screen.queryByText("1Panel runs on this server")).toBeNull();
  });

  it("reads what 1Panel keeps when asked, groups it, and hands the plan to the plan sheet", async () => {
    const importOnePanel = vi.fn((_req: { serverId: string }) => found());
    const discardChange = vi.fn((_req: { id: string }) => ({}));
    renderAt(
      "/servers/srv_1/import",
      api({
        imports: { importOnePanel },
        changes: {
          discardChange,
          getChange: () => ({ change: found().change }),
          watchChange: async function* () {},
        },
      }),
    );
    const read = await screen.findByRole("button", { name: "Read what 1Panel keeps" });
    expect(importOnePanel).not.toHaveBeenCalled();
    await expectAccessible(document.body);
    await userEvent.click(read);
    await waitFor(() => expect(importOnePanel).toHaveBeenCalledTimes(1));
    expect(importOnePanel.mock.lastCall?.[0]).toMatchObject({ serverId: "srv_1" });

    await screen.findByText(/1Panel v2.4.2, under/);
    const take = screen.getByRole("list", { name: "Take on, as it runs" });
    expect(within(take).getByText("wordpress")).toBeTruthy();
    expect(within(take).getByText("Becomes the project wordpress, taken on as it runs, with no restart.")).toBeTruthy();
    const make = screen.getByRole("list", { name: "Make again in Dockwell" });
    expect(within(make).getByText("Waits for #206")).toBeTruthy();
    const leave = screen.getByRole("list", { name: "Leave as it is" });
    expect(within(leave).getByText("It serves 1Panel's websites on ports 80 and 443.")).toBeTruthy();
    await expectAccessible(document.body);

    await userEvent.click(screen.getByRole("button", { name: "Review the plan" }));
    await screen.findByRole("dialog");
    // Handed to the sheet, the plan is the sheet's: the page doesn't discard it.
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("lets go of a plan nobody reviewed when it reads again", async () => {
    const importOnePanel = vi.fn((_req: { serverId: string }) => found());
    const discardChange = vi.fn((_req: { id: string }) => ({}));
    renderAt("/servers/srv_1/import", api({ imports: { importOnePanel }, changes: { discardChange } }));
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await userEvent.click(await screen.findByRole("button", { name: "Read it again" }));
    await waitFor(() => expect(importOnePanel).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(discardChange).toHaveBeenCalledTimes(1));
    expect(discardChange.mock.lastCall?.[0]).toMatchObject({ id: "chg_import" });
  });

  it("says why it couldn't read, in the server's words", async () => {
    const importOnePanel = vi.fn(() => {
      throw new ConnectError(
        "hz-1 doesn't run 1Panel: there's no /opt/1panel/db/agent.db or /opt/1panel/db/1Panel.db",
        Code.FailedPrecondition,
        new Headers({ "Dockwell-Error": "onepanel_not_found" }),
      );
    });
    renderAt("/servers/srv_1/import", api({ imports: { importOnePanel } }));
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await screen.findByText("Couldn't read what 1Panel keeps");
    expect(screen.getByText(/hz-1 doesn't run 1Panel/)).toBeTruthy();
  });
});
