// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  ChangeState,
  EventKind,
  type ListEventsRequest,
  ResourceSchema,
  StepProgressSchema,
  StepState,
  Surface,
  type UndoChangeRequest,
  type WatchChangeResponse,
  type WatchEventsRequest,
  type WatchEventsResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { applied, auditEvent, changeEvent, moved, shopPlanned, since, snapshot, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt, untilAborted } from "../../test/render";

const HOUR = 3_600_000;
const shop = applied(shopPlanned(), new Date(Date.now() - 2 * 60_000));
const shopEvent = changeEvent(shop);
const signIn = auditEvent({
  id: "evt_sign_in",
  action: "auth.sign_in",
  time: timestampFromDate(new Date(Date.now() - HOUR)),
});
const joined = auditEvent({
  id: "evt_join",
  kind: EventKind.SERVER,
  action: "server.join",
  time: timestampFromDate(new Date(Date.now() - 26 * HOUR)),
  actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
  surface: Surface.AUTOMATIC,
  subject: { id: "srv_1", name: "hz-1" },
});
const token = auditEvent({
  id: "evt_token",
  action: "token.create",
  time: timestampFromDate(new Date(Date.now() - 27 * HOUR)),
  subject: { id: "tok_1", name: "GitHub Actions deploy" },
});

function renderActivity(watch: ReturnType<typeof feed<WatchEventsResponse>>, api: Api = {}, path = "/activity") {
  return renderAt(path, { ...api, events: { watchEvents: watch.stream, ...api.events } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Activity", () => {
  it("lists the timeline by day, newest first, and follows it live", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn, joined, token]));
    renderActivity(watch);

    expect(await screen.findByRole("heading", { level: 1, name: "Activity" })).toBeVisible();
    const today = await screen.findByRole("region", { name: "Today" });
    const rows = within(within(today).getByRole("table")).getAllByRole("row");
    expect(rows.map((r) => r.textContent)).toEqual([
      "EventByWhen",
      expect.stringMatching(/^Create the project shop2 to createMCMira Chen2 min ago/),
      expect.stringMatching(/^Signed inFrom the panelMCMira Chen1 h ago/),
    ]);
    const yesterday = screen.getByRole("region", { name: "Yesterday" });
    expect(yesterday).toHaveTextContent("hz-1 joined as the control server");
    expect(yesterday).toHaveTextContent("Created the API token GitHub Actions deploy");
    expect(within(yesterday).getByText(/· 2 events$/)).toBeVisible();
    await expectAccessible(document.body);

    // A new plan arrives, then goes when it is discarded.
    const planned = changeEvent(
      moved(shopPlanned(), {
        id: "chg_qa",
        summary: "Create the environment qa",
        createTime: timestampFromDate(new Date()),
      }),
    );
    act(() => watch.push(since([planned])));
    const row = await screen.findByRole("row", { name: /Create the environment qa/ });
    expect(row).toHaveTextContent("Planned");
    act(() => watch.push(since([], ["chg_qa"])));
    await waitFor(() => expect(screen.queryByRole("row", { name: /Create the environment qa/ })).toBeNull());
  });

  it("opens a change beside the list, with its plan, and undoes it with a plan of its own", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    const undos: UndoChangeRequest[] = [];
    const undo = moved(shopPlanned(), {
      id: "chg_undo",
      summary: "Undo Create the project shop",
      reverts: "chg_shop",
    });
    if (undo.plan?.summary) undo.plan.summary.create = 0;
    if (undo.plan?.summary) undo.plan.summary.destroy = 2;
    for (const r of undo.plan?.resources ?? []) r.action = 5;
    const { router } = renderActivity(watch, {
      changes: {
        getChange: () => ({ change: shop }),
        undoChange: (req) => {
          undos.push(req);
          return { change: undo };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    await user.click(await screen.findByRole("row", { name: /Create the project shop/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ event: "chg_shop" }));
    const detail = await screen.findByRole("region", { name: "Create the project shop" });
    expect(detail).toHaveTextContent("Mira Chen");
    expect(detail).toHaveTextContent("The panel");
    expect(detail).toHaveTextContent("3f9a1c2b");
    expect(await within(detail).findByText("production")).toBeVisible();
    await expectAccessible(document.body);

    await user.click(within(detail).getByRole("button", { name: "Undo this change" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(undos).toEqual([expect.objectContaining({ id: "chg_shop" })]);
    expect(await within(sheet).findByText("Undo Create the project shop")).toBeVisible();
    expect(sheet).toHaveTextContent("2 to destroy");
  });

  it("filters by kind and actor, in its address", async () => {
    const user = userEvent.setup();
    const requests: WatchEventsRequest[] = [];
    const { router } = renderAt("/activity", {
      events: {
        async *watchEvents(req, ctx) {
          requests.push(req);
          yield snapshot([shopEvent, signIn]);
          await new Promise((resolve) => ctx.signal.addEventListener("abort", resolve));
        },
      },
    });
    await screen.findByRole("row", { name: /Signed in/ });

    await user.click(screen.getByRole("radio", { name: "Changes" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ kind: "changes" }));
    await waitFor(() => expect(requests.at(-1)?.kinds).toEqual([EventKind.CHANGE]));

    await user.selectOptions(screen.getByRole("combobox", { name: "Actor" }), "usr_1");
    await waitFor(() => expect(requests.at(-1)?.actorId).toBe("usr_1"));
    await user.selectOptions(screen.getByRole("combobox", { name: "Actor" }), "dockwell");
    await waitFor(() => expect(requests.at(-1)?.actorKind).toBe(ActorKind.DOCKWELL));
    expect(requests.at(-1)?.actorId).toBe("");
  });

  it("narrows to a server from its address, and clears it", async () => {
    const user = userEvent.setup();
    const requests: WatchEventsRequest[] = [];
    const { router } = renderAt("/activity?server=srv_1", {
      events: {
        async *watchEvents(req, ctx) {
          requests.push(req);
          yield snapshot([joined]);
          await new Promise((resolve) => ctx.signal.addEventListener("abort", resolve));
        },
      },
    });
    await screen.findByRole("row", { name: /hz-1 joined/ });
    expect(requests[0]?.serverId).toBe("srv_1");
    expect(screen.getByText("hz-1", { selector: ".dw-tag-value" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Remove the server filter" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    await waitFor(() => expect(requests.at(-1)?.serverId).toBe(""));
  });

  it("shows earlier events from the page after the snapshot", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent], "page-2"));
    const pages: ListEventsRequest[] = [];
    renderActivity(watch, {
      events: {
        listEvents: (req) => {
          pages.push(req);
          return { events: [token] };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    expect(await screen.findByRole("row", { name: /Created the API token/ })).toBeVisible();
    expect(pages).toEqual([expect.objectContaining({ pageToken: "page-2" })]);
    expect(screen.queryByRole("button", { name: "Show earlier events" })).toBeNull();
  });

  it("leaves out an earlier page asked for under the filter before", async () => {
    const user = userEvent.setup();
    let answer: () => void = () => {};
    renderAt("/activity", {
      events: {
        async *watchEvents(req, ctx) {
          yield req.kinds.length === 0 ? snapshot([shopEvent], "page-2") : snapshot([shopEvent], "changes-2");
          await untilAborted(ctx.signal);
        },
        listEvents: async (req) => {
          if (req.pageToken === "page-2") await new Promise<void>((resolve) => (answer = resolve));
          return { events: req.pageToken === "page-2" ? [token] : [], nextPageToken: "" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    await user.click(screen.getByRole("radio", { name: "Changes" }));
    await screen.findByRole("row", { name: /Create the project shop/ });
    // The page for every kind arrives once the list shows only changes.
    act(() => answer());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("row", { name: /Created the API token/ })).toBeNull();
    // The changes carry on from their own snapshot.
    expect(screen.getByRole("button", { name: "Show earlier events" })).toBeEnabled();
  });

  it("keeps the earlier pages it loaded across a reconnect, and pages on below them", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    const user = userEvent.setup();
    const first = feed<WatchEventsResponse>();
    const second = feed<WatchEventsResponse>();
    first.push(snapshot([shopEvent], "page-2"));
    const watches = [first, second];
    const pages: string[] = [];
    renderAt("/activity", {
      events: {
        watchEvents: (req, ctx) => (watches.shift() ?? second).stream(req, ctx),
        listEvents: (req) => {
          pages.push(req.pageToken);
          return req.pageToken === "page-2" ? { events: [token], nextPageToken: "page-3" } : { events: [] };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    await screen.findByRole("row", { name: /Created the API token/ });

    // The watch drops, and its new snapshot reaches back to what was shown.
    const newer = auditEvent({ id: "evt_new", action: "auth.sign_in" });
    second.push(snapshot([newer, shopEvent], "page-2-again"));
    act(() => first.fail(new ConnectError("the control plane restarted", Code.Unavailable)));
    await waitFor(() => expect(screen.getAllByRole("row", { name: /Signed in/ })).toHaveLength(1), { timeout: 3000 });
    expect(screen.getByRole("row", { name: /Created the API token/ })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show earlier events" }));
    await waitFor(() => expect(pages).toEqual(["page-2", "page-3"]));
  });

  it("counts only the steps that ran to the end", async () => {
    const watch = feed<WatchEventsResponse>();
    const steps = [StepState.DONE, StepState.FAILED, StepState.SKIPPED].map((state, i) =>
      create(StepProgressSchema, {
        serverId: "srv_1",
        resourceId: `res_${i}`,
        state,
        finishTime: timestampFromDate(new Date()),
      }),
    );
    watch.push(snapshot([changeEvent(moved(shopPlanned(), { state: ChangeState.APPLYING, steps }))]));
    renderActivity(watch);
    expect(await screen.findByRole("row", { name: /Create the project shop/ })).toHaveTextContent("1 of 3 steps done");
  });

  it("names the resource it narrows to, asking only for its kind", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent]));
    const kinds: string[] = [];
    renderActivity(
      watch,
      {
        resources: {
          listResources: (req) => {
            kinds.push(req.kind);
            return {
              resources: [create(ResourceSchema, { id: "prj_shop", resource: { kind: "project", name: "shop" } })],
            };
          },
        },
      },
      "/activity?resource=prj_shop",
    );
    expect(await screen.findByText("shop", { selector: ".dw-tag-value" })).toBeVisible();
    expect(kinds).toEqual(["project"]);
  });

  it("opens a change's own page, /changes/<id>, over Activity, to review and apply it", async () => {
    const user = userEvent.setup();
    // Proposed with the CLI, which sends a person here for a destructive plan.
    const destructive = moved(webPlanned(), { proposedVia: Surface.CLI });
    if (destructive.plan) destructive.plan.destructive = true;
    const applies: string[] = [];
    const { router } = renderAt("/changes/chg_web", {
      events: {
        async *watchEvents(_req, ctx) {
          yield snapshot([changeEvent(destructive)]);
          await untilAborted(ctx.signal);
        },
      },
      changes: {
        getChange: () => ({ change: destructive }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req.fingerprint);
          return { change: moved(destructive, { state: ChangeState.APPLYING }) };
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(apply).toBeDisabled();
    await user.type(within(sheet).getByLabelText("Type a1b2c3d4 to apply it"), "a1b2c3d4");
    await user.click(apply);
    await waitFor(() => expect(applies).toEqual(["a1b2c3d4e5f60718293a4b5c6d7e8f90"]));
    expect(screen.getByRole("link", { name: "Activity" })).toHaveAttribute("aria-current", "page");

    const applying = await screen.findByRole("dialog", { name: "Applying" });
    await user.click(within(applying).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    await waitFor(() => expect(router.state.location.pathname).toBe("/activity"));
    expect(router.state.location.search).toEqual({ event: "chg_web" });
    // Beside the list, the change says where it came from.
    expect(await screen.findByRole("region", { name: "Update web" })).toHaveTextContent("The CLI");
  });

  it("says when nothing has happened", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([]));
    renderActivity(watch);
    expect(await screen.findByText("Nothing here yet")).toBeVisible();
    expect(screen.getByText("Changes, servers joining and sign-ins show up here as they happen.")).toBeVisible();
  });

  it("is a line of time on a phone, where a change opens its plan and undo is one tap", async () => {
    onPhone();
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    const undos: string[] = [];
    renderActivity(watch, {
      changes: {
        getChange: () => ({ change: shop }),
        undoChange: (req) => {
          undos.push(req.id);
          throw new Error("not this time");
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    const today = await screen.findByRole("region", { name: "Today" });
    expect(within(today).queryByRole("table")).toBeNull();
    const items = within(today).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Create the project shop");
    expect(items[0]).toHaveTextContent("Mira Chen2 to create");
    await expectAccessible(document.body);

    await user.click(within(items[0] as HTMLElement).getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(undos).toEqual(["chg_shop"]));
    const failed = await screen.findByRole("dialog", { name: "The plan didn't work" });
    await user.click(within(failed).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);

    await user.click(within(items[0] as HTMLElement).getByRole("button", { name: "Create the project shop" }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
  });

  it("reads in Chinese", async () => {
    await activateLocale("zh-CN");
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    renderActivity(watch);
    expect(await screen.findByRole("heading", { level: 1, name: "动态" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "今天" })).toBeVisible();
    expect(screen.getByRole("row", { name: /登录了/ })).toHaveTextContent("来自面板");
    // A change's summary is its author's words, and stays as written.
    expect(screen.getByRole("row", { name: /Create the project shop/ })).toHaveTextContent("2 项创建");
    expect(screen.getByRole("radiogroup", { name: "事件类型" })).toBeVisible();
  });
});
