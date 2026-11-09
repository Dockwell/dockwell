// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  Connection,
  ImportAction,
  ImportKind,
  ImportOnePanelResponseSchema,
  ImportWaitsFor,
  ListContainersResponseSchema,
  ServerContainerSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, ...more };
}

const containers = (names: string[]) =>
  create(ListContainersResponseSchema, { containers: names.map((name) => create(ServerContainerSchema, { name })) });

const found = (withPlan = false) =>
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
      ...["blog.example.com", "shop.example.com", "docs.example.com"].map((name) => ({
        kind: ImportKind.WEBSITE,
        name,
        summary: "a static site",
        action: ImportAction.MAKE,
        becomes: "a website serving /opt/1panel/www/sites/blog/index, in place",
        waitsFor: ImportWaitsFor.WEBSITES,
      })),
      {
        kind: ImportKind.APP,
        name: "mysql",
        summary: "MySQL 8.4.3 from 1Panel's store, running",
        action: ImportAction.DONE,
        becomes: "the project mysql, taken on already",
      },
      {
        kind: ImportKind.APP,
        name: "openresty",
        summary: "OpenResty 1.27.1.2 from 1Panel's store, running",
        action: ImportAction.LEAVE,
        reason: "It serves 1Panel's websites on ports 80 and 443.",
      },
    ],
    change: withPlan
      ? create(ChangeSchema, {
          id: "chg_import",
          summary: "Take on what 1Panel runs on hz-1",
          state: ChangeState.PLANNED,
          plan: { fingerprint: "feedfacefeedfacefeedfacefeedface", summary: { change: 1 } },
        })
      : undefined,
  });

afterEach(() => {
  localStorage.clear();
});

describe("Moving in from 1Panel", () => {
  it("is offered on a server's Overview where 1Panel's containers run, until someone says not now", async () => {
    renderAt("/servers/srv_1", api({ containers: { listContainers: () => containers(["1Panel-mysql-AbCd", "web"]) } }));
    const offer = await screen.findByText("Containers 1Panel made run on this server");
    expect(offer).toBeTruthy();
    expect(screen.getByRole("link", { name: "Move in from 1Panel" }).getAttribute("href")).toBe(
      "/servers/srv_1/import",
    );
    // The server's own tabs have a way in that doesn't rest on the callout.
    expect(screen.getByRole("link", { name: "Move in" }).getAttribute("href")).toBe("/servers/srv_1/import");
    await userEvent.click(screen.getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByText("Containers 1Panel made run on this server")).toBeNull());
    expect(screen.getByRole("link", { name: "Move in" })).toBeTruthy();
  });

  it("isn't offered where nothing of 1Panel's is left to move, OpenResty aside", async () => {
    const listContainers = vi.fn(() => containers(["web", "1Panel-openresty-EfGh"]));
    renderAt("/servers/srv_1", api({ containers: { listContainers } }));
    await waitFor(() => expect(listContainers).toHaveBeenCalled());
    await screen.findByText("web");
    expect(screen.queryByText("Containers 1Panel made run on this server")).toBeNull();
    expect(screen.queryByRole("link", { name: "Move in" })).toBeNull();
  });

  it("hides what it read while the server or Docker can't answer, and reads again on return", async () => {
    const listContainers = vi.fn(() => containers(["1Panel-mysql-AbCd"]));
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    renderAt("/servers/srv_1", {
      servers: { watchServers: watch.stream },
      containers: { listContainers },
    });
    const offer = "Containers 1Panel made run on this server";
    await screen.findByText(offer);
    for (const fields of [
      { connection: Connection.UNREACHABLE },
      { connection: Connection.PENDING },
      { dockerUnreadableSince: timestampFromDate(new Date()) },
    ]) {
      const reads = listContainers.mock.calls.length;
      watch.push(changed(aServer({ id: hz1.id, name: hz1.name, ...fields })));
      await waitFor(() => expect(screen.queryByText(offer)).toBeNull());
      expect(screen.queryByRole("link", { name: "Move in" })).toBeNull();
      expect(listContainers).toHaveBeenCalledTimes(reads);
      watch.push(changed(hz1));
      await screen.findByText(offer);
      await waitFor(() => expect(listContainers.mock.calls.length).toBeGreaterThan(reads));
    }
  });

  it("reads what 1Panel keeps without planning, groups it by when each moves, and has the sheet make the plan", async () => {
    const importOnePanel = vi.fn((req: { serverId: string; skipPlan?: boolean }) => found(!req.skipPlan));
    const discardChange = vi.fn((_req: { id: string }) => ({}));
    renderAt(
      "/servers/srv_1/import",
      api({
        imports: { importOnePanel },
        changes: {
          discardChange,
          getChange: () => ({ change: found(true).change }),
          watchChange: async function* () {},
        },
      }),
    );
    const read = await screen.findByRole("button", { name: "Read what 1Panel keeps" });
    expect(importOnePanel).not.toHaveBeenCalled();
    await expectAccessible(document.body);
    await userEvent.click(read);
    await waitFor(() => expect(importOnePanel).toHaveBeenCalledTimes(1));
    expect(importOnePanel.mock.lastCall?.[0]).toMatchObject({ serverId: "srv_1", skipPlan: true });

    await screen.findByText(/1Panel v2.4.2, under/);
    const plan = screen.getByRole("list", { name: "In this plan" });
    expect(within(plan).getByText("wordpress")).toBeTruthy();
    expect(within(plan).getByText("Becomes")).toBeTruthy();
    expect(within(plan).getByText("the project wordpress, taken on as it runs, with no restart")).toBeTruthy();
    // What waits is folded to a count, and opens.
    expect(screen.getByText("Later, once Dockwell has websites (3)")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Later, once Dockwell has websites" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Show all 3" }));
    expect(
      within(screen.getByRole("list", { name: "Later, once Dockwell has websites" })).getAllByText("a static site"),
    ).toHaveLength(3);
    expect(within(screen.getByRole("list", { name: "In Dockwell already" })).getByText("mysql")).toBeTruthy();
    const stays = screen.getByRole("list", { name: "Stays with 1Panel" });
    expect(within(stays).getByText("It serves 1Panel's websites on ports 80 and 443.")).toBeTruthy();
    await expectAccessible(document.body);

    // Review is at the top and where the list ends, and the sheet makes the plan.
    const reviews = screen.getAllByRole("button", { name: "Review the plan" });
    expect(reviews).toHaveLength(2);
    await userEvent.click(reviews[1] as HTMLElement);
    await screen.findByRole("dialog");
    await waitFor(() => expect(importOnePanel).toHaveBeenCalledTimes(2));
    expect(importOnePanel.mock.lastCall?.[0].skipPlan).toBeFalsy();
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("leaves nothing planned when the page is left mid-read", async () => {
    let finish: (res: ReturnType<typeof found>) => void = () => {};
    const importOnePanel = vi.fn(
      (_req: { serverId: string; skipPlan?: boolean }) =>
        new Promise<ReturnType<typeof found>>((resolve) => {
          finish = resolve;
        }),
    );
    const discardChange = vi.fn((_req: { id: string }) => ({}));
    const { router } = renderAt(
      "/servers/srv_1/import",
      api({
        imports: { importOnePanel },
        changes: { discardChange },
        containers: { listContainers: () => containers([]) },
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await waitFor(() => expect(importOnePanel).toHaveBeenCalledTimes(1));
    await router.navigate({ to: "/servers/$serverId", params: { serverId: "srv_1" } });
    finish(found());
    await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_1"));
    await waitFor(() => expect(screen.queryByRole("button", { name: /Read/ })).toBeNull());
    // The read asked for no plan, so there was none to leave behind or discard.
    expect(importOnePanel.mock.lastCall?.[0]).toMatchObject({ skipPlan: true });
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("plans nothing when it reads again, and forgets what it found when a read fails", async () => {
    let fail = false;
    const importOnePanel = vi.fn((_req: { serverId: string; skipPlan?: boolean }) => {
      if (fail) throw new ConnectError("hz-1's agent isn't connected", Code.Unavailable);
      return found();
    });
    const discardChange = vi.fn((_req: { id: string }) => ({}));
    renderAt("/servers/srv_1/import", api({ imports: { importOnePanel }, changes: { discardChange } }));
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await screen.findByRole("list", { name: "In this plan" });
    fail = true;
    await userEvent.click(screen.getByRole("button", { name: "Read it again" }));
    await screen.findByText("Couldn't read what 1Panel keeps");
    expect(screen.queryByRole("list", { name: "In this plan" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Review the plan" })).toBeNull();
    expect(importOnePanel.mock.calls.every((call) => call[0].skipPlan)).toBe(true);
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("says plainly where 1Panel isn't, and which versions it was written from", async () => {
    const importOnePanel = vi.fn(() => {
      throw new ConnectError(
        "hz-1 doesn't run 1Panel: there's no /opt/1panel/db/agent.db or /opt/1panel/db/1Panel.db. Run this for a server where 1Panel is installed",
        Code.FailedPrecondition,
        new Headers({ "Dockwell-Error": "onepanel_not_found" }),
      );
    });
    renderAt("/servers/srv_1/import", api({ imports: { importOnePanel } }));
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await screen.findByText("1Panel isn't on this server");
    expect(screen.queryByText("Couldn't read what 1Panel keeps")).toBeNull();
    expect(screen.getByText(/hz-1 doesn't run 1Panel/)).toBeTruthy();
  });

  it("names the versions it was written from when this one isn't", async () => {
    const other = found();
    if (other.found) other.found = { ...other.found, version: "v2.6.0", knownVersion: false };
    renderAt("/servers/srv_1/import", api({ imports: { importOnePanel: () => other } }));
    await userEvent.click(await screen.findByRole("button", { name: "Read what 1Panel keeps" }));
    await screen.findByText("A version of 1Panel this wasn't written from");
    expect(screen.getByText(/written from 1Panel v2.0 to v2.4.2, and v1.10, and this server runs v2.6.0/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Move in" })).toBeTruthy();
  });
});
