// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  AlertDeliverySchema,
  AlertSchema,
  AlertState,
  AlertRule_Condition as Condition,
  EventKind,
  EventSchema,
  AlertRule_Severity as Severity,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { applied, changeEvent, webPlanned } from "../../test/changes";
import { type Api, onPhone, renderAt } from "../../test/render";
import { messagesAbout } from "./AlertPage";

const NOW = Date.now();
const ago = (minutes: number) => timestampFromDate(new Date(NOW - minutes * 60_000));

/** web's check has failed for 20 minutes on hz-1, and Leo is on it. */
const down = (more: object = {}) =>
  create(AlertSchema, {
    id: "alt_1",
    ruleId: "alr_1",
    ruleName: "app-down",
    severity: Severity.CRITICAL,
    state: AlertState.FIRING,
    condition: Condition.SERVICE_DOWN,
    title: "web fails its health check on hz-1",
    detail: "Its visitors get an error there.",
    subjectId: "svc_web",
    subjectName: "web",
    serverId: "srv_1",
    serverName: "hz-1",
    variant: "unhealthy",
    openTime: ago(20),
    sentTime: ago(19),
    acknowledgeTime: ago(5),
    acknowledgedBy: { kind: ActorKind.USER, id: "usr_leo", name: "Leo Park" },
    ...more,
  });

const alertEvent = (id: string, action: string, minutes: number, alert = "alt_1") =>
  create(EventSchema, {
    id,
    action,
    kind: EventKind.ALERT,
    time: ago(minutes),
    actor: { kind: ActorKind.USER, id: "usr_leo", name: "Leo Park" },
    alert: {
      id: alert,
      rule: "app-down",
      condition: Condition.SERVICE_DOWN,
      subjectName: "web",
      serverName: "hz-1",
      variant: "unhealthy",
      severity: Severity.CRITICAL,
    },
  });

/** A deploy of web 30 minutes ago, before it fired, and another 10 minutes ago, since. */
const deploy = (id: string, summary: string, minutes: number) => {
  const at = new Date(NOW - minutes * 60_000);
  return changeEvent(applied({ ...webPlanned(), id, summary }, at), { id, time: timestampFromDate(at) });
};

const delivery = (about: string, minutes: number, ok = true, channel = "ops-email") =>
  create(AlertDeliverySchema, { time: ago(minutes), ok, said: ok ? "250 OK" : "no such host", about, channel });

function api(alerts: ReturnType<typeof down>[], more: Api = {}): Api {
  return {
    ...more,
    alerts: {
      listAlerts: () => ({ firing: alerts.filter((a) => a.state === AlertState.FIRING).length, alerts }),
      listAlertRules: () => ({
        rules: [{ id: "alr_1", name: "app-down", title: "App down", expression: "service.health != healthy" }],
      }),
      listAlertChannels: () => ({ channels: [] }),
      listAlertDeliveries: () => ({
        deliveries: [
          delivery("web fails its health check on hz-1", 19),
          delivery("web fails its health check on hz-1", 19, false, "ops-hook"),
          // Another alert's, and this one's title from a day ago, which aren't about this one.
          delivery("api fails its health check on hz-1", 18),
          delivery("web fails its health check on hz-1", 60 * 24),
        ],
      }),
      ...more.alerts,
    },
    events: {
      listEvents: () => ({
        events: [
          alertEvent("evt_ack", "alert.acknowledge", 5),
          deploy("chg_fix", "Roll web back to 1.4", 10),
          alertEvent("evt_fired", "alert.firing", 20),
          alertEvent("evt_other", "alert.firing", 25, "alt_other"),
          deploy("chg_bad", "Deploy web 1.5", 30),
          deploy("chg_old", "Deploy web 1.4", 300),
        ],
      }),
      ...more.events,
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("an alert's page", () => {
  it("takes an open alert from its page's head, as from its card", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    renderAt(
      "/alerts/alt_1",
      api([down({ acknowledgeTime: undefined, acknowledgedBy: undefined })], {
        alerts: {
          acknowledgeAlert: (req: { id: string }) => {
            asked.push(req.id);
            return {};
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Acknowledge" }));
    await waitFor(() => expect(asked).toEqual(["alt_1"]));
  });

  it("offers to take it on a phone too, under who has it, as M-Incident has it", async () => {
    onPhone();
    const asked: string[] = [];
    renderAt(
      "/alerts/alt_1",
      api([down({ acknowledgeTime: undefined, acknowledgedBy: undefined })], {
        alerts: {
          acknowledgeAlert: (req: { id: string }) => {
            asked.push(req.id);
            return {};
          },
        },
      }),
    );
    const take = await screen.findByRole("button", { name: "Acknowledge" });
    expect(take.closest(".alert-phone-acknowledge")).not.toBeNull();
    await userEvent.setup().click(take);
    await waitFor(() => expect(asked).toEqual(["alt_1"]));
  });

  it("offers to take over an alert someone else has, from its page", async () => {
    renderAt("/alerts/alt_1", api([down()]));
    expect(await screen.findByRole("button", { name: "Take over" })).toHaveAttribute(
      "title",
      "Take over from Leo Park",
    );
  });

  it("offers nothing to take on a resolved alert", async () => {
    renderAt("/alerts/alt_1", api([down({ state: AlertState.RESOLVED, resolveTime: ago(2) })]));
    await screen.findByRole("heading", { level: 1 });
    expect(screen.queryByRole("button", { name: "Acknowledge" })).toBeNull();
  });

  it("says what fired, what its rule saw, who has it, and leads back", async () => {
    const { container } = renderAt("/alerts/alt_1", api([down()]));
    expect(await screen.findByRole("heading", { level: 1, name: "web fails its health check on hz-1" })).toBeVisible();
    // Above the title, apart from the rail's.
    expect(container.querySelector("a.alerts-back")).toHaveAttribute("href", "/alerts");
    const saw = screen.getByRole("region", { name: "What the rule saw" });
    expect(saw).toHaveTextContent("Its visitors get an error there.");
    expect(saw).toHaveTextContent("Open for20 min");
    // The rule's own words and expression, once the rules are read.
    await waitFor(() => expect(saw).toHaveTextContent("RuleApp down · service.health != healthy"));
    expect(saw).toHaveTextContent(/AcknowledgedLeo Park · /);
    expect(saw).toHaveTextContent(/SendingSent /);
    // What it's about, each a way to its own page.
    const lead = container.querySelector(".page-lead") as HTMLElement;
    expect(within(lead).getByRole("link", { name: "web" })).toHaveAttribute("href", "/apps/svc_web");
    expect(within(lead).getByRole("link", { name: "hz-1" })).toHaveAttribute("href", "/servers/srv_1");
    await expectAccessible(container);
  });

  it("lines up what changed in the hour before it fired, and since, from what Activity keeps of its app", async () => {
    const listEvents = vi.fn(api([]).events?.listEvents as () => object);
    renderAt("/alerts/alt_1", api([down()], { events: { listEvents } }));
    const before = await screen.findByRole("region", { name: "What changed before it fired" });
    await within(before).findByText("Deploy web 1.5");
    // Not the deploy of five hours ago, nor the one made since.
    expect(before).not.toHaveTextContent("Deploy web 1.4");
    expect(before).not.toHaveTextContent("Roll web back");
    const since = screen.getByRole("region", { name: "What has changed since" });
    expect(since).toHaveTextContent("Roll web back to 1.4");
    // Read by the app the alert is about, with its changes, its health and alerts.
    expect((listEvents.mock.calls as unknown as [object][])[0]?.[0]).toMatchObject({
      resourceId: "svc_web",
      serverId: "",
      kinds: [EventKind.CHANGE, EventKind.HEALTH, EventKind.ALERT],
    });
  });

  it("tells the alert's own story beside them, and none of another alert's", async () => {
    renderAt("/alerts/alt_1", api([down()]));
    const own = await screen.findByRole("region", { name: "This alert" });
    await within(own).findByText("Acknowledged the alert: web fails its health check on hz-1");
    expect(within(own).getByText("Alert opened: web fails its health check on hz-1")).toBeVisible();
    expect(within(own).getAllByText(/web fails its health check on hz-1/)).toHaveLength(2);
  });

  it("lists the messages about it, by what they said and when, and how each went", async () => {
    renderAt("/alerts/alt_1", api([down()]));
    const sent = await screen.findByRole("region", { name: "Messages about it" });
    await within(sent).findByText("ops-email");
    const rows = within(sent).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("ops-emailDelivered");
    expect(rows[1]).toHaveTextContent("ops-hookFailed");
    expect(rows[1]).toHaveTextContent("no such host");
    expect(sent).toHaveTextContent("2 sent");
  });

  it("says how it ended once it resolved, and how long it lasted", async () => {
    renderAt(
      "/alerts/alt_1",
      api([
        down({
          state: AlertState.RESOLVED,
          resolveTime: ago(2),
          acknowledgeTime: undefined,
          acknowledgedBy: undefined,
        }),
      ]),
    );
    const saw = await screen.findByRole("region", { name: "What the rule saw" });
    expect(saw).toHaveTextContent("Lasted18 min");
    expect(saw).toHaveTextContent("AcknowledgedNobody did");
    expect(screen.getByText(/^It resolved .*, after 18 min\.$/)).toBeVisible();
    expect(await screen.findByRole("region", { name: "What changed while it was open" })).toHaveTextContent(
      "Roll web back to 1.4",
    );
  });

  it("says when nothing changed before it, and when it couldn't read back far enough to tell", async () => {
    const quiet = renderAt("/alerts/alt_1", api([down()], { events: { listEvents: () => ({ events: [] }) } }));
    expect(
      await screen.findByText("Nothing changed here in the hour before, and no check changed its answer."),
    ).toBeVisible();
    quiet.unmount();

    // Five full pages, all newer than the hour before: there may be more behind them.
    const listEvents = vi.fn(() => ({ events: [deploy("chg_new", "Deploy web 1.6", 1)], nextPageToken: "more" }));
    renderAt("/alerts/alt_1", api([down()], { events: { listEvents } }));
    expect(await screen.findByText("It's further back than this page reads.")).toBeVisible();
    // Activity has the rest, for what the alert is about.
    expect(screen.getByRole("link", { name: "See it on Activity" })).toHaveAttribute(
      "href",
      expect.stringMatching(/^\/activity\?(resource|server)=/),
    );
    await waitFor(() => expect(listEvents).toHaveBeenCalledTimes(5));
  });

  it("says an alert that isn't among the newest isn't, as there's no read of one by its ID", async () => {
    renderAt("/alerts/alt_gone", api([down()]));
    expect(await screen.findByRole("heading", { level: 2, name: "This alert isn't among the newest" })).toBeVisible();
    expect(screen.queryByRole("region", { name: "What the rule saw" })).toBeNull();
  });

  it("goes back with the header's arrow on a phone", async () => {
    onPhone();
    renderAt("/alerts/alt_1", api([down()]));
    await screen.findByRole("heading", { level: 1, name: "web fails its health check on hz-1" });
    const back = screen.getByRole("link", { name: "Back to alerts" });
    expect(back).toHaveClass("mobile-header-button");
    expect(back).toHaveAttribute("href", "/alerts");
  });

  it("reads a server's alert by its server", async () => {
    const listEvents = vi.fn(() => ({ events: [] }));
    renderAt(
      "/alerts/alt_1",
      api([down({ condition: Condition.SERVER_UNREACHABLE, subjectId: "", subjectName: "", variant: "" })], {
        events: { listEvents },
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "hz-1 can't be reached" });
    await waitFor(() => expect(listEvents).toHaveBeenCalled());
    expect((listEvents.mock.calls as unknown as [object][])[0]?.[0]).toMatchObject({
      resourceId: "",
      serverId: "srv_1",
    });
  });
});

describe("messagesAbout", () => {
  it("takes an alert's firing and resolved messages, and no other time's or alert's", () => {
    const alert = down({ state: AlertState.RESOLVED, resolveTime: ago(2) });
    const got = messagesAbout(alert, [
      delivery("web fails its health check on hz-1", 19),
      delivery("Resolved: web fails its health check on hz-1", 2),
      delivery("api fails its health check on hz-1", 19),
      delivery("web fails its health check on hz-1", 21),
      delivery("Test message", 10),
    ]);
    expect(got.map((d) => d.about)).toEqual([
      "web fails its health check on hz-1",
      "Resolved: web fails its health check on hz-1",
    ]);
  });
});
