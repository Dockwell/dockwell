// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  type AlertChannelInfo,
  AlertChannelInfoSchema,
  AlertRuleInfoSchema,
  AlertState,
  ChangeSchema,
  ChangeState,
  AlertRule_Condition as Condition,
  type ProposeChangeRequest,
  Role,
  AlertEmail_Security as Security,
  AlertRule_Severity as Severity,
  type TestAlertChannelRequest,
  TokenScope,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authState, mira } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

const NOW = new Date();

function aChannel(init: MessageInitShape<typeof AlertChannelInfoSchema>): AlertChannelInfo {
  return create(AlertChannelInfoSchema, init);
}

const ops = aChannel({
  id: "ach_ops",
  name: "ops-email",
  type: "email",
  target: "ops@example.com",
  rules: 9,
  channel: {
    to: {
      case: "email",
      value: {
        host: "smtp.example.com",
        port: 587,
        security: Security.STARTTLS,
        username: "alerts",
        from: "alerts@example.com",
        to: ["ops@example.com"],
      },
    },
  },
  sealed: ["email.password"],
  last: {
    time: timestampFromDate(NOW),
    ok: true,
    said: "accepted by smtp.example.com",
    about: "web is down on hz-1",
    channel: "ops-email",
  },
});

const phones = aChannel({
  id: "ach_push",
  name: "phones",
  type: "ntfy",
  target: "ntfy.sh",
  rules: 2,
  channel: { to: { case: "ntfy", value: { server: "https://ntfy.sh" } } },
  sealed: ["ntfy.topic"],
  last: {
    time: timestampFromDate(NOW),
    ok: false,
    said: "403 Forbidden: forbidden",
    about: "Test message",
    channel: "phones",
  },
});

const rules = [
  create(AlertRuleInfoSchema, {
    id: "alr_down",
    name: "app-down",
    title: "App down",
    expression: "app.health == down",
    spec: { condition: Condition.SERVICE_DOWN, severity: Severity.CRITICAL },
  }),
  create(AlertRuleInfoSchema, {
    id: "alr_disk",
    name: "disk-full",
    title: "Disk above 90%",
    expression: "disk.percent > 90",
    spec: { condition: Condition.DISK_FULL, threshold: 90, severity: Severity.WARNING, channels: ["ops-email"] },
  }),
];

function api(channels: AlertChannelInfo[], more: Api = {}): Api {
  return {
    ...more,
    alerts: {
      listAlertChannels: () => ({ channels }),
      listAlertRules: () => ({ rules }),
      listAlerts: () => ({}),
      listAlertDeliveries: () => ({
        deliveries: [
          { time: timestampFromDate(NOW), ok: true, about: "web is down on hz-1", channel: "ops-email" },
          { time: timestampFromDate(NOW), ok: false, about: "web is down on hz-1", channel: "phones", said: "403" },
        ],
      }),
      ...more.alerts,
    },
  };
}

/** A row of the channels' table, as the deliveries below name channels too. */
async function channelRow(name: RegExp) {
  return within(await screen.findByRole("table", { name: "Alert channels" })).getByRole("row", { name });
}

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return { change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }) };
  });
}

describe("Alert channels", () => {
  it("lists each channel, how its last message went and what uses it, and routes each severity", async () => {
    const { container } = renderAt("/alerts/channels", api([ops, phones]));
    const table = await screen.findByRole("table", { name: "Alert channels" });
    const email = within(table).getByRole("row", { name: /ops-email/ });
    expect(email).toHaveTextContent("ops@example.com");
    expect(email).toHaveTextContent("Delivered");
    expect(email).toHaveTextContent("9 rules");
    expect(within(table).getByRole("row", { name: /phones/ })).toHaveTextContent("Failing");
    expect(screen.getByText(/2 channels · 1 failing/)).toBeVisible();
    // One message to two channels is one row, which says where it failed.
    const sent = screen.getByRole("table", { name: "Messages sent lately" });
    expect(within(sent).getAllByRole("row")).toHaveLength(2);
    expect(within(sent).getByRole("row", { name: /web is down on hz-1/ })).toHaveTextContent("Failed to phones");
    // A rule that names no channel reaches every one; disk-full names one.
    const routing = screen.getByRole("region", { name: "Routing" });
    expect(routing).toHaveTextContent(/Critical1 ruleops-emailphones/);
    expect(routing).toHaveTextContent(/Warning1 ruleops-email$|Warning1 ruleops-email[^p]/);
    expect(screen.getByRole("link", { name: /Channels/ })).toHaveAttribute("aria-current", "page");
    await expectAccessible(container);
  });

  it("sends a test by name from the list, and says what the far end answered", async () => {
    const user = userEvent.setup();
    const asked: TestAlertChannelRequest[] = [];
    renderAt(
      "/alerts/channels",
      api([ops, phones], {
        alerts: {
          testAlertChannel: (req) => {
            asked.push(req);
            return { delivery: { ok: false, said: "403 Forbidden: forbidden" } };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Send a test to phones" }));
    expect(await screen.findByText("The test didn't reach phones")).toBeVisible();
    expect(screen.getByText(/It said: 403 Forbidden: forbidden/)).toBeVisible();
    expect(asked).toHaveLength(1);
    expect(asked[0]?.name).toBe("phones");
    expect(asked[0]?.channel).toBeUndefined();
  });

  it("says where alerts can go when there's nowhere yet", async () => {
    const user = userEvent.setup();
    renderAt("/alerts/channels", api([]));
    expect(await screen.findByText("Nowhere to send alerts yet")).toBeVisible();
    expect(screen.getByText(/no channel yet, so nothing is sent/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Add channel" }));
    expect(await screen.findByRole("dialog", { name: "Add a channel" })).toBeVisible();
  });

  it("leaves a channel's Review plan off, with why, for someone who can't plan the install's channels", async () => {
    const user = userEvent.setup();
    renderAt(
      "/alerts/channels",
      api([], {
        auth: {
          getAuthState: () =>
            authState({
              user: { ...mira, role: Role.UNSPECIFIED },
              grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
            }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add channel" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a channel" });
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toHaveAttribute("aria-disabled", "true");
    expect(review).not.toHaveAccessibleDescription("");
  });

  it("adds email through a mail service, which fills in its server, named from its type", async () => {
    const user = userEvent.setup();
    const asked: TestAlertChannelRequest[] = [];
    const proposed = planned();
    renderAt(
      "/alerts/channels",
      api([ops], {
        alerts: {
          testAlertChannel: (req) => {
            asked.push(req);
            return { delivery: { ok: true, said: "accepted by smtp.gmail.com" } };
          },
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add channel" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a channel" });
    expect(within(sheet).getByLabelText("Mail service")).toHaveValue("gmail");
    // The server's settings and the name wait under Advanced, said in a line.
    expect(within(sheet).queryByLabelText("SMTP server")).toBeNull();
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveTextContent(
      "Named email · smtp.gmail.com:587 · STARTTLS",
    );
    const send = within(sheet).getByRole("button", { name: "Send a test" });
    await user.type(within(sheet).getByLabelText("To"), "ops@example.com, oncall@example.com");
    await user.type(within(sheet).getByLabelText("From"), "alerts@example.com");
    await user.type(within(sheet).getByLabelText(/^User/), "alerts@example.com");
    expect(send).toBeDisabled();
    await user.type(within(sheet).getByLabelText(/^Password/), "s3cret");
    await user.click(send);
    expect(await within(sheet).findByText("The test arrived")).toBeVisible();
    expect(asked[0]?.channel?.to).toMatchObject({
      case: "email",
      value: {
        host: "smtp.gmail.com",
        port: 587,
        security: Security.STARTTLS,
        to: ["ops@example.com", "oncall@example.com"],
      },
    });
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.resource).toMatchObject({ kind: "alert.channel", name: "email" });
  });

  it("takes another SMTP server as typed, and a name of one's own under Advanced", async () => {
    const user = userEvent.setup();
    const asked: TestAlertChannelRequest[] = [];
    const proposed = planned();
    renderAt(
      "/alerts/channels",
      api([ops], {
        alerts: {
          testAlertChannel: (req) => {
            asked.push(req);
            return { delivery: { ok: true, said: "accepted by smtp.example.net" } };
          },
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add channel" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a channel" });
    await user.selectOptions(within(sheet).getByLabelText("Mail service"), "other");
    const server = within(sheet).getByLabelText("SMTP server");
    // Another server starts empty, not as the service picked before.
    expect(server).toHaveValue("");
    await user.type(server, "smtp.example.net");
    await user.type(within(sheet).getByLabelText("To"), "ops@example.com");
    await user.type(within(sheet).getByLabelText("From"), "alerts@example.com");
    await user.type(within(sheet).getByLabelText(/^User/), "alerts");
    await user.type(within(sheet).getByLabelText(/^Password/), "s3cret");
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    const name = within(sheet).getByLabelText("Name");
    await user.clear(name);
    await user.type(name, "team-email");
    await user.click(within(sheet).getByRole("button", { name: "Send a test" }));
    expect(await within(sheet).findByText("The test arrived")).toBeVisible();
    expect(asked[0]?.channel?.to).toMatchObject({
      case: "email",
      value: { host: "smtp.example.net", username: "alerts" },
    });
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.resource).toMatchObject({ kind: "alert.channel", name: "team-email" });
  });

  it("changes a channel without asking again for what it keeps sealed", async () => {
    const user = userEvent.setup();
    const asked: TestAlertChannelRequest[] = [];
    const proposed = planned();
    renderAt(
      "/alerts/channels",
      api([ops], {
        alerts: {
          testAlertChannel: (req) => {
            asked.push(req);
            return { delivery: { ok: true, said: "accepted by smtp.example.com" } };
          },
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await channelRow(/ops-email/));
    const sheet = await screen.findByRole("dialog", { name: "Change ops-email" });
    const password = within(sheet).getByLabelText(/^Password/);
    expect(password).toHaveValue("");
    expect(password).toHaveAttribute("placeholder", "Kept, sealed");
    expect(within(sheet).getByLabelText("SMTP server")).toHaveValue("smtp.example.com");
    await user.clear(within(sheet).getByLabelText("To"));
    await user.type(within(sheet).getByLabelText("To"), "team@example.com");
    await user.click(within(sheet).getByRole("button", { name: "Send a test" }));
    expect(await within(sheet).findByText("The test arrived")).toBeVisible();
    expect(asked[0]?.name).toBe("ops-email");
    expect(asked[0]?.channel?.to).toMatchObject({ case: "email", value: { password: "", to: ["team@example.com"] } });
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].summary).toBe("Change the alert channel ops-email");
  });

  it("removes a channel through a plan", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt(
      "/alerts/channels",
      api([ops], { changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream } }),
    );
    await user.click(await channelRow(/ops-email/));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]).toMatchObject({
      resource: { kind: "alert.channel", name: "ops-email" },
      delete: true,
    });
  });

  it("says why a test wasn't sent, from what the control plane refused", async () => {
    const user = userEvent.setup();
    const { ConnectError, Code } = await import("@connectrpc/connect");
    renderAt(
      "/alerts/channels",
      api([ops], {
        alerts: {
          testAlertChannel: () => {
            throw new ConnectError(
              "the email's server, port, security or user changes, so it needs its password again: Dockwell won't send what it kept to where it hasn't sent before",
              Code.InvalidArgument,
            );
          },
        },
      }),
    );
    await user.click(await channelRow(/ops-email/));
    const sheet = await screen.findByRole("dialog", { name: "Change ops-email" });
    const host = within(sheet).getByLabelText("SMTP server");
    await user.clear(host);
    await user.type(host, "smtp.example.net");
    await user.click(within(sheet).getByRole("button", { name: "Send a test" }));
    expect(await within(sheet).findByText("Dockwell didn't send it")).toBeVisible();
    expect(within(sheet).getByText(/needs its password again/)).toBeVisible();
  });

  it("lists channels on a phone, with a test for each", async () => {
    onPhone();
    const user = userEvent.setup();
    const asked: TestAlertChannelRequest[] = [];
    renderAt(
      "/alerts/channels",
      api([ops], {
        alerts: {
          testAlertChannel: (req) => {
            asked.push(req);
            return { delivery: { ok: true, said: "accepted by smtp.example.com" } };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Send a test to ops-email" }));
    expect(await screen.findByText("The test reached ops-email")).toBeVisible();
    expect(screen.queryByRole("table", { name: "Alert channels" })).toBeNull();
    // The header's button adds one, as the page's own is for a wide screen.
    await user.click(screen.getByRole("button", { name: "Add channel" }));
    expect(await screen.findByRole("dialog", { name: "Add a channel" })).toBeVisible();
  });

  it("reads in Chinese too", async () => {
    await activateLocale("zh-CN");
    try {
      renderAt("/alerts/channels", api([ops, phones]));
      expect(await screen.findByRole("table", { name: "告警渠道" })).toBeVisible();
      expect(screen.getByText("最近的发送")).toBeVisible();
      expect(screen.getAllByText("已送达").length).toBeGreaterThan(0);
    } finally {
      await activateLocale("en");
    }
  });
});

describe("Alerts and rules", () => {
  it("lists what fired, firing first, and each rule with where it sends", async () => {
    renderAt(
      "/alerts",
      api([ops], {
        alerts: {
          listAlerts: () => ({
            firing: 1,
            alerts: [
              {
                id: "alt_1",
                title: "web isn't running on hz-1",
                ruleName: "app-down",
                severity: Severity.CRITICAL,
                state: AlertState.FIRING,
                openTime: timestampFromDate(NOW),
                condition: Condition.SERVICE_DOWN,
                subjectId: "svc_web",
                subjectName: "web",
                serverId: "srv_1",
                serverName: "hz-1",
                variant: "unhealthy",
              },
              {
                id: "alt_2",
                title: "hz-1's disk / is 93% full",
                ruleName: "disk-full",
                severity: Severity.WARNING,
                state: AlertState.RESOLVED,
                resolveTime: timestampFromDate(NOW),
              },
            ],
          }),
        },
      }),
    );
    // What's open is a card, as Alerts draws it. Its title is put together in the panel, from what it's about.
    const open = await screen.findByRole("region", { name: "Open" });
    const card = within(open).getByRole("article");
    expect(within(card).getByRole("heading", { level: 3 })).toHaveTextContent("web fails its health check on hz-1");
    expect(card).toHaveTextContent("Critical");
    expect(card).toHaveTextContent("app-down");
    expect(within(card).getByRole("link", { name: /^Inspect: web fails/ })).toHaveAttribute("href", "/apps/svc_web");
    // What resolved is beside it, with the way to the rest.
    const resolved = screen.getByRole("complementary", { name: "Resolved" });
    expect(within(resolved).getByRole("listitem")).toHaveTextContent("disk-full");
    expect(within(resolved).getByRole("link", { name: "History" })).toHaveAttribute("href", "/activity");
    expect(screen.getByText(/1 open · 2 rules · sent to 1 channel/)).toBeVisible();
  });

  it("draws a server's memory beside the alert that's about it, most severe first, and says what was sent", async () => {
    const hz1 = aServer({ id: "srv_1", name: "hz-1", history: [sample(20, 7.2), sample(20, 7.4), sample(20, 7.5)] });
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    const opened = timestampFromDate(new Date(NOW.getTime() - 22 * 60_000));
    renderAt(
      "/alerts",
      api([ops], {
        servers: { watchServers: watch.stream },
        alerts: {
          listAlerts: () => ({
            firing: 2,
            alerts: [
              {
                id: "alt_mem",
                ruleName: "memory-high",
                severity: Severity.WARNING,
                state: AlertState.FIRING,
                condition: Condition.MEMORY_HIGH,
                serverId: "srv_1",
                serverName: "hz-1",
                value: 94,
                openTime: opened,
                muted: true,
              },
              {
                id: "alt_down",
                ruleName: "server-unreachable",
                severity: Severity.CRITICAL,
                state: AlertState.FIRING,
                condition: Condition.SERVER_UNREACHABLE,
                serverId: "srv_2",
                serverName: "hz-2",
                openTime: opened,
                sentTime: opened,
              },
            ],
          }),
        },
      }),
    );
    const open = await screen.findByRole("region", { name: "Open" });
    expect(within(open).getByText("Most severe first")).toBeVisible();
    const [first, second] = within(open).getAllByRole("article");
    expect(first).toHaveTextContent("hz-2 can't be reached");
    expect(first).toHaveTextContent(/Sent \d\d:\d\d/);
    expect(first).toHaveTextContent(/22 min since \d\d:\d\d/);
    expect(within(first as HTMLElement).queryByRole("img")).toBeNull();
    expect(second).toHaveTextContent("hz-1 uses 94% of its memory");
    expect(second).toHaveTextContent("Held by a mute");
    // 7.5 of 8 GiB, from the minutes the control plane keeps.
    expect(await within(second as HTMLElement).findByRole("img", { name: "Memory on hz-1, now 94%" })).toBeVisible();
    expect(within(second as HTMLElement).getByRole("link", { name: /^Inspect: hz-1 uses/ })).toHaveAttribute(
      "href",
      "/servers/srv_1",
    );
    // Nothing resolved, so nothing beside them says so.
    expect(screen.queryByRole("complementary", { name: "Resolved" })).toBeNull();
  });

  describe("acknowledging", () => {
    const anAlert = (more: object = {}) => ({
      id: "alt_1",
      ruleName: "server-unreachable",
      severity: Severity.CRITICAL,
      state: AlertState.FIRING,
      condition: Condition.SERVER_UNREACHABLE,
      serverId: "srv_2",
      serverName: "hz-2",
      openTime: timestampFromDate(new Date(NOW.getTime() - 20 * 60_000)),
      ...more,
    });
    const since = timestampFromDate(new Date(NOW.getTime() - 5 * 60_000));

    it("says nobody has an alert, and takes it as yours without a plan", async () => {
      const user = userEvent.setup();
      let alert = anAlert();
      const acknowledgeAlert = vi.fn((req: { id: string }) => {
        alert = anAlert({
          acknowledgeTime: since,
          acknowledgedBy: { kind: ActorKind.USER, id: mira.id, name: mira.name },
        });
        return { alert: { id: req.id } };
      });
      const unacknowledgeAlert = vi.fn((req: { id: string }) => {
        alert = anAlert();
        return { alert: { id: req.id } };
      });
      const proposed = planned();
      renderAt(
        "/alerts",
        api([ops], {
          alerts: { listAlerts: () => ({ firing: 1, alerts: [alert] }), acknowledgeAlert, unacknowledgeAlert },
          changes: { proposeChange: proposed },
        }),
      );
      const card = within(await screen.findByRole("region", { name: "Open" })).getByRole("article");
      expect(card).toHaveTextContent("Not acknowledged");
      await user.click(within(card).getByRole("button", { name: "Acknowledge: hz-2 can't be reached" }));
      await waitFor(() => expect(card).toHaveTextContent("Acknowledged by you · 5 min ago"));
      // Said to someone who can't see it change.
      expect(within(card).getByRole("status")).toHaveTextContent("Acknowledged by you · 5 min ago");
      expect(acknowledgeAlert.mock.calls[0]?.[0]).toMatchObject({ id: "alt_1" });
      // Yours is taken back, not taken again.
      expect(within(card).queryByRole("button", { name: /^Acknowledge/ })).toBeNull();
      await user.click(
        within(card).getByRole("button", { name: "Take back your acknowledgement of: hz-2 can't be reached" }),
      );
      await waitFor(() => expect(card).toHaveTextContent("Not acknowledged"));
      expect(unacknowledgeAlert.mock.calls[0]?.[0]).toMatchObject({ id: "alt_1" });
      // An audited action, as a mute is: nothing is planned.
      expect(proposed).not.toHaveBeenCalled();
    });

    it("names who has it, and offers to take it over", async () => {
      const user = userEvent.setup();
      const acknowledgeAlert = vi.fn((req: { id: string }) => ({ alert: { id: req.id } }));
      renderAt(
        "/alerts",
        api([ops], {
          alerts: {
            listAlerts: () => ({
              firing: 1,
              alerts: [
                anAlert({
                  acknowledgeTime: since,
                  acknowledgedBy: { kind: ActorKind.USER, id: "usr_leo", name: "Leo Park" },
                }),
              ],
            }),
            acknowledgeAlert,
          },
        }),
      );
      const card = within(await screen.findByRole("region", { name: "Open" })).getByRole("article");
      expect(card).toHaveTextContent("Acknowledged by Leo Park · 5 min ago");
      // A person has no mark: only what isn't one does.
      expect(card.querySelector(".alerts-card-by")).toBeNull();
      expect(within(card).queryByRole("button", { name: /^Take back/ })).toBeNull();
      await user.click(within(card).getByRole("button", { name: "Take over from Leo Park: hz-2 can't be reached" }));
      await waitFor(() => expect(acknowledgeAlert).toHaveBeenCalledOnce());
    });

    it("says what the control plane refused, on the card, and reads the alerts again", async () => {
      const user = userEvent.setup();
      const { ConnectError, Code } = await import("@connectrpc/connect");
      const listAlerts = vi.fn(() => ({ firing: 1, alerts: [anAlert()] }));
      renderAt(
        "/alerts",
        api([ops], {
          alerts: {
            listAlerts,
            acknowledgeAlert: () => {
              throw new ConnectError(
                "that alert resolved, so there's nothing left to be on",
                Code.FailedPrecondition,
                new Headers({ "Dockwell-Error": "alert_resolved" }),
              );
            },
          },
        }),
      );
      const card = within(await screen.findByRole("region", { name: "Open" })).getByRole("article");
      const before = listAlerts.mock.calls.length;
      await user.click(within(card).getByRole("button", { name: /^Acknowledge:/ }));
      expect(await within(card).findByText("That didn't take")).toBeVisible();
      expect(card).toHaveTextContent("That alert resolved, so there's nothing left to be on.");
      await waitFor(() => expect(listAlerts.mock.calls.length).toBeGreaterThan(before));
      // It stays through the re-read, to be read, until it's closed.
      expect(within(card).getByText("That didn't take")).toBeVisible();
      await user.click(within(card).getByRole("button", { name: "Close" }));
      expect(within(card).queryByText("That didn't take")).toBeNull();
    });

    it("clears a card's refusal when that card is asked again, and leaves another card's alone", async () => {
      const user = userEvent.setup();
      const { ConnectError, Code } = await import("@connectrpc/connect");
      let refuse = true;
      renderAt(
        "/alerts",
        api([ops], {
          alerts: {
            listAlerts: () => ({
              firing: 2,
              alerts: [anAlert(), anAlert({ id: "alt_2", serverId: "srv_3", serverName: "hz-3" })],
            }),
            acknowledgeAlert: (req: { id: string }) => {
              if (refuse) throw new ConnectError("someone else has that alert now", Code.FailedPrecondition);
              return { alert: { id: req.id } };
            },
          },
        }),
      );
      const [first, second] = within(await screen.findByRole("region", { name: "Open" })).getAllByRole("article");
      await user.click(within(first as HTMLElement).getByRole("button", { name: /^Acknowledge:/ }));
      await user.click(within(second as HTMLElement).getByRole("button", { name: /^Acknowledge:/ }));
      await within(first as HTMLElement).findByText("That didn't take");
      await within(second as HTMLElement).findByText("That didn't take");
      refuse = false;
      await user.click(within(second as HTMLElement).getByRole("button", { name: /^Acknowledge:/ }));
      await waitFor(() => expect(within(second as HTMLElement).queryByText("That didn't take")).toBeNull());
      expect(within(first as HTMLElement).getByText("That didn't take")).toBeVisible();
    });

    it("shows a card as busy while it's asked, and only that card", async () => {
      const user = userEvent.setup();
      let answer: () => void = () => {};
      const asked = new Promise<void>((resolve) => {
        answer = resolve;
      });
      renderAt(
        "/alerts",
        api([ops], {
          alerts: {
            listAlerts: () => ({
              firing: 2,
              alerts: [anAlert(), anAlert({ id: "alt_2", serverId: "srv_3", serverName: "hz-3" })],
            }),
            acknowledgeAlert: async (req: { id: string }) => {
              await asked;
              return { alert: { id: req.id } };
            },
          },
        }),
      );
      const [first, second] = within(await screen.findByRole("region", { name: "Open" })).getAllByRole("article");
      const button = (card: HTMLElement | undefined) =>
        within(card as HTMLElement).getByRole("button", { name: /^Acknowledge:/ });
      await user.click(button(first));
      await waitFor(() => expect(button(first)).toHaveAttribute("aria-busy", "true"));
      expect(button(second)).not.toHaveAttribute("aria-busy", "true");
      answer();
      await waitFor(() => expect(button(first)).not.toHaveAttribute("aria-busy", "true"));
    });

    it("names a token as a token, as Activity does, and never as you", async () => {
      const held = (acknowledgedBy: object) => anAlert({ acknowledgeTime: since, acknowledgedBy });
      renderAt(
        "/alerts",
        api([ops], {
          alerts: {
            listAlerts: () => ({
              firing: 3,
              alerts: [
                // A token called what a person is called: its mark says what it is.
                held({ kind: ActorKind.TOKEN, id: "tok_1", name: "Leo Park" }),
                // The CLI's own token on the control server, by where it was made, not by its name.
                {
                  ...held({
                    kind: ActorKind.TOKEN,
                    id: "tok_2",
                    name: "Local access on hz-1, read-write",
                    localAccess: { account: "mira@hz-1", scope: TokenScope.READ_WRITE },
                  }),
                  id: "alt_2",
                },
                // A token with your own ID, were there one: a token is never you.
                { ...held({ kind: ActorKind.TOKEN, id: mira.id, name: "Mira's deploy token" }), id: "alt_3" },
              ],
            }),
          },
        }),
      );
      const cards = within(await screen.findByRole("region", { name: "Open" })).getAllByRole("article");
      const [named, cli, own] = cards as HTMLElement[];
      expect(named).toHaveTextContent("Acknowledged by Leo Park · 5 min ago");
      expect(named?.querySelector(".alerts-card-by")).not.toBeNull();
      expect(cli).toHaveTextContent("Acknowledged by CLI on hz-1 · 5 min ago");
      expect(within(cli as HTMLElement).getByText("Acknowledged by CLI on hz-1")).toHaveAttribute(
        "title",
        "Local access token, read-write, tok_2, made by mira@hz-1",
      );
      expect(own).toHaveTextContent("Acknowledged by Mira's deploy token · 5 min ago");
      expect(own).not.toHaveTextContent("Acknowledged by you");
      // Not yours to take back, so it offers to take over.
      expect(
        within(own as HTMLElement).getByRole("button", { name: /^Take over from Mira's deploy token/ }),
      ).toBeVisible();
      expect(within(own as HTMLElement).queryByRole("button", { name: /^Take back/ })).toBeNull();
    });
  });

  it("says a role doesn't cover alerts, rules or channels as every page does, not as a load that failed", async () => {
    const { ConnectError, Code } = await import("@connectrpc/connect");
    const refuse = () => {
      throw new ConnectError(
        "this needs the admin role on the install",
        Code.PermissionDenied,
        new Headers({ "Dockwell-Error": "needs_role" }),
      );
    };
    const refused: Api = {
      alerts: { listAlerts: refuse, listAlertRules: refuse, listAlertChannels: refuse, listAlertDeliveries: refuse },
    };
    for (const [path, old] of [
      ["/alerts", "Can't load the alerts"],
      ["/alerts/rules", "Can't load the rules"],
      ["/alerts/channels", "Can't load the channels"],
    ] as const) {
      const page = renderAt(path, api([], refused));
      const why = (await screen.findByText("Your role doesn't cover this")).closest(".dw-callout");
      expect(why).toHaveClass("dw-tone-warning");
      expect(why).toHaveTextContent("Your role doesn't let you do this here. Ask an admin to change your role.");
      expect(screen.queryByText(old)).toBeNull();
      page.unmount();
    }
  });

  it("lists the rules", async () => {
    renderAt("/alerts/rules", api([ops]));
    const table = await screen.findByRole("table", { name: "Alert rules" });
    expect(within(table).getByRole("row", { name: /App down/ })).toHaveTextContent("Every channel");
    expect(within(table).getByRole("row", { name: /Disk above 90%/ })).toHaveTextContent("ops-email");
  });

  it("changes a rule from its row, as a plan, with only the values its condition takes", async () => {
    const user = userEvent.setup();
    const proposeChange = planned();
    renderAt("/alerts/rules", api([ops, phones], { changes: { proposeChange } }));
    const table = await screen.findByRole("table", { name: "Alert rules" });
    await user.click(within(table).getByRole("row", { name: /Disk above 90%/ }));
    const sheet = await screen.findByRole("dialog", { name: "Disk above 90%" });
    expect(within(sheet).queryByText(/dockwell alert-rules/)).toBeNull();
    await user.selectOptions(within(sheet).getByLabelText("When a disk is fuller than"), "80");
    await user.selectOptions(within(sheet).getByLabelText("For"), "10m");
    await user.click(within(sheet).getByRole("radio", { name: "Critical" }));
    await user.selectOptions(within(sheet).getByLabelText("While it lasts, say it again"), "24h");
    // It sends to ops-email alone; phones joins it.
    await user.click(within(sheet).getByRole("switch", { name: "phones" }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposeChange).toHaveBeenCalled());
    const edit = proposeChange.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "alert.rule", name: "disk-full" });
    const spec = edit?.spec?.kind.case === "alertRule" ? edit.spec.kind.value : undefined;
    expect(spec).toMatchObject({
      condition: Condition.DISK_FULL,
      threshold: 80,
      duration: "10m",
      severity: Severity.CRITICAL,
      repeat: "24h",
      channels: ["ops-email", "phones"],
      disabled: false,
    });
  });

  it("leaves a rule's Review plan off, with why, for someone who can't change the install's rules", async () => {
    const user = userEvent.setup();
    renderAt(
      "/alerts/rules",
      api([ops], {
        auth: {
          getAuthState: () =>
            authState({
              user: { ...mira, role: Role.UNSPECIFIED },
              grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
            }),
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Alert rules" });
    await user.click(within(table).getByRole("row", { name: /App down/ }));
    const sheet = await screen.findByRole("dialog", { name: "App down" });
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toHaveAttribute("aria-disabled", "true");
    expect(review).not.toHaveAccessibleDescription("");
  });

  it.each([
    ["memory", Condition.MEMORY_HIGH, "When memory in use is over", "90", { threshold: 90 }],
    ["certificate", Condition.CERTIFICATE_EXPIRING, "When it expires within", "14", { threshold: 14 }],
    ["late backup", Condition.BACKUP_LATE, "When a run is late by", "1h", { duration: "1h" }],
  ])("plans a %s rule with no level of its own at the default it shows", async (_, condition, label, shown, sent) => {
    const user = userEvent.setup();
    const proposeChange = planned();
    const bare = create(AlertRuleInfoSchema, { id: "alr_bare", name: "bare", title: "Bare rule", spec: { condition } });
    renderAt(
      "/alerts/rules",
      api([ops], { changes: { proposeChange }, alerts: { listAlertRules: () => ({ rules: [bare] }) } }),
    );
    const table = await screen.findByRole("table", { name: "Alert rules" });
    await user.click(within(table).getByRole("row", { name: /Bare rule/ }));
    const sheet = await screen.findByRole("dialog", { name: "Bare rule" });
    expect(within(sheet).getByLabelText(label)).toHaveValue(shown);
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposeChange).toHaveBeenCalled());
    const spec = proposeChange.mock.calls[0]?.[0].edits[0]?.spec?.kind;
    expect(spec?.case === "alertRule" ? spec.value : undefined).toMatchObject(sent);
  });

  it("turns a rule off, and asks nothing else of one that's off", async () => {
    const user = userEvent.setup();
    const proposeChange = planned();
    renderAt("/alerts/rules", api([ops], { changes: { proposeChange } }));
    const table = await screen.findByRole("table", { name: "Alert rules" });
    await user.click(within(table).getByRole("row", { name: /App down/ }));
    const sheet = await screen.findByRole("dialog", { name: "App down" });
    await user.click(within(sheet).getByRole("switch", { name: "Watch for this" }));
    expect(within(sheet).queryByLabelText("For")).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposeChange).toHaveBeenCalled());
    const spec = proposeChange.mock.calls[0]?.[0].edits[0]?.spec?.kind;
    expect(spec?.case === "alertRule" && spec.value.disabled).toBe(true);
  });

  it("asks a failed change's rule for no duration or repeat, as it fires once, as the change fails", async () => {
    const user = userEvent.setup();
    const failed = create(AlertRuleInfoSchema, {
      id: "alr_change",
      name: "change-failed",
      title: "A change failed",
      expression: "change.failed",
      spec: { condition: Condition.CHANGE_FAILED, severity: Severity.WARNING },
    });
    renderAt("/alerts/rules", api([ops], { alerts: { listAlertRules: () => ({ rules: [failed] }) } }));
    const table = await screen.findByRole("table", { name: "Alert rules" });
    await user.click(within(table).getByRole("row", { name: /A change failed/ }));
    const sheet = await screen.findByRole("dialog", { name: "A change failed" });
    expect(within(sheet).queryByLabelText("For")).toBeNull();
    expect(within(sheet).queryByLabelText("While it lasts, say it again")).toBeNull();
    expect(within(sheet).getByRole("radio", { name: "Warning" })).toBeChecked();
  });

  it("lists the alerts and the rules on a phone as rows, with no table to cut off", async () => {
    onPhone();
    const first = renderAt(
      "/alerts",
      api([ops], {
        alerts: {
          listAlerts: () => ({
            firing: 1,
            alerts: [
              {
                id: "alt_1",
                title: "web isn't running on hz-1",
                detail: "Its visitors get an error there.",
                ruleName: "app-down",
                severity: Severity.CRITICAL,
                state: AlertState.FIRING,
                openTime: timestampFromDate(NOW),
              },
            ],
          }),
        },
      }),
    );
    const open = await screen.findByRole("region", { name: "Open" });
    expect(screen.queryByRole("table")).toBeNull();
    const alert = within(open).getByRole("article");
    expect(alert).toHaveTextContent("Its visitors get an error there.");
    expect(alert).toHaveTextContent("Critical");
    expect(alert).toHaveTextContent("app-down");
    first.unmount();

    renderAt("/alerts/rules", api([ops]));
    const rules = await screen.findByRole("list", { name: "Alert rules" });
    expect(screen.queryByRole("table")).toBeNull();
    const down = within(rules).getByText("App down").closest("li") as HTMLElement;
    expect(down).toHaveTextContent("Critical");
    expect(down).toHaveTextContent("Every channel");
  });
});
