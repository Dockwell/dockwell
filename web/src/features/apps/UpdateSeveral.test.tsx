// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  BackupMethod,
  type Change,
  ChangeSchema,
  ChangeState,
  IntentKind,
  PlanAction,
  type ServiceInfo,
  ServiceUpdateSchema,
  UpdateCheck,
  UpdateKind,
  UpdateSource,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, SERVER } from "../../test/apps";
import { moved, watched } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

/** An app on image from, with an update to to of kind. */
function withUpdate(name: string, from: string, to: string, kind: UpdateKind): ServiceInfo {
  const repo = from.split(":")[0] ?? from;
  return anApp(
    name,
    from,
    {},
    {
      update: create(ServiceUpdateSchema, {
        check: UpdateCheck.AVAILABLE,
        source: UpdateSource.IMAGE,
        available: [
          {
            kind,
            fromVersion: from.split(":")[1] ?? "",
            toVersion: to,
            fromImage: from,
            toImage: `${repo}:${to}`,
          },
        ],
      }),
    },
  );
}

/** The plan of updating a service, with its backup first and its downtime. */
function planOf(service: ServiceInfo, image: string): Change {
  const id = `chg_up_${service.resource?.name}`;
  return create(ChangeSchema, {
    id,
    summary: `Update ${service.resource?.name} to ${image}`,
    state: ChangeState.PLANNED,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: `fp_${id}`,
      intent: { kind: IntentKind.UPGRADE, resourceId: service.id, image, fromImage: "" },
      servers: [
        {
          serverId: SERVER,
          serverName: "hz-1",
          downtime: durationFromMs(2_000),
          steps: [
            {
              resourceId: `vol_${service.resource?.name}`,
              resource: { kind: "volume", scope: ENV, name: "data" },
              action: PlanAction.UPDATE,
              backupFirst: { method: BackupMethod.DUMP },
            },
            {
              resourceId: service.id,
              resource: { kind: "service", scope: ENV, name: service.resource?.name ?? "" },
              action: PlanAction.REPLACE,
              downtime: durationFromMs(2_000),
            },
          ],
        },
      ],
    },
  });
}

/** A watch of each change by its ID, pushed to one at a time. */
function watches() {
  const feeds = new Map<string, ReturnType<typeof feed<WatchChangeResponse>>>();
  const of = (id: string) => {
    let f = feeds.get(id);
    if (!f) {
      f = feed<WatchChangeResponse>();
      feeds.set(id, f);
    }
    return f;
  };
  return {
    push: (change: Change) => of(change.id).push(watched(change)),
    stream: (req: { id: string }, ctx: HandlerContext) => of(req.id).stream(req, ctx),
  };
}

function api(services: ServiceInfo[], extra: Api = {}): Api {
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

describe("updating several apps at once", () => {
  it("picks the patch updates, plans each as its own change, applies them together, and follows each", async () => {
    const user = userEvent.setup();
    const db = withUpdate("db", "postgres:16.3", "16.4", UpdateKind.PATCH);
    const cache = withUpdate("cache", "redis:7.2.4", "7.2.5", UpdateKind.PATCH);
    const ghost = withUpdate("blog", "ghost:5.96", "6.0", UpdateKind.MAJOR);
    const asked: string[] = [];
    const applies: { id: string; fingerprint: string }[] = [];
    const w = watches();
    const plans = new Map<string, Change>();
    renderAt(
      "/apps",
      api([db, cache, ghost], {
        services: {
          listServices: () => ({ services: [db, cache, ghost] }),
          upgradeService: (req) => {
            asked.push(`${req.id} ${req.image} ${req.skipBackup}`);
            const service = [db, cache, ghost].find((s) => s.id === req.id) as ServiceInfo;
            const change = planOf(service, req.image);
            plans.set(change.id, change);
            return { change };
          },
        },
        changes: {
          watchChange: w.stream,
          discardChange: () => ({}),
          applyChange: (req) => {
            applies.push({ id: req.id, fingerprint: req.fingerprint });
            return { change: moved(plans.get(req.id) as Change, { state: ChangeState.APPLYING }) };
          },
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Apps" });
    expect(within(table).getByRole("checkbox", { name: "Select blog's update" })).not.toBeChecked();
    await user.click(screen.getByRole("button", { name: "Select 2 patch updates" }));
    expect(within(table).getByRole("checkbox", { name: "Select db's update" })).toBeChecked();
    expect(within(table).getByRole("checkbox", { name: "Select cache's update" })).toBeChecked();
    const bar = screen.getByRole("region", { name: "Selected updates" });
    expect(bar).toHaveTextContent("2 selected");
    await user.click(within(bar).getByRole("button", { name: "Update 2 selected" }));

    const sheet = await screen.findByRole("dialog", { name: "Update 2 apps" });
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Apply 2 updates" })).toBeEnabled());
    expect(asked.sort()).toEqual(["svc_cache redis:7.2.5 false", "svc_db postgres:16.4 false"]);
    const rows = within(sheet).getByRole("list", { name: "Updates" });
    expect(within(rows).getAllByText("Backs up first")).toHaveLength(2);
    expect(rows).toHaveTextContent("16.3 → 16.4");
    await expectAccessible(document.body);
    await user.click(within(sheet).getByRole("button", { name: "Apply 2 updates" }));
    await waitFor(() => expect(applies).toHaveLength(2));
    expect(applies.map((a) => a.fingerprint).sort()).toEqual(["fp_chg_up_cache", "fp_chg_up_db"]);
    // Until each change has ended, the sheet says they're updating.
    const updating = await screen.findByRole("dialog", { name: "Updating 2 apps" });
    expect(within(updating).getAllByText("Applying")).toHaveLength(2);

    // Each follows its own change: one lands, the other fails and rolls back alone.
    act(() => {
      w.push(moved(plans.get("chg_up_db") as Change, { state: ChangeState.APPLIED }));
      w.push(
        moved(plans.get("chg_up_cache") as Change, {
          state: ChangeState.FAILED,
          error: "cache isn't healthy: it exited with code 1",
          rolledBackBy: "chg_rb",
        }),
      );
    });
    const done = await screen.findByRole("dialog", { name: "Updates finished" });
    const finished = within(done).getByRole("list", { name: "Updates" });
    expect(within(finished).getByText("Updated")).toBeVisible();
    expect(finished).toHaveTextContent("Failed, and rolled back: cache isn't healthy: it exited with code 1");
    await user.click(within(done).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("takes a catalog install's new build alone, never the release beside it", async () => {
    const user = userEvent.setup();
    const board = anApp(
      "board",
      "nginx:latest",
      {},
      {
        update: create(ServiceUpdateSchema, {
          check: UpdateCheck.AVAILABLE,
          source: UpdateSource.CATALOG,
          available: [
            {
              kind: UpdateKind.DIGEST,
              toDigest: "sha256:bbbb",
              toImage: "nginx:latest",
              catalogRelease: 6,
              skipAs: "sha256:bbbb",
            },
          ],
        }),
      },
    );
    const asked: { rebuild: boolean; image: string }[] = [];
    renderAt(
      "/apps",
      api([board], {
        services: {
          listServices: () => ({ services: [board] }),
          upgradeService: (req) => {
            asked.push({ rebuild: req.rebuild, image: req.image });
            return { change: planOf(board, "nginx:latest") };
          },
        },
        changes: { watchChange: watches().stream, discardChange: () => ({}) },
      }),
    );
    const table = await screen.findByRole("table", { name: "Apps" });
    await user.click(within(table).getByRole("checkbox", { name: "Select board's update" }));
    await user.click(
      within(screen.getByRole("region", { name: "Selected updates" })).getByRole("button", {
        name: /^Update 1 selected/,
      }),
    );
    await waitFor(() => expect(asked).toEqual([{ rebuild: true, image: "" }]));
  });

  it("says what can't be planned, applies only what can, and discards what it planned when cancelled", async () => {
    const user = userEvent.setup();
    const db = withUpdate("db", "postgres:16.3", "16.4", UpdateKind.PATCH);
    const cache = withUpdate("cache", "redis:7.2.4", "7.2.5", UpdateKind.PATCH);
    const discarded: string[] = [];
    renderAt(
      "/apps",
      api([db, cache], {
        services: {
          listServices: () => ({ services: [db, cache] }),
          upgradeService: (req) => {
            if (req.id === "svc_cache")
              throw new ConnectError("cache isn't on a server that's connected", Code.FailedPrecondition);
            return { change: planOf(db, req.image) };
          },
        },
        changes: {
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Select 2 patch updates" }));
    await user.click(screen.getByRole("button", { name: "Update 2 selected" }));
    const sheet = await screen.findByRole("dialog", { name: "Update 2 apps" });
    expect(await within(sheet).findByText(/cache isn't on a server that's connected/)).toBeVisible();
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Apply 1 update" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(discarded).toEqual(["chg_up_db"]));
  });

  it("plans again what another update changed the server under, and applies it where it's the same update", async () => {
    const user = userEvent.setup();
    const a = withUpdate("cache-a", "redis:7.2", "7.4", UpdateKind.MINOR);
    const b = withUpdate("cache-b", "redis:7.2", "7.4", UpdateKind.MINOR);
    let planned = 0;
    const applied: string[] = [];
    const discarded: string[] = [];
    renderAt(
      "/apps",
      api([a, b], {
        services: {
          listServices: () => ({ services: [a, b] }),
          upgradeService: (req) => {
            planned += 1;
            const service = req.id === a.id ? a : b;
            const change = planOf(service, req.image);
            // The second plan of cache-b is a new change.
            if (req.id === b.id && planned > 2) change.id = "chg_up_cache-b_again";
            return { change };
          },
        },
        changes: {
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
          applyChange: (req) => {
            // cache-a's pull of redis:7.4 changed the server under cache-b's first plan.
            if (req.id === "chg_up_cache-b") {
              throw new ConnectError("the servers changed since this plan was made", Code.FailedPrecondition, {
                "dockwell-error": "plan_changed",
              });
            }
            applied.push(req.id);
            return { change: create(ChangeSchema, { id: req.id, state: ChangeState.APPLYING }) };
          },
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Apps" });
    await user.click(within(table).getByRole("checkbox", { name: "Select every update shown" }));
    await user.click(screen.getByRole("button", { name: "Update 2 selected" }));
    const sheet = await screen.findByRole("dialog", { name: "Update 2 apps" });
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Apply 2 updates" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Apply 2 updates" }));
    await waitFor(() => expect(applied.sort()).toEqual(["chg_up_cache-a", "chg_up_cache-b_again"]));
    expect(discarded).toEqual(["chg_up_cache-b"]);
  });

  it("drops the updates filter once nothing is left to show, and says nothing of updates at 0", async () => {
    const user = userEvent.setup();
    const db = withUpdate("db", "postgres:16.3", "16.4", UpdateKind.PATCH);
    const web = anApp("web", "nginx:1.27");
    const updated = anApp("db", "postgres:16.4");
    let services = [db, web];
    const w = watches();
    const plan = planOf(db, "postgres:16.4");
    const { router } = renderAt(
      "/apps?show=updates",
      api([], {
        services: {
          listServices: () => ({ services }),
          upgradeService: () => ({ change: plan }),
        },
        changes: {
          watchChange: w.stream,
          discardChange: () => ({}),
          applyChange: () => {
            services = [updated, web];
            return { change: moved(plan, { state: ChangeState.APPLYING }) };
          },
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Apps" });
    expect(within(table).queryByRole("row", { name: /web/ })).toBeNull();
    await user.click(within(table).getByRole("checkbox", { name: "Select db's update" }));
    await user.click(screen.getByRole("button", { name: "Update 1 selected" }));
    const sheet = await screen.findByRole("dialog", { name: "Update 1 app" });
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Apply 1 update" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Apply 1 update" }));
    act(() => w.push(moved(plan, { state: ChangeState.APPLIED })));
    await user.click(await screen.findByRole("button", { name: "Done" }));
    // The list comes back whole, with no filter that shows nothing, and no count of none.
    const whole = await screen.findByRole("table", { name: "Apps" });
    await waitFor(() => expect(within(whole).getByRole("row", { name: /web/ })).toBeVisible());
    expect(within(whole).getByRole("row", { name: /db/ })).toBeVisible();
    expect(router.state.location.search).not.toHaveProperty("show");
    expect(screen.queryByRole("button", { name: /updates? available/ })).toBeNull();
    expect(screen.queryByText("No apps here")).toBeNull();

    await router.navigate({ to: "/" });
    await screen.findByRole("heading", { name: "Apps" });
    expect(screen.queryByRole("link", { name: /updates? · Review/ })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Updates" })).toBeNull();
  });

  it("lists the apps with an update on the Overview, each with a box to pick it by", async () => {
    const user = userEvent.setup();
    const db = withUpdate("db", "postgres:16.3", "16.4", UpdateKind.PATCH);
    renderAt(
      "/",
      api([db], {
        services: {
          listServices: () => ({ services: [db] }),
          upgradeService: (req) => ({ change: planOf(db, req.image) }),
        },
        changes: { discardChange: () => ({}) },
      }),
    );
    const panel = (await screen.findByRole("heading", { name: "Updates" })).closest("section") as HTMLElement;
    const list = within(panel).getByRole("list", { name: "Apps with an update" });
    expect(list).toHaveTextContent("16.3 → 16.4");
    await user.click(within(list).getByRole("checkbox", { name: "Select db's update" }));
    await user.click(within(panel).getByRole("button", { name: "Update 1 selected" }));
    expect(await screen.findByRole("dialog", { name: "Update 1 app" })).toBeVisible();
  });
});
