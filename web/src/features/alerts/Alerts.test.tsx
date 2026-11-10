// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type AlertChannelInfo,
  AlertChannelInfoSchema,
  AlertRuleInfoSchema,
  AlertState,
  ChangeSchema,
  ChangeState,
  AlertRule_Condition as Condition,
  type ProposeChangeRequest,
  AlertEmail_Security as Security,
  AlertRule_Severity as Severity,
  type TestAlertChannelRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
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

  it("adds an email channel: a test sends what the form holds, and the plan carries it", async () => {
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
    const send = within(sheet).getByRole("button", { name: "Send a test" });
    expect(send).toBeDisabled();
    await user.type(within(sheet).getByLabelText("Name"), "team-email");
    await user.type(within(sheet).getByLabelText("To"), "ops@example.com, oncall@example.com");
    await user.type(within(sheet).getByLabelText("From"), "alerts@example.com");
    await user.type(within(sheet).getByLabelText("SMTP server"), "smtp.example.net");
    await user.type(within(sheet).getByLabelText(/^User/), "alerts");
    expect(send).toBeDisabled();
    await user.type(within(sheet).getByLabelText(/^Password/), "s3cret");
    await user.click(send);
    expect(await within(sheet).findByText("The test arrived")).toBeVisible();
    expect(asked[0]?.name).toBe("");
    expect(asked[0]?.channel?.to).toMatchObject({
      case: "email",
      value: {
        host: "smtp.example.net",
        username: "alerts",
        password: "s3cret",
        to: ["ops@example.com", "oncall@example.com"],
      },
    });
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "alert.channel", name: "team-email" });
    expect(edit?.spec?.kind).toMatchObject({ case: "alertChannel", value: { to: { case: "email" } } });
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

  it("lists the rules", async () => {
    renderAt("/alerts/rules", api([ops]));
    const table = await screen.findByRole("table", { name: "Alert rules" });
    expect(within(table).getByRole("row", { name: /App down/ })).toHaveTextContent("Every channel");
    expect(within(table).getByRole("row", { name: /Disk above 90%/ })).toHaveTextContent("ops-email");
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
