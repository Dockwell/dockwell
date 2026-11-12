// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ServiceUpdateSchema,
  UpdateCheck,
  UpdateKind,
  UpdateProblem,
  UpdateSource,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { anApp, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hour = 3_600_000;

/** postgres:16.3, with 16.4 out three days and 17.2 beside it. */
function postgres(snoozed = false) {
  return anApp(
    "db",
    "postgres:16.3",
    {},
    {
      update: create(ServiceUpdateSchema, {
        check: UpdateCheck.AVAILABLE,
        source: UpdateSource.IMAGE,
        checkTime: timestampFromDate(new Date(Date.now() - 2 * hour)),
        snoozeTime: snoozed ? timestampFromDate(new Date(Date.now() + 24 * hour)) : undefined,
        available: [
          {
            kind: UpdateKind.PATCH,
            fromVersion: "16.3",
            toVersion: "16.4",
            fromImage: "postgres:16.3",
            toImage: "postgres:16.4",
            releaseTime: timestampFromDate(new Date(Date.now() - 72 * hour)),
            notesUrl: "https://github.com/docker-library/postgres/releases",
          },
          {
            kind: UpdateKind.MAJOR,
            fromVersion: "16.3",
            toVersion: "17.2",
            fromImage: "postgres:16.3",
            toImage: "postgres:17.2",
          },
        ],
      }),
    },
  );
}

function upToDate() {
  return anApp(
    "web",
    "nginx:1.27",
    {},
    {
      update: create(ServiceUpdateSchema, {
        check: UpdateCheck.UP_TO_DATE,
        source: UpdateSource.IMAGE,
        checkTime: timestampFromDate(new Date(Date.now() - hour)),
      }),
    },
  );
}

function api(services: ReturnType<typeof anApp>[], extra: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1", inventory: { addresses: [] } })), snapshotComplete);
  return {
    services: { listServices: () => ({ services }) },
    resources: places(),
    servers: { watchServers: watch.stream },
    ...extra,
  };
}

afterEach(async () => {
  localStorage.clear();
  await activateLocale("en");
});

describe("updates on the apps list", () => {
  it("marks an app with an update, counts them, and filters to them", async () => {
    const user = userEvent.setup();
    renderAt("/apps", api([postgres(), upToDate()]));
    const table = await screen.findByRole("table", { name: "Apps" });
    const db = within(table).getByRole("row", { name: /db/ });
    expect(within(db).getByTitle("Update available: 16.3 → 16.4, patch")).toHaveTextContent("16.4");
    expect(within(within(table).getByRole("row", { name: /web/ })).queryByTitle(/Update available/)).toBeNull();
    const filter = screen.getByRole("button", { name: "1 update available" });
    expect(filter).toHaveAttribute("aria-pressed", "false");
    await user.click(filter);
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Apps" })).queryByRole("row", { name: /web/ })).toBeNull(),
    );
    expect(screen.getByRole("button", { name: "1 update available" })).toHaveAttribute("aria-pressed", "true");
  });

  it("counts nothing for an app whose updates are snoozed", async () => {
    renderAt("/apps", api([postgres(true), upToDate()]));
    await screen.findByRole("table", { name: "Apps" });
    expect(screen.queryByRole("button", { name: /update available/ })).toBeNull();
    expect(screen.queryByTitle(/Update available/)).toBeNull();
  });
});

describe("the update card on an app's page", () => {
  it("offers the patch first, with its notes and date, the major beside it, and skip one step away", async () => {
    const user = userEvent.setup();
    const skipped: string[] = [];
    renderAt(
      "/apps/svc_db",
      api([postgres()], {
        appUpdates: {
          skipServiceUpdate: (req) => {
            skipped.push(req.version);
            return {};
          },
        },
      }),
    );
    const card = (await screen.findByRole("heading", { name: "Update available" })).closest("section") as HTMLElement;
    expect(within(card).getByText("16.3 → 16.4")).toBeVisible();
    expect(within(card).getByText("patch")).toBeVisible();
    expect(within(card).getByText("out 3 days")).toBeVisible();
    expect(within(card).getByRole("link", { name: "Notes" })).toHaveAttribute(
      "href",
      "https://github.com/docker-library/postgres/releases",
    );
    expect(within(card).getByRole("button", { name: "Update to 16.4" })).toBeEnabled();
    expect(within(card).getByRole("button", { name: "Update to 17.2" })).toBeEnabled();
    expect(within(card).getByText("major")).toBeVisible();
    await user.click(within(card).getByText("Not now"));
    await user.click(within(card).getByRole("button", { name: "Skip 16.4" }));
    await waitFor(() => expect(skipped).toEqual(["16.4"]));
  });

  it("opens the deploy sheet with the new image filled in", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_db", api([postgres()]));
    await user.click(await screen.findByRole("button", { name: "Update to 16.4" }));
    expect(await screen.findByDisplayValue("postgres:16.4")).toBeVisible();
  });

  it("says an app is up to date in a line, with Check now", async () => {
    const user = userEvent.setup();
    let checked = 0;
    renderAt(
      "/apps/svc_web",
      api([upToDate()], {
        appUpdates: {
          checkServiceUpdates: () => {
            checked += 1;
            return {};
          },
        },
      }),
    );
    const card = (await screen.findByRole("heading", { name: "Updates" })).closest("section") as HTMLElement;
    expect(within(card).getByText("Up to date")).toBeVisible();
    expect(within(card).getByText(/Checked 1 h ago/)).toBeVisible();
    await user.click(within(card).getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(checked).toBe(1));
  });

  it("says why an app can't be checked, in a line", async () => {
    const app = anApp(
      "shop",
      "ghcr.io/acme/shop:1.4",
      {},
      {
        update: create(ServiceUpdateSchema, { check: UpdateCheck.FAILED, problem: UpdateProblem.PRIVATE_OR_NOT_FOUND }),
      },
    );
    renderAt("/apps/svc_shop", api([app]));
    expect(await screen.findByText("Can't check: the image is private, or not on its registry")).toBeVisible();
  });

  it("says nothing before an app's first check", async () => {
    renderAt("/apps/svc_web", api([anApp("web", "nginx:1.27")]));
    await screen.findByRole("heading", { name: "Configuration" });
    expect(screen.queryByRole("heading", { name: "Updates" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Update available" })).toBeNull();
  });

  it("reads in Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/apps/svc_db", api([postgres()]));
    expect(await screen.findByRole("heading", { name: "有可用更新" })).toBeVisible();
    expect(screen.getByText("补丁")).toBeVisible();
    expect(screen.getByRole("link", { name: "发布说明" })).toBeVisible();
  });
});
